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
	if v, err := ss.ResolveFor(ctx, "iris_token", UseConnection, "iris"); err != nil || v != "s3cr3t" {
		if _, err := ss.Resolve(ctx, "iris_token"); err == nil {
			t.Fatal("a scoped secret must not resolve without a named use")
		}
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
	if v, err := (&SecretStore{Repo: st.Secrets, Keys: only2}).ResolveFor(ctx, "iris_token", UseConnection, "iris"); err != nil || v != "s3cr3t" {
		t.Fatalf("after rotation the old key must not be needed: %q %v", v, err)
	}
	if _, err := (&SecretStore{Repo: st.Secrets, Keys: k1}).ResolveFor(ctx, "iris_token", UseConnection, "iris"); err == nil {
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

	t.Setenv("SOAR_SECRET_TEST", "from-env")
	if v, _ := ss.Resolve(ctx, "env:SOAR_SECRET_TEST"); v != "from-env" {
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
	viewer := Principal{ID: "oidc:u-bob", Name: "bob", Type: "user", Roles: []string{RoleViewer}}
	if viewer.Can("cases:write") || !viewer.HasAny([]string{"bob"}) || !viewer.HasAny([]string{"user:bob"}) ||
		!viewer.HasAny([]string{"oidc:u-bob"}) || viewer.HasAny([]string{"analyst"}) {
		t.Error("viewer")
	}
	// an API key labelled like a user never stands in for that user
	labelled := Principal{ID: "prn_k", Name: "bob", Type: "api_key", Roles: []string{RoleAnalyst}}
	if labelled.HasAny([]string{"bob"}) || labelled.HasAny([]string{"user:bob"}) || !labelled.HasAny([]string{"prn_k"}) || !labelled.HasAny([]string{"analyst"}) {
		t.Error("API keys must match approvers by id or role only")
	}
}

// The user name comes from preferred_username, or a verified email; never an
// unverified address someone typed into their profile.
func TestOIDCNameNeedsAVerifiedEmail(t *testing.T) {
	p := newIDP(t)
	a := &Authenticator{OIDC: &OIDCVerifier{Issuer: p.issuer(), Audience: "soar-api"}}
	ctx := context.Background()
	base := func(extra map[string]any) string {
		c := map[string]any{
			"iss": p.issuer(), "aud": "soar-api", "exp": float64(time.Now().Add(time.Hour).Unix()), "sub": "u-9",
			"realm_access": map[string]any{"roles": []any{"analyst"}},
		}
		for k, v := range extra {
			c[k] = v
		}
		return p.token(t, "RS256", "r1", c)
	}
	for _, c := range []struct {
		claims map[string]any
		want   string
	}{
		{map[string]any{"preferred_username": "alice", "email": "x@example.com", "email_verified": true}, "alice"},
		{map[string]any{"email": "boss@example.com", "email_verified": true}, "boss@example.com"},
		{map[string]any{"email": "boss@example.com", "email_verified": false}, "u-9"},
		{map[string]any{"email": "boss@example.com"}, "u-9"},
	} {
		pr, err := a.Authenticate(ctx, base(c.claims))
		if err != nil || pr.Name != c.want || pr.ID != "oidc:u-9" {
			t.Errorf("%v: name %q id %q err %v, want name %q", c.claims, pr.Name, pr.ID, err, c.want)
		}
	}
	noSub := p.token(t, "RS256", "r1", map[string]any{"iss": p.issuer(), "aud": "soar-api", "exp": float64(time.Now().Add(time.Hour).Unix()), "preferred_username": "x"})
	if _, err := a.Authenticate(ctx, noSub); err == nil {
		t.Error("a token without sub has no stable identity and must be refused")
	}
}

// env: references reach only variables meant as integration secrets, never the
// platform's own keys, whatever the process environment holds.
func TestEnvReferencesAreRestricted(t *testing.T) {
	t.Setenv("SOAR_MASTER_KEY", "master-key-value")
	t.Setenv("SOAR_SECRET_IRIS", "iris-key")
	t.Setenv("SOAR_SOURCE_TOKEN_X", "ingest-token")
	for _, c := range []struct {
		name, use string
		ok        bool
	}{
		{"SOAR_SECRET_IRIS", UseConnection, true},
		{"SOAR_WAZUH_INDEXER_PASSWORD", UseConnection, true},
		{"SOAR_SOURCE_TOKEN_X", UseConnection, false}, // an ingest token must not be sent anywhere
		{"SOAR_SOURCE_TOKEN_X", UseIngestAuth, true},
		{"SOAR_MASTER_KEY", UseConnection, false},
		{"SOAR_MASTER_KEY", UseIngestAuth, false},
		{"SOAR_OS_PASSWORD", "", false},
		{"SOAR_BOOTSTRAP_API_KEY", "", false},
		{"SOAR_BLOB_SECRET_KEY", "", false},
		{"SOAR_SECRET_", UseConnection, false}, // the prefix alone names nothing
		{"PATH", "", false},
	} {
		if got := EnvRefAllowed(c.name, c.use); got != c.ok {
			t.Errorf("EnvRefAllowed(%s, %q) = %v, want %v", c.name, c.use, got, c.ok)
		}
	}
	if v, err := ResolveSecret("env:SOAR_MASTER_KEY"); err == nil || v != "" {
		t.Errorf("the master key resolved: %q %v", v, err)
	}
	ss := &SecretStore{}
	ctx := context.Background()
	if _, err := ss.Resolve(ctx, "env:SOAR_MASTER_KEY"); err == nil {
		t.Error("Resolve must refuse platform variables")
	}
	if v, err := ss.ResolveFor(ctx, "env:SOAR_SECRET_IRIS", UseConnection, "dfir_iris"); err != nil || v != "iris-key" {
		t.Errorf("allowed connection reference: %q %v", v, err)
	}
	if _, err := ss.ResolveFor(ctx, "env:SOAR_SOURCE_TOKEN_X", UseConnection, "dfir_iris"); err == nil {
		t.Error("an ingest token must not resolve for a connection")
	}
	if _, err := ss.ResolveFor(ctx, "env:SOAR_SECRET_IRIS", UseWorkflow, "workflows"); err == nil {
		t.Error("workflow parameters use stored secrets only")
	}
}

// A stored secret with a scope serves only the consumers its scope lists.
func TestSecretScopes(t *testing.T) {
	ctx := context.Background()
	st := memory.NewStore("soar")
	keys, _ := NewKeyring("k1", make([]byte, 32), nil)
	ss := &SecretStore{Repo: st.Secrets, Keys: keys}
	if _, err := ss.Put(ctx, "iris_key", "v-iris", "dfir_iris"); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Put(ctx, "shared", "v-shared", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Put(ctx, "two", "v-two", "dfir_iris, workflows"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ref, use, consumer string
		ok                 bool
	}{
		{"iris_key", UseConnection, "dfir_iris", true},
		{"iris_key", UseConnection, "outlook", false},
		{"iris_key", UseWorkflow, "workflows", false},
		{"shared", UseConnection, "outlook", true},
		{"shared", UseWorkflow, "workflows", true},
		{"two", UseWorkflow, "workflows", true},
		{"two", UseConnection, "wazuh", false},
	} {
		_, err := ss.ResolveFor(ctx, c.ref, c.use, c.consumer)
		if (err == nil) != c.ok {
			t.Errorf("%s for %s: err=%v, want ok=%v", c.ref, c.consumer, err, c.ok)
		}
	}
	for s, ok := range map[string]bool{"": true, "dfir_iris": true, "dfir_iris,workflows": true, "Bad Scope": false, "x;y": false} {
		if ValidScope(s) != ok {
			t.Errorf("ValidScope(%q) != %v", s, ok)
		}
	}
}

// A secret pinned to a host reaches only connections whose URL is on that host: a new
// connection of the same connector pointing elsewhere does not receive it.
func TestHostPinnedSecretFollowsTheEndpointNotTheConnector(t *testing.T) {
	ctx := context.Background()
	st := memory.NewStore("soar")
	keys, _ := NewKeyring("k1", make([]byte, 32), nil)
	ss := &SecretStore{Repo: st.Secrets, Keys: keys}
	if _, err := ss.Put(ctx, "iris_prod", "v-iris", "host:iris.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Put(ctx, "by_id", "v-id", "connection:con_legit"); err != nil {
		t.Fatal(err)
	}
	legit := domain.Connection{Meta: domain.Meta{ID: "con_legit"}, ConnectorID: "dfir_iris", Name: "iris-prod", Config: domain.JSON{"base_url": "https://IRIS.example.com:8443/"}}
	// same name as the legitimate connection (names are not unique), another host
	rogue := domain.Connection{Meta: domain.Meta{ID: "con_rogue"}, ConnectorID: "dfir_iris", Name: "iris-prod", Config: domain.JSON{"base_url": "https://attacker.invalid"}}
	// base_url on the pinned host, but another URL key a connector might use elsewhere
	mixed := domain.Connection{Meta: domain.Meta{ID: "con_mixed"}, ConnectorID: "dfir_iris", Name: "iris-x",
		Config: domain.JSON{"base_url": "https://iris.example.com", "options": map[string]any{"token_url": "https://attacker.invalid/token"}}}
	if v, err := ss.ResolveFor(ctx, "iris_prod", UseConnection, ConnectionConsumers(legit)...); err != nil || v != "v-iris" {
		t.Errorf("pinned host: %q %v", v, err)
	}
	if _, err := ss.ResolveFor(ctx, "iris_prod", UseConnection, ConnectionConsumers(rogue)...); err == nil {
		t.Error("a connection on another host received the pinned secret")
	}
	if _, err := ss.ResolveFor(ctx, "by_id", UseConnection, ConnectionConsumers(rogue)...); err == nil {
		t.Error("a connection with the same name but another id received a connection-scoped secret")
	}
	if v, err := ss.ResolveFor(ctx, "by_id", UseConnection, ConnectionConsumers(legit)...); err != nil || v != "v-id" {
		t.Errorf("connection id scope: %q %v", v, err)
	}
	if _, err := ss.ResolveFor(ctx, "iris_prod", UseConnection, ConnectionConsumers(mixed)...); err == nil {
		t.Error("a connection with URLs on two hosts received a host-pinned secret")
	}
	if _, err := ss.Resolver()("iris_prod", UseConnection, ConnectionConsumers(rogue)...); err == nil {
		t.Error("the collector's resolver must enforce scopes too")
	}
	for s, ok := range map[string]bool{"host:iris.example.com": true, "connection:con_legit": true, "source:wazuh-prod,ingest": true, "host:": false, "nope:x": false} {
		if ValidScope(s) != ok {
			t.Errorf("ValidScope(%q) != %v", s, ok)
		}
	}
}

// The host attribute fails closed: any endpoint-looking value that parses ambiguously,
// or names another host in any form, withholds it.
func TestSoleHostFailsClosed(t *testing.T) {
	for _, c := range []struct {
		cfg  domain.JSON
		want string
	}{
		{domain.JSON{"base_url": "https://IRIS.example.com:8443/api"}, "iris.example.com"},
		{domain.JSON{"base_url": "https://iris.example.com", "tenant": "acme", "timeout": 30}, "iris.example.com"},
		{domain.JSON{"base_url": "https://iris.example.com", "host": "attacker.invalid"}, ""},
		{domain.JSON{"base_url": "https://iris.example.com", "nested": map[string]any{"server": "attacker.invalid:443"}}, ""},
		{domain.JSON{"base_url": "https://user@iris.example.com"}, ""},
		{domain.JSON{"base_url": `https://iris.example.com\@attacker.invalid`}, ""},
		{domain.JSON{"base_url": " https://iris.example.com"}, ""},
		{domain.JSON{"base_url": "HTTPS://iris.example.com"}, ""},
		{domain.JSON{"base_url": "ftp://iris.example.com"}, ""},
		{domain.JSON{"base_url": "//attacker.invalid/x"}, ""},
		{domain.JSON{"base_url": "https://iris.example.com.", "x": 1}, ""},
		{domain.JSON{"base_url": "https://iris.example.com", "note": "see https://attacker.invalid/doc"}, ""},
		{domain.JSON{"host": "iris.example.com", "port": 443}, "iris.example.com"},
		{domain.JSON{"timeout": 30}, ""},
	} {
		if got := soleHost(map[string]any(c.cfg)); got != c.want {
			t.Errorf("%v: host %q, want %q", c.cfg, got, c.want)
		}
	}
}
