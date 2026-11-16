package adapters

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
)

func selfSigned(t *testing.T) (string, string) {
	t.Helper()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "soar"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	return string(certPEM), string(keyPEM)
}

func TestGraphMailPoller(t *testing.T) {
	certPEM, keyPEM := selfSigned(t)
	var assertion, filter string
	tokens := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			_ = r.ParseForm()
			assertion = r.Form.Get("client_assertion")
			tokens++
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "graph-token", "expires_in": 3600})
		case strings.Contains(r.URL.Path, "/v1.0/users/phish@corp.example/mailFolders/inbox/messages"):
			if r.Header.Get("Authorization") != "Bearer graph-token" {
				w.WriteHeader(401)
				return
			}
			filter = r.URL.Query().Get("$filter")
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{
				map[string]any{"id": "AAMk1", "subject": "Invoice", "receivedDateTime": "2026-11-09T08:00:00Z"},
				map[string]any{"id": "AAMk2", "subject": "Password reset", "receivedDateTime": "2026-11-09T08:05:00Z"},
			}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	conn := domain.Connection{
		Name: "m365", ConnectorID: "outlook",
		Config:     map[string]any{"tenant_id": "contoso", "client_id": "app-1", "authority_url": srv.URL, "graph_url": srv.URL},
		SecretRefs: map[string]any{"certificate_pem": "cert", "private_key_pem": "key"},
	}
	resolve := func(ref string) (string, error) {
		if ref == "cert" {
			return certPEM, nil
		}
		return keyPEM, nil
	}
	p, err := NewGraphMailPoller(domain.Source{Name: "phish", Config: map[string]any{"mailbox": "phish@corp.example", "page_size": float64(2)}}, conn, resolve)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 9, 7, 59, 0, 0, time.UTC)
	page, err := p.Page(context.Background(), Query{From: start, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || !page.Full || !page.Last.Equal(time.Date(2026, 11, 9, 8, 5, 0, 0, time.UTC)) {
		t.Fatalf("page: %+v", page)
	}
	if filter != "receivedDateTime ge 2026-11-09T07:59:00Z" {
		t.Errorf("filter %q", filter)
	}
	if parts := strings.Split(assertion, "."); len(parts) != 3 {
		t.Fatalf("certificate credential must send a signed assertion, got %q", assertion)
	}
	if _, err := p.Page(context.Background(), Query{From: start, Now: time.Now()}); err != nil || tokens != 1 {
		t.Errorf("token must be cached: %d requests, %v", tokens, err)
	}
	if _, err := NewGraphMailPoller(domain.Source{Name: "x", Config: map[string]any{"mailbox": "a@b"}}, domain.Connection{Name: "c", Config: map[string]any{"tenant_id": "t", "client_id": "c"}}, resolve); err == nil {
		t.Error("a connection without credentials must be rejected")
	}
}
