package opensearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func server(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_cluster/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestPing(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		body    string
		wantErr bool
	}{
		{"green", 200, `{"status":"green"}`, false},
		{"yellow is ok", 200, `{"status":"yellow"}`, false},
		{"red fails", 200, `{"status":"red"}`, true},
		{"http error", 503, `{}`, true},
		{"garbage", 200, `not json`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server(t, tt.code, tt.body)
			err := New([]string{s.URL}, "", "").Ping(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPingFailsOverToSecondNode(t *testing.T) {
	good := server(t, 200, `{"status":"green"}`)
	if err := New([]string{"http://127.0.0.1:1", good.URL}, "", "").Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPingErrorDoesNotLeakPassword(t *testing.T) {
	err := New([]string{"http://user:hunter2@127.0.0.1:1"}, "", "").Ping(context.Background())
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("want error without password, got %v", err)
	}
}
