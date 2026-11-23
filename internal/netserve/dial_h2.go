package netserve

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

const (
	h2StreamID   = 1
	h2DataSize   = 16 << 10
	h2WindowSize = 65535
	h2MaxWindow  = 1<<31 - 1
)

// h2WebSocket carries a single RFC 8441 stream. Go's public HTTP transport can
// reject :protocol before routing to its HTTP/2 implementation. The framer
// and HPACK codecs let this dialer send the pseudo-header directly over h2.
type h2WebSocket struct {
	conn   *tls.Conn
	input  *http2.Framer
	output *http2.Framer

	outMu sync.Mutex // serializes DATA and connection control frames
	mu    sync.Mutex
	ready *sync.Cond // signals queued data, send credit, or shutdown

	err         error
	readEnded   bool
	pending     bytes.Buffer
	recvWindow  int64
	recvCredit  int64
	connWindow  int64
	sendWindow  int64
	initialSend int64
}

func dialH2(ctx context.Context, u *url.URL, cfg *tls.Config, headers http.Header) (frameStream, error) {
	dialer := tls.Dialer{Config: cfg}
	conn, err := dialer.DialContext(ctx, "tcp", dialAddress(u))
	if err != nil {
		return frameStream{}, err
	}
	tlsConn := conn.(*tls.Conn)
	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		_ = tlsConn.NetConn().Close()
		return frameStream{}, errors.New("TLS peer did not negotiate h2")
	}
	c := &h2WebSocket{
		conn: tlsConn, input: http2.NewFramer(nil, conn), output: http2.NewFramer(conn, nil),
		recvWindow: h2WindowSize, connWindow: h2WindowSize, sendWindow: h2WindowSize, initialSend: h2WindowSize,
	}
	c.ready = sync.NewCond(&c.mu)
	c.input.SetMaxReadFrameSize(h2DataSize)
	c.input.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	c.input.MaxHeaderListSize = maxHeaderBytes

	if err := runIO(ctx, func() { c.fail(net.ErrClosed) }, func() error { return c.connect(u, headers) }); err != nil {
		c.fail(err)
		return frameStream{}, err
	}
	go c.receive()
	interrupt := func() { c.fail(net.ErrClosed) }
	return frameStream{
		reader: c, writer: c, endWrite: c.endWrite, interruptRead: interrupt, interruptWrite: interrupt,
		close: func() error { interrupt(); return nil },
	}, nil
}

func (c *h2WebSocket) connect(u *url.URL, headers http.Header) error {
	if _, err := io.WriteString(c.conn, http2.ClientPreface); err != nil {
		return err
	}
	if err := c.output.WriteSettings(
		http2.Setting{ID: http2.SettingEnablePush, Val: 0},
		http2.Setting{ID: http2.SettingMaxHeaderListSize, Val: maxHeaderBytes},
	); err != nil {
		return err
	}
	frame, err := c.input.ReadFrame()
	if err != nil {
		return err
	}
	settings, ok := frame.(*http2.SettingsFrame)
	if !ok || settings.IsAck() {
		return errors.New("expected initial HTTP/2 SETTINGS")
	}
	if enabled, _ := settings.Value(http2.SettingEnableConnectProtocol); enabled != 1 {
		return errors.New("peer did not enable HTTP/2 Extended CONNECT")
	}
	if err := c.settings(settings); err != nil {
		return err
	}
	if err := c.writeConnectHeaders(u, headers); err != nil {
		return err
	}
	for {
		frame, err := c.input.ReadFrame()
		if err != nil {
			return err
		}
		if response, ok := frame.(*http2.MetaHeadersFrame); ok {
			if response.StreamID != h2StreamID || response.Truncated || response.StreamEnded() {
				return errors.New("invalid HTTP/2 Extended CONNECT response")
			}
			status, err := strconv.Atoi(response.PseudoValue("status"))
			if err != nil {
				return errors.New("invalid HTTP/2 Extended CONNECT response status")
			}
			if status >= 100 && status < 200 {
				continue
			}
			h := make(http.Header)
			for _, field := range response.RegularFields() {
				h.Add(field.Name, field.Value)
			}
			return checkConnectResponse(status, h)
		}
		if _, ok := frame.(*http2.DataFrame); ok {
			return errors.New("unexpected HTTP/2 DATA before CONNECT response")
		}
		if err := c.process(frame); err != nil {
			return err
		}
	}
}

