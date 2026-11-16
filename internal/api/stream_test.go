package api

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/store/memory"
)

// The UI keeps one /stream open per tab. The server's WriteTimeout must not cut it: the
// handler gives each write its own deadline instead. Before that, every stream dropped
// after 30 s and events in the reconnect gap were lost.
func TestStreamOutlivesTheServerWriteTimeout(t *testing.T) {
	oldKeep, oldWindow := streamKeepAlive, streamWriteWindow
	streamKeepAlive, streamWriteWindow = 40*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { streamKeepAlive, streamWriteWindow = oldKeep, oldWindow })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(memory.NewStore("soar"), log)
	mux := http.NewServeMux()
	s.Mount(mux)
	srv := httptest.NewUnstartedServer(obs.Middleware(log, mux)) // the same wrappers as production
	srv.Config.WriteTimeout = 150 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+Prefix+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	start := time.Now()
	sc := bufio.NewScanner(resp.Body)
	keepAlives := 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), ": keep-alive") {
			keepAlives++
		}
		if time.Since(start) > 600*time.Millisecond {
			break
		}
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("stream closed after %v (%d keep-alives, err %v); WriteTimeout is 150ms", elapsed, keepAlives, sc.Err())
	}
	if keepAlives < 5 {
		t.Errorf("want steady keep-alives past the write timeout, got %d", keepAlives)
	}
}
