package netserve

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gobwas/ws"
)

const (
	// Subprotocol is required on all three WebSocket handshakes.
	Subprotocol = "csac.v2"
	// DefaultMaxMessageBytes caps a text message across all its fragments.
	DefaultMaxMessageBytes = 1 << 20
	// ResponseEnvelopeBytes is the reserved wire overhead above the RPC response payload limit.
	ResponseEnvelopeBytes = 1024
	defaultWriteTimeout   = 5 * time.Second
	closeTimeout          = time.Second
	maxMessageFragments   = 1024
)

// ErrMessageTooLarge means a frame or reassembled message exceeded a limit.
var ErrMessageTooLarge = errors.New("netserve: WebSocket message exceeds limit")

// CloseError reports the status and reason sent by the remote endpoint.
// Code 1005 denotes a close frame without a status code.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("netserve: WebSocket peer closed (%d): %s", e.Code, e.Reason)
}

type frameStream struct {
	reader         io.Reader
	writer         io.Writer
	flush          func() error
	endWrite       func() error
	interruptRead  func()
	interruptWrite func()
	close          func() error
}

// Socket exchanges UTF-8 text messages. Reads and writes are separately
// serialized and may run concurrently. Close may run during either operation.
type Socket struct {
	reader          *bufio.Reader
	wire            frameStream
	client          bool
	maxReadMessage  int
	maxWriteMessage int
	writeTimeout    time.Duration
	onClose         func()

	readSlot, writeSlot chan struct{}
	closing             chan struct{}
	closed              chan struct{}
	closeOnce           sync.Once
	closeErr            error
	readErr             error // guarded by readSlot
	writeBad            bool  // guarded by writeSlot
}

func newSocket(wire frameStream, client bool, maxReadMessage, maxWriteMessage int, writeTimeout time.Duration) *Socket {
	return &Socket{
		reader:          bufio.NewReader(wire.reader),
		wire:            wire,
		client:          client,
		maxReadMessage:  maxReadMessage,
		maxWriteMessage: maxWriteMessage,
		writeTimeout:    writeTimeout,
		readSlot:        make(chan struct{}, 1),
		writeSlot:       make(chan struct{}, 1),
		closing:         make(chan struct{}),
		closed:          make(chan struct{}),
	}
}

// Read returns one complete text message and answers incoming ping and close
// frames. Keep a reader running to service control frames. Cancellation during
// frame I/O ends further reads. Call Close to send a final status if possible.
func (s *Socket) Read(ctx context.Context) ([]byte, error) {
	if err := s.acquire(ctx, s.readSlot); err != nil {
		return nil, err
	}
	if s.readErr != nil {
		err := s.readErr
		<-s.readSlot
		return nil, err
	}
	var message []byte
	var reply closeReply
	err := runIO(ctx, s.wire.interruptRead, func() (err error) {
		message, reply, err = s.readMessage(ctx)
		return err
	})
	canceled := err != nil && ctx.Err() == err
	if canceled {
		s.readErr = err
	}
	finish := err != nil && !canceled
	ownsClose := finish && s.beginClose()
	<-s.readSlot
	if finish {
		if ownsClose {
			s.completeClose(reply)
		} else {
			<-s.closed
		}
	}
	if err != nil {
		return nil, err
	}
	return message, nil
}

// Write sends one text message. Output is bounded by ctx and the socket write
// timeout. An error after starting frame I/O ends the socket.
func (s *Socket) Write(ctx context.Context, payload []byte) error {
	if len(payload) > s.maxWriteMessage {
		return ErrMessageTooLarge
	}
	if !utf8.Valid(payload) {
		return errors.New("netserve: invalid UTF-8 WebSocket message")
	}
	return s.send(ctx, ws.OpText, payload)
}

// Ping sends a control frame. The payload may contain up to 125 bytes.
func (s *Socket) Ping(ctx context.Context, payload []byte) error {
	if len(payload) > ws.MaxControlFramePayloadSize {
		return errors.New("netserve: WebSocket ping exceeds 125 bytes")
	}
	return s.send(ctx, ws.OpPing, payload)
}