func (c *h2WebSocket) writeConnectHeaders(u *url.URL, headers http.Header) error {
	fields := []hpack.HeaderField{
		{Name: ":method", Value: "CONNECT"},
		{Name: ":protocol", Value: "websocket"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: u.Host},
		{Name: ":path", Value: u.RequestURI()},
	}
	for key, values := range headers {
		for _, value := range values {
			fields = append(fields, hpack.HeaderField{Name: strings.ToLower(key), Value: value, Sensitive: true})
		}
	}
	var size uint64
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	// Avoid using the peer's dynamic table. No encoder state spans requests.
	encoder.SetMaxDynamicTableSize(0)
	for _, field := range fields {
		size += 32 + uint64(len(field.Name)) + uint64(len(field.Value))
		if size > maxHeaderBytes {
			return errors.New("HTTP/2 Extended CONNECT headers too large")
		}
		if err := encoder.WriteField(field); err != nil {
			return err
		}
	}
	data := block.Bytes()
	first := min(len(data), h2DataSize)
	if err := c.output.WriteHeaders(http2.HeadersFrameParam{
		StreamID: h2StreamID, EndHeaders: first == len(data), BlockFragment: data[:first],
	}); err != nil {
		return err
	}
	for data = data[first:]; len(data) != 0; {
		n := min(len(data), h2DataSize)
		if err := c.output.WriteContinuation(h2StreamID, n == len(data), data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func (c *h2WebSocket) settings(frame *http2.SettingsFrame) error {
	if frame.IsAck() {
		return nil
	}
	if err := frame.ForeachSetting(func(s http2.Setting) error { return s.Valid() }); err != nil {
		return err
	}
	if val, ok := frame.Value(http2.SettingInitialWindowSize); ok {
		c.mu.Lock()
		c.sendWindow += int64(val) - c.initialSend
		c.initialSend = int64(val)
		window := c.sendWindow
		c.ready.Broadcast()
		c.mu.Unlock()
		if window > h2MaxWindow {
			return errors.New("HTTP/2 stream window overflow")
		}
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	return c.output.WriteSettingsAck()
}

func (c *h2WebSocket) receive() {
	for {
		frame, err := c.input.ReadFrame()
		if err == nil {
			err = c.process(frame)
		}
		if err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *h2WebSocket) process(frame http2.Frame) error {
	switch f := frame.(type) {
	case *http2.SettingsFrame:
		return c.settings(f)
	case *http2.WindowUpdateFrame:
		c.mu.Lock()
		defer c.mu.Unlock()
		switch f.StreamID {
		case 0:
			c.connWindow += int64(f.Increment)
		case h2StreamID:
			c.sendWindow += int64(f.Increment)
		default:
			return errors.New("HTTP/2 window update for unknown stream")
		}
		if c.connWindow > h2MaxWindow || c.sendWindow > h2MaxWindow {
			return errors.New("HTTP/2 send window overflow")
		}
		c.ready.Broadcast()
	case *http2.DataFrame:
		c.mu.Lock()
		if f.StreamID != h2StreamID || c.readEnded || int64(f.Length) > c.recvWindow {
			c.mu.Unlock()
			return errors.New("invalid HTTP/2 DATA on WebSocket stream")
		}
		c.recvWindow -= int64(f.Length)
		data := f.Data()
		_, _ = c.pending.Write(data)
		credit := c.collectCredit(int64(f.Length) - int64(len(data)))
		c.readEnded = f.StreamEnded()
		c.ready.Broadcast()
		c.mu.Unlock()
		return c.returnCredit(credit)
	case *http2.PingFrame:
		if !f.IsAck() {
			c.outMu.Lock()
			defer c.outMu.Unlock()
			return c.output.WritePing(true, f.Data)
		}
	case *http2.GoAwayFrame:
		if f.LastStreamID < h2StreamID || f.ErrCode != http2.ErrCodeNo {
			return fmt.Errorf("HTTP/2 GOAWAY: %s", f.ErrCode)
		}
	case *http2.RSTStreamFrame:
		return fmt.Errorf("HTTP/2 stream %d reset: %s", f.StreamID, f.ErrCode)
	case *http2.PushPromiseFrame, *http2.MetaHeadersFrame:
		return errors.New("unexpected HTTP/2 headers after Extended CONNECT")
	}
	return nil
}

func (c *h2WebSocket) collectCredit(n int64) uint32 {
	// Called with c.mu held. Retain at most one window of unread DATA.
	c.recvCredit += n
	if c.recvCredit < h2WindowSize/2 {
		return 0
	}
	credit := c.recvCredit
	c.recvCredit = 0
	c.recvWindow += credit
	return uint32(credit)
}

func (c *h2WebSocket) returnCredit(n uint32) error {
	if n == 0 {
		return nil
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if err := c.output.WriteWindowUpdate(0, n); err != nil {
		return err
	}
	return c.output.WriteWindowUpdate(h2StreamID, n)
}

func (c *h2WebSocket) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	for c.pending.Len() == 0 && !c.readEnded && c.err == nil {
		c.ready.Wait()
	}
	if c.pending.Len() == 0 {
		err := c.err
		if c.readEnded {
			err = io.EOF
		}
		c.mu.Unlock()
		return 0, err
	}
	n, _ := c.pending.Read(p)
	credit := c.collectCredit(int64(n))
	c.mu.Unlock()
	// Yield already received bytes even if the peer has closed its write side.
	if err := c.returnCredit(credit); err != nil {
		c.fail(err)
	}
	return n, nil
}

func (c *h2WebSocket) Write(p []byte) (int, error) {
	var written int
	for len(p) > 0 {
		c.mu.Lock()
		for min(c.connWindow, c.sendWindow) <= 0 && c.err == nil {
			c.ready.Wait()
		}
		if c.err != nil {
			err := c.err
			c.mu.Unlock()
			return written, err
		}
		n := min(int64(len(p)), h2DataSize, c.connWindow, c.sendWindow)
		c.connWindow -= n
		c.sendWindow -= n
		c.mu.Unlock()
		c.outMu.Lock()
		err := c.output.WriteData(h2StreamID, false, p[:n])
		c.outMu.Unlock()
		if err != nil {
			c.fail(err)
			return written, err
		}
		p = p[n:]
		written += int(n)
	}
	return written, nil
}

func (c *h2WebSocket) endWrite() error {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	return c.output.WriteData(h2StreamID, true, nil)
}

func (c *h2WebSocket) fail(err error) {
	c.mu.Lock()
	first := c.err == nil
	if first {
		c.err = err
		c.ready.Broadcast()
	}
	c.mu.Unlock()
	if first {
		_ = c.conn.NetConn().Close()
	}
}
