package rpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	MaxCommandBytes  = 64 << 10
	MaxResponseBytes = 1 << 20
)

var operationName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){1,3}$`)
var retryKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,96}$`)

type Request struct {
	ID   string          `json:"id"`
	Op   string          `json:"op"`
	Key  string          `json:"key,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type Response struct {
	ID    string          `json:"id"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error *Fault          `json:"error,omitempty"`
}

func Parse(raw []byte) (Request, error) {
	if len(raw) > MaxCommandBytes {
		return Request{}, Invalid("Command exceeds the size limit")
	}
	req, err := Decode[Request](raw)
	if err != nil {
		return req, err
	}
	if len(req.ID) == 0 || len(req.ID) > 96 || len(req.Op) > 64 || !operationName.MatchString(req.Op) {
		return req, Invalid("Expected a request ID and a valid operation name")
	}
	if req.Key != "" && !retryKey.MatchString(req.Key) {
		return req, Invalid("Invalid retry key")
	}
	if _, err := Decode[map[string]json.RawMessage](req.Data); err != nil {
		return req, err
	}
	return req, nil
}

func digest(req Request) ([32]byte, error) {
	d := json.NewDecoder(bytes.NewReader(req.Data))
	d.UseNumber()
	value := make(map[string]any)
	if len(req.Data) != 0 {
		if err := d.Decode(&value); err != nil {
			return [32]byte{}, err
		}
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append(append([]byte(req.Op), 0), canonical...)), nil
}

// Registry is populated once during startup and then shared by both transports.
type Registry struct {
	pool    *pgxpool.Pool
	methods map[string]Method
	log     *slog.Logger
}

func New(pool *pgxpool.Pool, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	registry := &Registry{pool: pool, methods: make(map[string]Method), log: logger}
	registry.Register("commands.status", Method{Access: SignedIn, Handle: readCommandStatus})
	return registry
}

func (r *Registry) Register(name string, m Method) {
	if !operationName.MatchString(name) || (m.Handle == nil) == (m.Prepare == nil) || (m.Dedup && (!m.Write || m.Access == Public)) {
		panic("invalid command registration: " + name)
	}
	if _, found := r.methods[name]; found {
		panic("duplicate command registration: " + name)
	}
	r.methods[name] = m
}

type Operation struct {
	Name          string `json:"name"`
	Access        string `json:"access"`
	Writes        bool   `json:"writes"`
	RetryKey      bool   `json:"requires_key"`
	WebSocketOnly bool   `json:"websocket_only,omitempty"`
}

func (r *Registry) Operations() []Operation {
	access := [...]string{"public", "signed_in", "member", "admin"}
	list := make([]Operation, 0, len(r.methods))
	for name, m := range r.methods {
		list = append(list, Operation{Name: name, Access: access[m.Access], Writes: m.Write, RetryKey: m.Dedup})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

func (r *Registry) Execute(ctx context.Context, actor Actor, ip string, req Request) (resp Response) {
	resp.ID = req.ID
	m, ok := r.methods[req.Op]
	if !ok {
		return r.Failed(req.ID, NotFound("Unknown operation"))
	}
	if m.Access != Public && actor.ID == 0 {
		return r.Failed(req.ID, Unauthenticated())
	}
	if m.Access == Member && !actor.Chat {
		return r.Failed(req.ID, Error("verification_required", "Complete account verification", http.StatusForbidden))
	}
	if m.Access == Admin && !actor.Admin {
		return r.Failed(req.ID, Denied("Administrator access required"))
	}
	if m.Dedup && req.Key == "" {
		return r.Failed(req.ID, Invalid("This operation requires a retry key"))
	}

	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	handle := m.Handle
	if m.Prepare != nil {
		run, release, err := m.Prepare(ctx, actor, ip, req.Data)
		if err != nil {
			return r.Failed(req.ID, err)
		}
		if release != nil {
			defer release()
		}
		handle = func(ctx context.Context, call Call, _ json.RawMessage) (any, error) {
			return run(ctx, call)
		}
	}
	var err error
	if m.Write {
		err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
			var fingerprint [32]byte
			if m.Dedup {
				fingerprint, err = digest(req)
				if err != nil {
					return Invalid("Invalid command data")
				}
				cached, err := reserve(ctx, tx, actor.ID, req.Key, fingerprint)
				if err != nil || cached != nil {
					resp.Data = cached
					return err
				}
			}
			events := pendingEvents{}
			value, err := handle(ctx, Call{DB: tx, Actor: actor, IP: ip, Emit: events.add, EmitChat: events.addChat}, req.Data)
			if err != nil {
				return err
			}
			resp.Data, err = encode(value)
			if err != nil {
				return err
			}
			if m.Dedup {
				if _, err := tx.Exec(ctx, `UPDATE command_keys SET result=$3 WHERE user_id=$1 AND key=$2`, actor.ID, req.Key, resp.Data); err != nil {
					return err
				}
			}
			return events.appendTo(ctx, tx)
		})
	} else {
		value, callErr := handle(ctx, Call{DB: r.pool, Actor: actor, IP: ip, Emit: func(string, []int64, any) error { return errors.New("read operation cannot emit durable events") }}, req.Data)
		err = callErr
		if err == nil {
			resp.Data, err = encode(value)
		}
	}
	if err != nil {
		failure := r.Failed(req.ID, err)
		if failure.Error.Status >= 500 {
			r.log.Error("command failed", "op", req.Op, "code", failure.Error.Code, "error", safeLogError(err))
		}
		return failure
	}
	return resp
}

func (r *Registry) Failed(id string, err error) Response {
	var fault *Fault
	if errors.As(err, &fault) {
		return Response{ID: id, Error: fault}
	}
	var pgerr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		fault = &Fault{"not_found", "Resource not found", http.StatusNotFound}
	case errors.As(err, &pgerr) && pgerr.Code == "23505":
		fault = &Fault{"conflict", "Resource already exists", http.StatusConflict}
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		fault = &Fault{"timeout", "Operation did not complete", http.StatusServiceUnavailable}
	case pgerr != nil && (pgerr.Code == "40P01" || pgerr.Code == "40001"):
		fault = &Fault{"retry_later", "Concurrent change. Retry with the same key", http.StatusServiceUnavailable}
	default:
		fault = &Fault{"internal", "Operation failed", http.StatusInternalServerError}
	}
	return Response{ID: id, Error: fault}
}

func safeLogError(err error) string {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		return "postgres " + pgerr.Code + " " + pgerr.ConstraintName
	}
	return fmt.Sprintf("%T", err)
}

func encode(value any) (json.RawMessage, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxResponseBytes {
		return nil, Error("response_limit", "Use a smaller page size", http.StatusRequestEntityTooLarge)
	}
	return payload, nil
}

func reserve(ctx context.Context, tx pgx.Tx, userID int64, key string, digest [32]byte) (json.RawMessage, error) {
	res, err := tx.Exec(ctx, `INSERT INTO command_keys(user_id,key,digest) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, userID, key, digest[:])
	if err != nil || res.RowsAffected() == 1 {
		return nil, err
	}
	var previous []byte
	var cached json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT digest,result FROM command_keys WHERE user_id=$1 AND key=$2 FOR SHARE`, userID, key).Scan(&previous, &cached); err != nil {
		return nil, err
	}
	if !bytes.Equal(previous, digest[:]) {
		return nil, Conflict("Retry key is already bound to another command")
	}
	if cached == nil {
		return nil, errors.New("incomplete command result")
	}
	return cached, nil
}
