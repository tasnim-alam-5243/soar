package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/memory"
)

func TestAuthenticationAuthorisationAndAudit(t *testing.T) {
	ctx := context.Background()
	st := memory.NewStore("soar")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(st, log).WithAuth(&security.Authenticator{Principals: st.Principals})
	mux := http.NewServeMux()
	s.Mount(mux)
	srv := httptest.NewServer(obs.Middleware(log, mux))
	defer srv.Close()

	keys := map[string]string{}
	for _, p := range []struct{ name, role string }{{"vic", "viewer"}, {"ana", "analyst"}, {"eng", "engineer"}} {
		k, h, _ := security.NewAPIKey()
		keys[p.name] = k
		if _, err := st.Principals.Create(ctx, domain.Principal{Meta: domain.Meta{ID: "prn_" + p.name}, Type: "api_key", Name: p.name, Roles: []string{p.role}, KeyHash: h}); err != nil {
			t.Fatal(err)
		}
	}
	call := func(method, path, who string, body any) int {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+Prefix+path, rd)
		req.Header.Set("Content-Type", "application/json")
		if who != "" {
			req.Header.Set("Authorization", "Bearer "+keys[who])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if c := call("GET", "/openapi.yaml", "", nil); c != 200 {
		t.Errorf("the API document is public, got %d", c)
	}
	if c := call("GET", "/cases", "", nil); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c := call("GET", "/cases", "vic", nil); c != 200 {
		t.Errorf("viewer read: %d", c)
	}
	if c := call("POST", "/cases", "vic", map[string]any{"title": "x"}); c != 403 {
		t.Errorf("viewer write: %d", c)
	}
	if c := call("POST", "/cases", "ana", map[string]any{"title": "Phishing"}); c != 201 {
		t.Errorf("analyst write: %d", c)
	}
	if c := call("POST", "/workflows", "ana", map[string]any{"name": "w", "definition": goodDef}); c != 403 {
		t.Errorf("analysts cannot author workflows: %d", c)
	}
	if c := call("POST", "/workflows", "eng", map[string]any{"name": "w", "definition": goodDef}); c != 201 {
		t.Errorf("engineers author workflows: %d", c)
	}
	if c := call("GET", "/principals", "eng", nil); c != 403 {
		t.Errorf("only admins manage principals: %d", c)
	}
	if c := call("GET", "/audit", "eng", nil); c != 200 {
		t.Errorf("engineers read the audit log: %d", c)
	}

	// every successful mutation is audited with the real actor; failures are not
	time.Sleep(10 * time.Millisecond)
	page, err := st.Audit.List(ctx, store.ListQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	actors := map[string]int{}
	for _, a := range page.Items {
		actors[a.Actor]++
	}
	if actors["ana"] != 1 || actors["eng"] != 1 || actors["vic"] != 0 {
		t.Fatalf("audit actors: %v (%d records)", actors, len(page.Items))
	}

	// approvals: analysts may decide when their role is listed
	ap := domain.Approval{Meta: domain.Meta{ID: "apr_x"}, ExecutionID: "exe_x", NodeID: "n", Approvers: []string{"engineer"}, Status: "pending", DueAt: time.Now().Add(time.Hour)}
	if _, err := st.Approvals.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}
	s.pub = &fakeQueue{}
	if c := call("POST", "/approvals/apr_x/decision", "ana", map[string]any{"approved": true}); c != 403 {
		t.Errorf("an analyst not named in approvers: %d", c)
	}
}
