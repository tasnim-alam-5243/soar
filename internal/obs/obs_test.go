package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testLogger(buf *bytes.Buffer) *slog.Logger { return NewLogger(buf, slog.LevelDebug, "test") }

func TestMiddlewareRequestID(t *testing.T) {
	var buf bytes.Buffer
	var seen string
	h := Middleware(testLogger(&buf), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	// generated
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if len(rec.Header().Get("X-Request-ID")) != 32 || seen != rec.Header().Get("X-Request-ID") {
		t.Fatalf("bad generated id: header=%q ctx=%q", rec.Header().Get("X-Request-ID"), seen)
	}

	// accepted
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") != "abc-123" {
		t.Fatal("valid inbound id was not kept")
	}

	// rejected (log injection attempt)
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-ID", "bad id\n{\"x\":1}")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Header().Get("X-Request-ID"), "bad") {
		t.Fatal("invalid inbound id was kept")
	}

	// every log line is valid JSON and carries request_id
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if m["request_id"] == nil || m["role"] != "test" {
			t.Fatalf("missing fields: %v", m)
		}
	}
}

func TestMiddlewareRecoversPanic(t *testing.T) {
	var buf bytes.Buffer
	h := Middleware(testLogger(&buf), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	var buf bytes.Buffer
	mux := http.NewServeMux()
	NewHealth(testLogger(&buf), time.Second,
		Check{"a", func(context.Context) error { return nil }},
		Check{"b", func(context.Context) error { return errors.New("secret detail") }},
	).Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("readyz = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret detail") {
		t.Fatal("error detail leaked to the response")
	}
	if !strings.Contains(buf.String(), "secret detail") {
		t.Fatal("error detail should be in the log")
	}
}

func TestHealthAllOK(t *testing.T) {
	var buf bytes.Buffer
	mux := http.NewServeMux()
	NewHealth(testLogger(&buf), time.Second, Check{"a", func(context.Context) error { return nil }}).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"a":"ok"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}
