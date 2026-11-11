package main

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourorg/soar/internal/tlstest"
)

// With SOAR_TLS_CERT_FILE the probe keeps its http:// URL, talks HTTPS and accepts
// only the role's own certificate.
func TestHealthcheckSwitchesToHTTPSAndPinsTheRoleCertificate(t *testing.T) {
	f := tlstest.New(t)
	pair, err := tls.LoadX509KeyPair(f.ServerCert, f.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	plainURL := strings.Replace(srv.URL, "https://", "http://", 1) + "/readyz"

	t.Setenv("SOAR_TLS_CERT_FILE", f.ServerCert)
	if code := healthcheck([]string{plainURL}, io.Discard, io.Discard); code != 0 {
		t.Errorf("own certificate: exit %d, want 0", code)
	}
	t.Setenv("SOAR_TLS_CERT_FILE", f.ClientCert) // a different certificate: the pin must not match
	var stderr strings.Builder
	if code := healthcheck([]string{plainURL}, io.Discard, &stderr); code == 0 || !strings.Contains(stderr.String(), "not the one in SOAR_TLS_CERT_FILE") {
		t.Errorf("foreign certificate: exit %d, stderr %q", code, stderr.String())
	}
}
