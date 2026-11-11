package security

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store/memory"
)

func key(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

func TestEnvelopeEncryptionAndRotation(t *testing.T) {
	ctx := context.Background()
	st := memory.NewStore("soar")
	k1, _ := NewKeyring("k1", key(1), nil)
	ss := &SecretStore{Repo: st.Secrets, Keys: k1}
	meta, err := ss.Put(ctx, "iris_token", "s3cr3t", "iris")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Ciphertext != "" || meta.WrappedKey != "" || meta.KeyID != "k1" {
		t.Fatalf("metadata leaks material: %+v", meta)
	}
	raw, _, _ := st.Secrets.Get(ctx, SecretID("iris_token"))
	if strings.Contains(raw.Ciphertext, "s3cr3t") || raw.WrappedKey == "" {
		t.Fatal("not encrypted")
	}
	if v, err := ss.Resolve(ctx, "iris_token"); err != nil || v != "s3cr3t" {
		t.Fatalf("resolve: %q %v", v, err)
	}

	// rotate: k2 becomes active, k1 stays for decryption until every secret is re-wrapped
	k2, _ := NewKeyring("k2", key(2), map[string][]byte{"k1": key(1)})
	ss2 := &SecretStore{Repo: st.Secrets, Keys: k2}
	n, err := ss2.RotateAll(ctx)
	if err != nil || n != 1 {
		t.Fatalf("rotate: %d %v", n, err)
	}
	only2, _ := NewKeyring("k2", key(2), nil)
	if v, err := (&SecretStore{Repo: st.Secrets, Keys: only2}).Resolve(ctx, "iris_token"); err != nil || v != "s3cr3t" {
		t.Fatalf("after rotation the old key must not be needed: %q %v", v, err)
	}
	if _, err := (&SecretStore{Repo: st.Secrets, Keys: k1}).Resolve(ctx, "iris_token"); err == nil {
		t.Fatal("decrypting with a missing key must fail")
	}

	// tampering is detected (the name is bound as associated data)
	raw, _, _ = st.Secrets.Get(ctx, SecretID("iris_token"))
	moved := raw
	moved.Name = "other"
	if _, err := only2.Decrypt(moved); err == nil {
		t.Fatal("a secret copied under another name must not decrypt")
	}
	b, _ := base64.StdEncoding.DecodeString(raw.Ciphertext)
	b[0] ^= 1
	raw.Ciphertext = base64.StdEncoding.EncodeToString(b)
	if _, err := only2.Decrypt(raw); err == nil {
		t.Fatal("modified ciphertext must fail authentication")
	}

	t.Setenv("SOAR_TEST_SECRET", "from-env")
	if v, _ := ss.Resolve(ctx, "env:SOAR_TEST_SECRET"); v != "from-env" {
		t.Fatal("env references must keep working")
	}
	if _, err := ss.Put(ctx, "9bad", "x", ""); err == nil {
		t.Fatal("invalid name accepted")
	}
}

// ---- OIDC ----

type idp struct {
	srv  *httptest.Server
	rsa  *rsa.PrivateKey
	ec   *ecdsa.PrivateKey
	hits int
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	p := &idp{}
	p.rsa, _ = rsa.GenerateKey(rand.Reader, 2048)
	p.ec, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/soar/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jwks_uri": p.srv.URL + "/realms/soar/certs"})
	})
	mux.HandleFunc("/realms/soar/certs", func(w http.ResponseWriter, r *http.Request) {
		p.hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{
			map[string]any{"kty": "RSA", "kid": "r1", "use": "sig", "n": b64(p.rsa.N.Bytes()), "e": b64(big.NewInt(int64(p.rsa.E)).Bytes())},
			map[string]any{"kty": "EC", "kid": "e1", "crv": "P-256", "x": b64(p.ec.X.FillBytes(make([]byte, 32))), "y": b64(p.ec.Y.FillBytes(make([]byte, 32)))},
		}})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) issuer() string { return p.srv.URL + "/realms/soar" }

