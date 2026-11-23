package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the subset shared by a pool and a transaction.
type DB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Actor struct {
	ID      int64
	Session []byte // Hash of the active session token.
	Admin   bool
	Chat    bool
}

type Authenticator interface {
	Authenticate(context.Context, string) (Actor, error)
}

type Access uint8

const (
	Public Access = iota
	SignedIn
	Member
	Admin
)

type Call struct {
	DB    DB
	Actor Actor
	IP    string
	Emit  func(kind string, recipients []int64, data any) error
	// EmitChat applies conversation visibility at delivery and replay. A nonzero
	// message sequence also suppresses the event after that message is recalled.
	EmitChat func(conversationID, messageSeq int64, kind string, recipients []int64, data any) error
}

type Handler func(context.Context, Call, json.RawMessage) (any, error)
type PreparedHandler func(context.Context, Call) (any, error)

// Prepare admits a command and decodes its input before the registry gets a
// database connection. Any release function runs after the command completes.
type Prepare func(ctx context.Context, actor Actor, address string, data json.RawMessage) (run PreparedHandler, release func(), err error)

type Method struct {
	Access  Access
	Write   bool
	Dedup   bool
	Handle  Handler
	Prepare Prepare
}

type Fault struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (f *Fault) Error() string { return f.Message }

func Error(code, message string, status int) error { return &Fault{code, message, status} }
func Invalid(message string) error                 { return Error("invalid", message, http.StatusBadRequest) }
func NotFound(message string) error                { return Error("not_found", message, http.StatusNotFound) }
func Denied(message string) error                  { return Error("forbidden", message, http.StatusForbidden) }
func Conflict(message string) error                { return Error("conflict", message, http.StatusConflict) }
func RateLimited(message string) error {
	return Error("rate_limited", message, http.StatusTooManyRequests)
}
func Unauthenticated() error {
	return Error("unauthenticated", "Authentication required", http.StatusUnauthorized)
}

func Decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return v, Invalid("data must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return v, Invalid(fmt.Sprintf("Invalid data: %v", err))
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return v, Invalid("Expected one JSON object")
	}
	return v, nil
}
