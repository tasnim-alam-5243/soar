package opensearch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDo(t *testing.T) {
	var gotBody, gotCT, gotAuth string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotCT = string(b), r.Header.Get("Content-Type")
		if u, p, ok := r.BasicAuth(); ok {
			gotAuth = u + ":" + p
		}
		switch r.URL.Path {
		case "/missing":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		case "/boom":
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer s.Close()
	c := New([]string{s.URL}, "svc", "pw")
	ctx := context.Background()

	st, out, err := c.Do(ctx, http.MethodPut, "/x", map[string]any{"a": 1})
	if err != nil || st != 200 || !strings.Contains(string(out), "ok") {
		t.Fatalf("PUT: %d %s %v", st, out, err)
	}
	if gotBody != `{"a":1}` || gotCT != "application/json" || gotAuth != "svc:pw" {
		t.Fatalf("request not as expected: %q %q %q", gotBody, gotCT, gotAuth)
	}

	if st, _, err = c.Do(ctx, http.MethodGet, "/missing", nil, 404); err != nil || st != 404 {
		t.Fatalf("allowed 404 must not be an error: %d %v", st, err)
	}
	_, _, err = c.Do(ctx, http.MethodGet, "/missing", nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("want APIError 404, got %v", err)
	}
	if _, _, err = c.Do(ctx, http.MethodGet, "/boom", nil, 404); !errors.As(err, &ae) || ae.Status != 500 {
		t.Fatalf("500 must be an error even when 404 is allowed: %v", err)
	}
}

func TestDoFailsOverOnConnectionErrorOnly(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer good.Close()

	// unreachable first, healthy second: succeeds
	if _, _, err := New([]string{"http://127.0.0.1:1", good.URL}, "", "").Do(context.Background(), http.MethodGet, "/", nil); err != nil {
		t.Fatal(err)
	}
	// an HTTP 500 is an answer, not a reason to ask the next node
	_, _, err := New([]string{bad.URL, good.URL}, "", "").Do(context.Background(), http.MethodGet, "/", nil)
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want APIError, got %v", err)
	}
}

func TestDoErrorDoesNotLeakPassword(t *testing.T) {
	_, _, err := New([]string{"http://user:hunter2@127.0.0.1:1"}, "", "").Do(context.Background(), http.MethodGet, "/", nil)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("want error without password, got %v", err)
	}
}