func (s *Socket) send(ctx context.Context, op ws.OpCode, payload []byte) error {
	attempted, err := s.sendFrame(ctx, op, payload)
	if attempted && err != nil {
		_ = s.finish(closeReply{})
	}
	return err
}

func (s *Socket) sendFrame(ctx context.Context, op ws.OpCode, payload []byte) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.writeTimeout)
	defer cancel()
	if err := s.acquire(ctx, s.writeSlot); err != nil {
		return false, err
	}
	defer func() { <-s.writeSlot }()
	if s.writeBad {
		return false, net.ErrClosed
	}
	err := runIO(ctx, s.wire.interruptWrite, func() error { return s.writeFrame(op, payload) })
	s.writeBad = err != nil
	return true, err
}

func (s *Socket) acquire(ctx context.Context, slot chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.closing:
		return net.ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	case slot <- struct{}{}:
		select {
		case <-s.closing:
			<-slot
			return net.ErrClosed
		default:
			return nil
		}
	}
}

// runIO interrupts the transport on cancellation. It waits for a running
// interrupter before releasing the I/O slot or returning a ResponseWriter.
func runIO(ctx context.Context, interrupt func(), operation func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ctx.Done() == nil {
		return operation()
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		interrupt()
		close(done)
	})
	err := operation()
	if !stop() {
		<-done
		return ctx.Err()
	}
	return err
}

func (s *Socket) writeFrame(op ws.OpCode, payload []byte) error {
	frame := ws.NewFrame(op, true, payload)
	if s.client {
		var mask [4]byte
		_, _ = rand.Read(mask[:]) // crypto/rand.Read fills the buffer or fails irrecoverably.
		frame.Payload = slices.Clone(payload)
		frame = ws.MaskFrameInPlaceWith(frame, mask)
	}
	if err := ws.WriteFrame(s.wire.writer, frame); err != nil {
		return err
	}
	if s.wire.flush != nil {
		return s.wire.flush()
	}
	return nil
}

type closeReply struct {
	send    bool
	payload []byte
}

func statusReply(code ws.StatusCode) closeReply {
	return closeReply{true, ws.NewCloseFrameBody(code, "")}
}

func (s *Socket) readMessage(ctx context.Context) ([]byte, closeReply, error) {
	var data []byte
	var frames int
	var scratch [ws.MaxHeaderSize - 2]byte
	state := ws.StateServerSide
	if s.client {
		state = ws.StateClientSide
	}
	for {
		prefix, err := s.reader.Peek(2)
		if err != nil {
			return nil, closeReply{}, err
		}
		encodedLength := prefix[1] & 0x7f
		h, err := ws.ReadHeaderBuffer(s.reader, scratch[:])
		if err != nil {
			if errors.Is(err, ws.ErrHeaderLengthMSB) {
				return nil, statusReply(ws.StatusProtocolError), err
			}
			return nil, closeReply{}, err
		}
		if err := ws.CheckHeader(h, state); err != nil {
			return nil, statusReply(ws.StatusProtocolError), err
		}
		if (encodedLength == 126 && h.Length < 126) || (encodedLength == 127 && h.Length < 65536) {
			return nil, statusReply(ws.StatusProtocolError), errors.New("netserve: non-minimal WebSocket frame length")
		}
		if h.OpCode.IsControl() {
			var control [ws.MaxControlFramePayloadSize]byte
			payload := control[:int(h.Length)]
			if err := readPayload(s.reader, h, payload); err != nil {
				return nil, closeReply{}, err
			}
			switch h.OpCode {
			case ws.OpPing:
				if _, err := s.sendFrame(ctx, ws.OpPong, payload); err != nil {
					return nil, closeReply{}, err
				}
			case ws.OpClose:
				if len(payload) == 0 {
					return nil, closeReply{send: true}, &CloseError{Code: 1005}
				}
				code, reason := ws.ParseCloseFrameData(payload)
				if len(payload) == 1 || !validCloseCode(int(code)) {
					return nil, statusReply(ws.StatusProtocolError), errors.New("netserve: invalid WebSocket close status")
				}
				if !utf8.ValidString(reason) {
					return nil, statusReply(ws.StatusInvalidFramePayloadData), errors.New("netserve: invalid UTF-8 WebSocket close reason")
				}
				return nil, closeReply{true, payload}, &CloseError{Code: int(code), Reason: reason}
			}
			continue
		}
		if h.OpCode == ws.OpBinary {
			return nil, statusReply(ws.StatusUnsupportedData), errors.New("netserve: WebSocket binary messages are unsupported")
		}
		frames++
		if frames > maxMessageFragments || h.Length > int64(s.maxReadMessage-len(data)) {
			return nil, statusReply(ws.StatusMessageTooBig), ErrMessageTooLarge
		}
		previous := len(data)
		data = slices.Grow(data, int(h.Length))[:previous+int(h.Length)]
		if err := readPayload(s.reader, h, data[previous:]); err != nil {
			return nil, closeReply{}, err
		}
		if h.Fin {
			if !utf8.Valid(data) {
				return nil, statusReply(ws.StatusInvalidFramePayloadData), errors.New("netserve: invalid UTF-8 WebSocket message")
			}
			return data, closeReply{}, nil
		}
		state |= ws.StateFragmented
	}
}

