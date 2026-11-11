// Package obs holds logging, HTTP middleware and health endpoints shared by every role.
package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
)

type (
	attrsKey     struct{}
	requestIDKey struct{}
)

// NewLogger returns a JSON logger. Attributes attached to the context with
// WithAttrs (request_id, execution_id, node_id, trace_id, ...) appear on every record.
func NewLogger(w io.Writer, level slog.Level, role string) *slog.Logger {
	h := ctxHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})}
	return slog.New(h).With("role", role, "version", Version)
}

// WithAttrs returns a context whose log records carry attrs.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	prev, _ := ctx.Value(attrsKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(prev)+len(attrs))
	merged = append(merged, prev...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, attrsKey{}, merged)
}

// WithRequestID stores the request ID and tags subsequent log records with it.
func WithRequestID(ctx context.Context, id string) context.Context {
	ctx = context.WithValue(ctx, requestIDKey{}, id)
	return WithAttrs(ctx, slog.String("request_id", id))
}

// RequestID returns the request ID in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// NewRequestID returns 128 random bits as hex.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type ctxHandler struct{ slog.Handler }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(attrsKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h ctxHandler) WithAttrs(a []slog.Attr) slog.Handler { return ctxHandler{h.Handler.WithAttrs(a)} }

func (h ctxHandler) WithGroup(n string) slog.Handler { return ctxHandler{h.Handler.WithGroup(n)} }
