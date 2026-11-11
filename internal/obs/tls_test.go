package obs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/tlstest"
)

// HTTPS with client certificates: every path but the probes needs a verified certificate.
func TestServeTLSRequiresClientCertificates(t *testing.T) {
	f := tlstest.New(t)
	pair, err := tls.LoadX509KeyPair(f.ServerCert, f.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(f.CA)
	pool.AppendCertsFromPEM(caPEM)
	srvTLS := &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.VerifyClientCertIfGiven, MinVersion: tls.VersionTLS12}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	go func() { done <- ServeTLS(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), addr, mux, srvTLS, true) }()
	t.Cleanup(func() { cancel(); <-done })

	get := func(path string, withCert bool) int {
		tc := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		if withCert {
			c, err := tls.LoadX509KeyPair(f.ClientCert, f.ClientKey)
			if err != nil {
				t.Fatal(err)
			}
			tc.Certificates = []tls.Certificate{c}
		}
		cl := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 5 * time.Second}
		var resp *http.Response
		for i := 0; i < 50; i++ { // the server goroutine may not listen yet
			if resp, err = cl.Get("https://" + addr + path); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("/api/v1/cases", false); c != http.StatusUnauthorized {
		t.Errorf("no client certificate: %d, want 401", c)
	}
	if c := get("/api/v1/cases", true); c != http.StatusOK {
		t.Errorf("with a client certificate: %d, want 200", c)
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		if c := get(p, false); c != http.StatusOK {
			t.Errorf("%s must stay open for container probes: %d", p, c)
		}
	}
	// plain HTTP is not served on a TLS port
	if resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr + "/healthz"); err == nil && resp.StatusCode == http.StatusOK {
		t.Error("a plain HTTP request was answered on the TLS port")
	}
}
