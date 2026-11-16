package obs

import (
	"log/slog"
	"net/http"
	"time"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush passes flushes through (server-sent events).
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection (per-request write deadlines).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Middleware assigns a request ID (accepting a well-formed inbound X-Request-ID),
// recovers panics and writes one access-log line per request.
// Health probes are logged at debug level to keep logs quiet.
func Middleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			id = NewRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := WithRequestID(r.Context(), id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		defer func() {
			if p := recover(); p != nil {
				log.ErrorContext(ctx, "panic in handler", "panic", p, "path", r.URL.Path)
				rec.WriteHeader(http.StatusInternalServerError)
			}
			level := slog.LevelInfo
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				level = slog.LevelDebug
			}
			log.Log(ctx, level, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", float64(time.Since(start).Microseconds())/1000,
			)
		}()
		next.ServeHTTP(rec, r.WithContext(ctx))
	})
}

// validRequestID accepts 1-64 chars of [A-Za-z0-9._-], which also blocks log injection.
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
