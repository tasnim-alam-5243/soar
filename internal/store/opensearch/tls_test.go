package opensearch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/tlstest"
)

// A cluster behind a private CA that also demands a client certificate (mTLS).
func TestClientTrustsAPrivateCAAndPresentsAClientCertificate(t *testing.T) {
	f := tlstest.New(t)
	pair, err := tls.LoadX509KeyPair(f.ServerCert, f.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(f.CA)
	pool.AppendCertsFromPEM(caPEM)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"cluster_name":"test","version":{"number":"2.19.1"}}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	ctx := context.Background()
	if err := New([]string{srv.URL}, "", "").Ping(ctx); err == nil {
		t.Error("without the CA the client must refuse the server")
	}
	caOnly, err := config.Load(func(k string) (string, bool) {
		v, ok := map[string]string{"SOAR_OS_URLS": srv.URL, "SOAR_NATS_URL": "nats://n:4222", "SOAR_OS_CA_FILE": f.CA}[k]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Open(caOnly).Ping(ctx); err == nil {
		t.Error("without a client certificate the server must refuse the client")
	}
	full, err := config.Load(func(k string) (string, bool) {
		v, ok := map[string]string{
			"SOAR_OS_URLS": srv.URL, "SOAR_NATS_URL": "nats://n:4222", "SOAR_OS_CA_FILE": f.CA,
			"SOAR_OS_CERT_FILE": f.ClientCert, "SOAR_OS_KEY_FILE": f.ClientKey,
		}[k]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Open(full).Ping(ctx); err != nil {
		t.Errorf("CA + client certificate: %v", err)
	}
}