func (p *idp) token(t *testing.T, alg, kid string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	signed := enc(map[string]any{"alg": alg, "kid": kid, "typ": "JWT"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signed))
	var sig []byte
	switch alg {
	case "RS256":
		sig, _ = rsa.SignPKCS1v15(rand.Reader, p.rsa, crypto.SHA256, sum[:])
	case "ES256":
		r, s, _ := ecdsa.Sign(rand.Reader, p.ec, sum[:])
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	case "none":
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestOIDCVerifier(t *testing.T) {
	p := newIDP(t)
	v := &OIDCVerifier{Issuer: p.issuer(), Audience: "soar-api"}
	ctx := context.Background()
	now := time.Now()
	good := map[string]any{
		"iss": p.issuer(), "aud": []any{"soar-api", "account"}, "exp": float64(now.Add(time.Hour).Unix()),
		"sub": "u1", "preferred_username": "alice", "realm_access": map[string]any{"roles": []any{"analyst", "offline_access"}},
	}
	for _, alg := range []struct{ alg, kid string }{{"RS256", "r1"}, {"ES256", "e1"}} {
		if c, err := v.Verify(ctx, p.token(t, alg.alg, alg.kid, good)); err != nil || c["preferred_username"] != "alice" {
			t.Fatalf("%s: %v", alg.alg, err)
		}
	}
	bad := func(name string, mut func(map[string]any)) {
		c := map[string]any{}
		for k, x := range good {
			c[k] = x
		}
		mut(c)
		if _, err := v.Verify(ctx, p.token(t, "RS256", "r1", c)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	bad("expired", func(c map[string]any) { c["exp"] = float64(now.Add(-time.Hour).Unix()) })
	bad("wrong issuer", func(c map[string]any) { c["iss"] = "https://evil.example/realms/soar" })
	bad("wrong audience", func(c map[string]any) { c["aud"] = "other"; delete(c, "azp") })
	bad("not yet valid", func(c map[string]any) { c["nbf"] = float64(now.Add(time.Hour).Unix()) })
	if _, err := v.Verify(ctx, p.token(t, "none", "r1", good)); err == nil {
		t.Error("alg none accepted")
	}
	if _, err := v.Verify(ctx, p.token(t, "ES256", "r1", good)); err == nil {
		t.Error("algorithm/key confusion accepted")
	}
	tok := p.token(t, "RS256", "r1", good)
	parts := strings.Split(tok, ".")
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"`+p.issuer()+`","aud":"soar-api","exp":9999999999,"realm_access":{"roles":["admin"]}}`)) + "." + parts[2]
	if _, err := v.Verify(ctx, forged); err == nil {
		t.Error("modified claims accepted")
	}

	// the authenticator maps Keycloak roles to SOAR roles; unknown roles are ignored
	a := &Authenticator{Principals: memory.NewStore("soar").Principals, OIDC: v, Audience: "soar-api"}
	pr, err := a.Authenticate(ctx, tok)
	if err != nil || pr.Name != "alice" || len(pr.Roles) != 1 || pr.Roles[0] != RoleAnalyst {
		t.Fatalf("principal %+v %v", pr, err)
	}
	if !pr.Can("approvals:decide") || pr.Can("secrets:write") || !pr.Can("cases:write") {
		t.Errorf("analyst permissions wrong: %+v", pr)
	}
}

func TestAPIKeys(t *testing.T) {
	ctx := context.Background()
	st := memory.NewStore("soar")
	key, hash, err := NewAPIKey()
	if err != nil || !IsAPIKey(key) || hash != HashAPIKey(key) {
		t.Fatal("key generation")
	}
	doc := domain.Principal{Meta: domain.Meta{ID: "prn_1"}, Type: "service", Name: "wazuh-bridge", Roles: []string{RoleService}, KeyHash: hash}
	if _, err := st.Principals.Create(ctx, doc); err != nil {
		t.Fatal(err)
	}
	a := &Authenticator{Principals: st.Principals}
	p, err := a.Authenticate(ctx, key)
	if err != nil || p.Name != "wazuh-bridge" || p.ActorType != "system" {
		t.Fatalf("%+v %v", p, err)
	}
	if !p.Can("alerts:write") || p.Can("workflows:write") {
		t.Errorf("service permissions: %+v", p)
	}
	if _, err := a.Authenticate(ctx, key+"x"); err == nil {
		t.Error("wrong key accepted")
	}
	if _, err := a.Authenticate(ctx, "eyJhbGciOi.fake.jwt"); err == nil {
		t.Error("a JWT without an OIDC provider must be rejected")
	}
	_, _, _ = st.Principals.Update(ctx, "prn_1", func(x *domain.Principal) error { x.Disabled = true; return nil })
	a.Forget()
	if _, err := a.Authenticate(ctx, key); err == nil {
		t.Error("disabled principal accepted")
	}
	admin := Principal{Roles: []string{RoleAdmin}}
	if !admin.Can("anything:at_all") || !admin.HasAny([]string{"analyst"}) {
		t.Error("admin")
	}
	viewer := Principal{Name: "bob", Roles: []string{RoleViewer}}
	if viewer.Can("cases:write") || !viewer.HasAny([]string{"bob"}) || viewer.HasAny([]string{"analyst"}) {
		t.Error("viewer")
	}
}
