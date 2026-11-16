package api

// Security suite (Step 9 gate, spec section 13). Run it with `make test-security`.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/memory"
)

// readOnlyPosts are the non-GET operations that change nothing (they validate or test).
var readOnlyPosts = map[string]bool{
	"POST /workflows/validate":    true,
	"POST /mappings/test":         true,
	"POST /filters/test":          true,
	"POST /filters/simulate":      true,
	"POST /sources/{id}/test":     true,
	"POST /connections/{id}/test": true,
}

// Every mutating route needs a permission other than ":read", so a read-only role
// can never change anything, also after new routes are added.
func TestSecurityEveryMutationNeedsAWritePermission(t *testing.T) {
	s := New(memory.NewStore("soar"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, rt := range s.Routes() {
		if rt.Method == http.MethodGet {
			continue
		}
		perm := PermFor(rt.Method, rt.Path)
		key := rt.Method + " " + rt.Path
		if readOnlyPosts[key] {
			if !strings.HasSuffix(perm, ":read") {
				t.Errorf("%s changes nothing but needs %q", key, perm)
			}
			continue
		}
		if perm == "" || strings.HasSuffix(perm, ":read") || strings.HasSuffix(perm, ":read_metadata") {
			t.Errorf("%s is a mutation but only needs %q", key, perm)
		}
	}
}

type secSuite struct {
	t    *testing.T
	st   *store.Store
	srv  *httptest.Server
	keys map[string]string // role -> API key
}

func newSecSuite(t *testing.T) *secSuite {
	ctx := context.Background()
	st := memory.NewStore("soar")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	keys0, err := security.NewKeyring("k1", bytes.Repeat([]byte{7}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := New(st, log).WithAuth(&security.Authenticator{Principals: st.Principals}).WithSecrets(&security.SecretStore{Repo: st.Secrets, Keys: keys0})
	mux := http.NewServeMux()
	s.Mount(mux)
	srv := httptest.NewServer(obs.Middleware(log, mux))
	t.Cleanup(srv.Close)
	keys := map[string]string{}
	for _, role := range security.AllRoles {
		k, h, err := security.NewAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		keys[role] = k
		if _, err := st.Principals.Create(ctx, domain.Principal{
			Meta: domain.Meta{ID: "prn_" + role}, Type: "api_key", Name: "sec-" + role,
			Roles: []string{role}, KeyHash: h,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return &secSuite{t: t, st: st, srv: srv, keys: keys}
}

func (s *secSuite) do(method, path, token string, body []byte) (int, string) {
	s.t.Helper()
	req, _ := http.NewRequest(method, s.srv.URL+Prefix+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func concrete(path string) string {
	r := strings.NewReplacer("{id}", "sec_x", "{name}", "sec_x", "{eid}", "sec_x", "{task_id}", "sec_x")
	return r.Replace(path)
}

// Every route, every role, no token, a wrong token: authentication and the role table
// decide exactly as specified, before any handler runs.
func TestSecurityAuthorizationMatrix(t *testing.T) {
	s := newSecSuite(t)
	h := New(s.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, rt := range h.Routes() {
		path := concrete(rt.Path)
		key := rt.Method + " " + rt.Path
		body := []byte(`{}`)
		if rt.Method == http.MethodGet {
			body = nil
		}
		if rt.Path == "/stream" {
			continue // a server-sent event stream stays open; covered below
		}
		public := key == "GET /openapi.yaml"
		for _, tok := range []string{"", "soar_not-a-real-key-000000000000000000000000000", "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0."} {
			code, _ := s.do(rt.Method, path, tok, body)
			if public && code == http.StatusUnauthorized {
				t.Errorf("%s must be public, got 401", key)
			}
			if !public && code != http.StatusUnauthorized {
				t.Errorf("%s with token %.12q: %d, want 401", key, tok, code)
			}
		}
		perm := PermFor(rt.Method, rt.Path)
		for _, role := range security.AllRoles {
			code, out := s.do(rt.Method, path, s.keys[role], body)
			allowed := perm == "" || (security.Principal{Roles: []string{role}}).Can(perm)
			switch {
			case code == http.StatusUnauthorized:
				t.Errorf("%s as %s: 401 with a valid key", key, role)
			case allowed && code == http.StatusForbidden && !strings.Contains(out, "approvers"):
				t.Errorf("%s as %s: 403, but the role holds %s", key, role, perm)
			case !allowed && code != http.StatusForbidden:
				t.Errorf("%s as %s: %d, want 403 (needs %s)", key, role, code, perm)
			}
			if role == security.RoleViewer && rt.Method != http.MethodGet && !readOnlyPosts[key] && code != http.StatusForbidden {
				t.Errorf("a viewer reached the mutation %s: %d", key, code)
			}
		}
	}
	// the event stream accepts its token in the query string (EventSource cannot send headers)
	if code, _ := s.do("GET", "/stream?access_token=nope", "", nil); code != http.StatusUnauthorized {
		t.Errorf("stream with a bad token: %d", code)
	}
}

// Principals: only admins manage them; a key is shown once and only its hash is kept;
// disabled keys stop working at once.
func TestSecurityAPIKeysAndPrincipals(t *testing.T) {
	s := newSecSuite(t)
	for _, role := range []string{"engineer", "analyst", "viewer", "service"} {
		if code, _ := s.do("POST", "/principals", s.keys[role], []byte(`{"type":"api_key","name":"x","roles":["admin"]}`)); code != http.StatusForbidden {
			t.Errorf("%s created a principal: %d", role, code)
		}
	}
	code, out := s.do("POST", "/principals", s.keys["admin"], []byte(`{"type":"api_key","name":"ci-bot","roles":["service"]}`))
	if code != http.StatusCreated {
		t.Fatalf("admin create: %d %s", code, out)
	}
	var created struct {
		ID     string `json:"id"`
		APIKey string `json:"api_key"`
	}
	_ = json.Unmarshal([]byte(out), &created)
	if !strings.HasPrefix(created.APIKey, "soar_") {
		t.Fatalf("the new key must be returned once: %s", out)
	}
	if code, _ := s.do("GET", "/alerts", created.APIKey, nil); code != http.StatusOK {
		t.Errorf("new service key: %d", code)
	}
	for _, path := range []string{"/principals", "/principals/" + created.ID} {
		_, list := s.do("GET", path, s.keys["admin"], nil)
		if strings.Contains(list, created.APIKey) || strings.Contains(list, security.HashAPIKey(created.APIKey)) || strings.Contains(list, "key_hash") {
			t.Errorf("GET %s exposes the key or its hash: %s", path, list)
		}
	}
	p, _, err := s.st.Principals.Get(context.Background(), created.ID)
	if err != nil || p.KeyHash != security.HashAPIKey(created.APIKey) || strings.Contains(p.KeyHash, created.APIKey) {
		t.Errorf("stored principal must hold only the SHA-256 of the key: %+v %v", p, err)
	}
	if code, _ := s.do("PATCH", "/principals/"+created.ID, s.keys["admin"], []byte(`{"disabled":true}`)); code != http.StatusOK {
		t.Fatalf("disable: %d", code)
	}
	if code, _ := s.do("GET", "/alerts", created.APIKey, nil); code != http.StatusUnauthorized {
		t.Errorf("a disabled key still works: %d", code)
	}
	// unknown roles are refused
	if code, _ := s.do("POST", "/principals", s.keys["admin"], []byte(`{"type":"api_key","name":"y","roles":["root"]}`)); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown role: %d", code)
	}
}

// Secret values are write-only; every mutation is audited with the real actor; a
// failed or forbidden request is not.
func TestSecuritySecretsAreWriteOnlyAndMutationsAudited(t *testing.T) {
	s := newSecSuite(t)
	const value = "sec-suite-value-4711"
	if code, out := s.do("PUT", "/secrets/sec_token", s.keys["engineer"], []byte(`{"value":"`+value+`"}`)); code/100 != 2 {
		t.Fatalf("put secret: %d %s", code, out)
	}
	_, list := s.do("GET", "/secrets", s.keys["engineer"], nil)
	if strings.Contains(list, value) || !strings.Contains(list, "sec_token") {
		t.Errorf("secret listing: %s", list)
	}
	for _, role := range []string{"analyst", "viewer", "service"} {
		if code, _ := s.do("GET", "/secrets", s.keys[role], nil); code != http.StatusForbidden {
			t.Errorf("%s listed secrets: %d", role, code)
		}
	}
	doc, err := s.st.Secrets.List(context.Background(), store.ListQuery{Limit: 5})
	if err != nil || len(doc.Items) != 1 {
		t.Fatalf("stored secrets: %v %v", doc, err)
	}
	if b, _ := json.Marshal(doc.Items[0]); bytes.Contains(b, []byte(value)) {
		t.Errorf("the secret is stored in clear: %s", b)
	}
	if code, _ := s.do("POST", "/cases", s.keys["viewer"], []byte(`{"title":"forbidden"}`)); code != http.StatusForbidden {
		t.Fatalf("viewer case: %d", code)
	}
	if code, _ := s.do("POST", "/cases", s.keys["analyst"], []byte(`{"title":"audited","severity":2}`)); code != http.StatusCreated {
		t.Fatalf("analyst case: %d", code)
	}
	page, err := s.st.Audit.List(context.Background(), store.ListQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	actors := map[string]string{}
	for _, r := range page.Items {
		actors[r.Action+" "+r.Resource] = r.Actor
		if strings.Contains(r.Resource+r.Action+r.BeforeHash+r.AfterHash, value) {
			t.Errorf("audit record contains the secret: %+v", r)
		}
	}
	var sawSecret, sawCase bool
	for k, actor := range actors {
		if strings.Contains(k, "secret") && actor == "sec-engineer" {
			sawSecret = true
		}
		if strings.Contains(k, "case") && actor == "sec-analyst" {
			sawCase = true
		}
		if actor == "sec-viewer" {
			t.Errorf("a forbidden request was audited: %s", k)
		}
	}
	if !sawSecret || !sawCase || len(page.Items) != 2 {
		t.Errorf("want exactly the secret write and the case creation audited, got %v", actors)
	}
}

// Size and depth limits refuse oversized input before it is parsed (spec section 13).
func TestSecurityBodySizeAndDepthLimits(t *testing.T) {
	s := newSecSuite(t)
	deep := strings.Repeat(`{"a":`, security.MaxJSONDepth+1) + `1` + strings.Repeat(`}`, security.MaxJSONDepth+1)
	code, out := s.do("POST", "/cases", s.keys["admin"], []byte(deep))
	if code != http.StatusBadRequest || !strings.Contains(out, "levels deep") {
		t.Errorf("deep body: %d %s", code, out)
	}
	ok := strings.Repeat(`[`, 10) + strings.Repeat(`]`, 10)
	if security.JSONDepth([]byte(ok)) != 10 || security.JSONDepth([]byte(`{"s":"[[[[{{{{"}`)) != 1 || security.JSONDepth([]byte(`"\"[["`)) != 0 {
		t.Error("JSONDepth must count only structure outside strings")
	}
	big := `{"title":"` + strings.Repeat("x", 1<<20+10) + `"}`
	if code, _ := s.do("POST", "/cases", s.keys["admin"], []byte(big)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("body over 1 MiB: %d", code)
	}
}

// A connection or source can name only environment variables meant as integration
// secrets: an engineer must not be able to point a connection at the master key and
// have the worker send it to a server of their choice.
func TestSecurityEnvReferencesCannotReachPlatformKeys(t *testing.T) {
	s := newSecSuite(t)
	conn := func(ref string) int {
		code, _ := s.do("POST", "/connections", s.keys["engineer"], []byte(`{"connector_id":"dfir_iris","name":"x`+ref+`","auth_type":"bearer",
			"secret_refs":{"api_key":"`+ref+`"},"config":{"base_url":"https://attacker.invalid"}}`))
		return code
	}
	for _, ref := range []string{
		"env:SOAR_MASTER_KEY", "env:SOAR_OLD_MASTER_KEYS", "env:SOAR_OS_PASSWORD", "env:SOAR_BLOB_SECRET_KEY",
		"env:SOAR_BOOTSTRAP_API_KEY", "env:SOAR_SOURCE_TOKEN_WAZUH_PROD", "env:PATH",
	} {
		if c := conn(ref); c != http.StatusUnprocessableEntity {
			t.Errorf("connection with %s: %d, want 422", ref, c)
		}
	}
	for _, ref := range []string{"env:SOAR_SECRET_IRIS_KEY", "env:SOAR_WAZUH_INDEXER_PASSWORD", "iris_key"} {
		if c := conn(ref); c != http.StatusCreated {
			t.Errorf("connection with %s: %d, want 201", ref, c)
		}
	}
	src := func(ref string) (int, string) {
		return s.do("POST", "/sources", s.keys["engineer"], []byte(`{"name":"s-`+strings.ToLower(strings.NewReplacer(":", "-", "_", "-").Replace(ref))+`","system":"x",
			"adapter":"webhook","parser":"json","mapping":"m@1","auth":{"type":"token","token_ref":"`+ref+`"}}`))
	}
	if c, out := src("env:SOAR_MASTER_KEY"); c != http.StatusUnprocessableEntity || !strings.Contains(out, "auth.token_ref") {
		t.Errorf("source token_ref env:SOAR_MASTER_KEY: %d %s, want 422 naming auth.token_ref", c, out)
	}
	if _, out := src("env:SOAR_SOURCE_TOKEN_ACME"); strings.Contains(out, "auth.token_ref") {
		t.Errorf("an ingest token variable must be accepted: %s", out)
	}
	if c, _ := s.do("PUT", "/secrets/x", s.keys["engineer"], []byte(`{"value":"v","scope":"Not A Scope"}`)); c != http.StatusUnprocessableEntity {
		t.Errorf("invalid scope: %d, want 422", c)
	}
}

// Restarting the api must not undo an admin's revocation of the bootstrap key.
func TestSecurityBootstrapKeyStaysRevoked(t *testing.T) {
	ctx := context.Background()
	st := memory.NewStore("soar")
	const key = "soar_bootstrap-test-key-0123456789abcdefghij"
	if disabled, err := EnsureBootstrapKey(ctx, st, key); err != nil || disabled {
		t.Fatalf("first start: %v %v", disabled, err)
	}
	if _, _, err := st.Principals.Update(ctx, BootstrapPrincipalID, func(p *domain.Principal) error {
		p.Disabled, p.Roles = true, []string{security.RoleViewer}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	disabled, err := EnsureBootstrapKey(ctx, st, key) // the api restarts with the variable still set
	p, _, _ := st.Principals.Get(ctx, BootstrapPrincipalID)
	if err != nil || !disabled || !p.Disabled || len(p.Roles) != 1 || p.Roles[0] != security.RoleViewer {
		t.Errorf("restart undid the admin's changes: disabled=%v err=%v principal=%+v", disabled, err, p)
	}
	const rotated = "soar_bootstrap-rotated-0123456789abcdefghijk"
	if _, err := EnsureBootstrapKey(ctx, st, rotated); err != nil {
		t.Fatal(err)
	}
	p, _, _ = st.Principals.Get(ctx, BootstrapPrincipalID)
	if p.KeyHash != security.HashAPIKey(rotated) || !p.Disabled {
		t.Errorf("a rotated key must replace the hash and keep the principal disabled: %+v", p)
	}
}