func readPayload(r io.Reader, h ws.Header, payload []byte) error {
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	if h.Masked {
		ws.Cipher(payload, h.Mask, 0)
	}
	return nil
}

func validCloseCode(code int) bool {
	return code >= 1000 && code <= 1014 && code != 1004 && code != 1005 && code != 1006 || code >= 3000 && code <= 4999
}

// Close sends a close frame if the peer can accept it and ends this socket.
// It is safe to kick a peer while another goroutine is reading or writing.
func (s *Socket) Close(code int, reason string) error {
	if !validCloseCode(code) || len(reason) > 123 || !utf8.ValidString(reason) {
		return errors.New("netserve: invalid WebSocket close code or reason")
	}
	return s.finish(closeReply{true, ws.NewCloseFrameBody(ws.StatusCode(code), reason)})
}

func (s *Socket) beginClose() bool {
	started := false
	s.closeOnce.Do(func() {
		close(s.closing)
		started = true
	})
	return started
}

func (s *Socket) finish(reply closeReply) error {
	if s.beginClose() {
		s.completeClose(reply)
	} else {
		<-s.closed
	}
	return s.closeErr
}

func (s *Socket) completeClose(reply closeReply) {
	defer close(s.closed)
	if !reply.send {
		waitSlot(s.writeSlot, s.wire.interruptWrite)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), min(closeTimeout, s.writeTimeout))
		defer cancel()
		select {
		case s.writeSlot <- struct{}{}:
			if !s.writeBad {
				s.closeErr = runIO(ctx, s.wire.interruptWrite, func() error {
					if err := s.writeFrame(ws.OpClose, reply.payload); err != nil {
						return err
					}
					if s.wire.endWrite != nil {
						return s.wire.endWrite()
					}
					return nil
				})
			}
			<-s.writeSlot
		case <-ctx.Done():
			s.closeErr = ctx.Err()
			waitSlot(s.writeSlot, s.wire.interruptWrite)
		}
	}
	waitSlot(s.readSlot, s.wire.interruptRead)
	if s.wire.close != nil {
		s.closeErr = errors.Join(s.closeErr, s.wire.close())
	}
	if s.onClose != nil {
		s.onClose()
	}
}

func waitSlot(slot chan struct{}, interrupt func()) {
	select {
	case slot <- struct{}{}:
	default:
		interrupt()
		slot <- struct{}{}
	}
	<-slot
}

var expiredDeadline = time.Unix(1, 0)

func tcpStream(conn net.Conn, reader io.Reader) frameStream {
	closeConn := conn.Close
	if encrypted, ok := conn.(*tls.Conn); ok {
		// The WebSocket close frame has been written. Closing the TCP socket
		// directly avoids an additional, potentially blocking TLS alert.
		closeConn = encrypted.NetConn().Close
	}
	return frameStream{
		reader: reader, writer: conn,
		interruptRead:  func() { _ = conn.SetReadDeadline(expiredDeadline) },
		interruptWrite: func() { _ = conn.SetWriteDeadline(expiredDeadline) },
		close:          closeConn,
	}
}
