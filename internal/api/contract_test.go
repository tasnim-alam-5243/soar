package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/soar/integrations"
	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/blob"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/ingest/pack"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/memory"
)

func newTestServer(t *testing.T, st *store.Store, q ...QueueOps) *httptest.Server {
	t.Helper()
	srv, _ := newTestServerS(t, st, q...)
	return srv
}

func newTestServerS(t *testing.T, st *store.Store, q ...QueueOps) (*httptest.Server, *Server) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	s := New(st, log)
	reg, err := actions.Build(actions.Deps{Store: st, Log: log}, integrations.Packs, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.reg = reg
	if len(q) > 0 {
		s.WithQueue(q[0])
		if p, ok := q[0].(Publisher); ok {
			s.WithEngine(reg, p, 8)
		}
	}
	keys, _ := security.NewKeyring("k1", make([]byte, 32), nil)
	s.WithSecrets(&security.SecretStore{Repo: st.Secrets, Keys: keys})
	s.WithBlob(blob.NewMemory())
	s.Mount(mux)
	srv := httptest.NewServer(obs.Middleware(log, mux))
	t.Cleanup(srv.Close)
	return srv, s
}

// harness sends requests and checks every response against openapi.yaml.
type harness struct {
	t   *testing.T
	sp  *spec
	url string
	hit map[string]bool
}

type reqOpt struct {
	headers map[string]string
	loose   bool // do not validate the request against the spec (negative tests)
	raw     []byte
	ctype   string
}

func (h *harness) do(method, path string, body any, want int, opts ...reqOpt) (map[string]any, http.Header) {
	h.t.Helper()
	var o reqOpt
	if len(opts) > 0 {
		o = opts[0]
	}
	u, err := url.Parse(path)
	if err != nil {
		h.t.Fatal(err)
	}
	op, found := h.sp.find(method, u.Path)
	if !found {
		h.t.Fatalf("%s %s is not in openapi.yaml", method, u.Path)
	}

	if !o.loose {
		declared := map[string]bool{}
		for _, p := range op.params {
			if p["in"] == "query" {
				declared[p["name"].(string)] = true
			}
		}
		for k := range u.Query() {
			if !declared[k] {
				h.t.Errorf("%s %s: query parameter %q is not declared in the spec", method, u.Path, k)
			}
		}
		if rb := asMap(op.def["requestBody"]); rb != nil && body != nil {
			schema := asMap(asMap(asMap(rb["content"])["application/json"]))["schema"]
			raw, _ := json.Marshal(body)
			var decoded any
			_ = json.Unmarshal(raw, &decoded)
			for _, e := range h.sp.check(schema, decoded, "request") {
				h.t.Errorf("%s %s: request breaks the spec: %s", method, u.Path, e)
			}
		}
	}

	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	if o.raw != nil {
		rd = bytes.NewReader(o.raw)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, h.url+Prefix+path, rd)
	switch {
	case o.raw != nil:
		req.Header.Set("Content-Type", o.ctype)
	case body != nil:
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		h.t.Fatalf("%s %s: status %d, want %d; body: %s", method, path, resp.StatusCode, want, raw)
	}
	h.hit[op.id] = true

	responses := asMap(op.def["responses"])
	def := responses[strconv.Itoa(resp.StatusCode)]
	if def == nil && (resp.StatusCode == 413 || resp.StatusCode == 500 || resp.StatusCode == 503) {
		def = responses["default"] // generic failures any operation may return
	}
	if def == nil {
		h.t.Fatalf("%s %s: status %d is not declared for %s", method, path, resp.StatusCode, op.id)
	}
	rdef := asMap(h.sp.resolve(def))
	for name := range asMap(rdef["headers"]) {
		if resp.Header.Get(name) == "" {
			h.t.Errorf("%s %s: declared response header %s is missing", method, path, name)
		}
	}
	if resp.Header.Get("X-Request-Id") == "" {
		h.t.Errorf("%s %s: X-Request-ID missing", method, path)
	}

	content := asMap(rdef["content"])
	if len(content) == 0 {
		if len(raw) != 0 {
			h.t.Errorf("%s %s: response declares no body but has %d bytes", method, path, len(raw))
		}
		return nil, resp.Header
	}
	mt := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	media := asMap(content[mt])
	if media == nil {
		h.t.Fatalf("%s %s: content type %q is not declared for status %d", method, path, mt, resp.StatusCode)
	}
	if !strings.HasSuffix(mt, "json") {
		return nil, resp.Header
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		h.t.Fatalf("%s %s: response is not JSON: %v: %s", method, path, err, raw)
	}
	for _, e := range h.sp.check(media["schema"], decoded, "response") {
		h.t.Errorf("%s %s -> %d: response breaks the spec: %s", method, path, resp.StatusCode, e)
	}
	m, _ := decoded.(map[string]any)
	if resp.StatusCode >= 400 {
		if m["status"] != float64(resp.StatusCode) || m["request_id"] == nil || m["request_id"] == "" {
			h.t.Errorf("%s %s: problem document needs matching status and request_id: %s", method, path, raw)
		}
	}
	return m, resp.Header
}

func sa(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func problemPaths(m map[string]any) []string {
	var out []string
	errs, _ := m["errors"].([]any)
	for _, e := range errs {
		out = append(out, sa(e.(map[string]any), "path"))
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var goodDef = map[string]any{
	"nodes": []any{
		map[string]any{"id": "check", "action": "http.request", "params": map[string]any{"url": "\"https://intel.example/u\""}},
		map[string]any{"id": "decide", "type": "branch", "cases": []any{map[string]any{"when": "inputs.x > 1", "next": []any{"block"}}}, "default": []any{"close"}},
		map[string]any{"id": "block", "action": "notify.log", "params": map[string]any{"message": "blocked"}},
		map[string]any{"id": "close", "action": "case.close"},
	},
	"edges": []any{"check -> decide"},
}

func TestContract(t *testing.T) {
	sp, err := loadSpec()
	if err != nil {
		t.Fatal(err)
	}
	st := memory.NewStore("soar")
	bundle, err := pack.Load(integrations.Packs, "wazuh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pack.Install(context.Background(), st, bundle); err != nil {
		t.Fatal(err)
	}
	fq := &fakeQueue{}
	srv, srvS := newTestServerS(t, st, fq)
	srvS.testWait = 100 * time.Millisecond
	h := &harness{t: t, sp: sp, url: srv.URL, hit: map[string]bool{}}
	ctx := context.Background()

	t.Run("openapi", func(t *testing.T) {
		h.t = t
		h.do("GET", "/openapi.yaml", nil, 200)
	})

	t.Run("workflows", func(t *testing.T) {
		h.t = t
		out, _ := h.do("POST", "/workflows/validate", map[string]any{"definition": goodDef}, 200)
		if out["valid"] != true {
			t.Errorf("good definition reported invalid: %v", out)
		}
		cyc := map[string]any{"nodes": []any{map[string]any{"id": "a", "action": "x"}, map[string]any{"id": "b", "action": "x"}}, "edges": []any{"a -> b", "b -> a"}}
		out, _ = h.do("POST", "/workflows/validate", map[string]any{"definition": cyc}, 200)
		if out["valid"] != false || len(out["errors"].([]any)) == 0 {
			t.Errorf("cycle not detected: %v", out)
		}
		agent := map[string]any{"nodes": []any{map[string]any{"id": "a", "type": "agent"}}}
		out, _ = h.do("POST", "/workflows/validate", map[string]any{"definition": agent}, 200)
		if out["valid"] != false {
			t.Errorf("agent node must be rejected: %v", out)
		}
		h.do("POST", "/workflows/validate", map[string]any{}, 422, reqOpt{loose: true})

		out, _ = h.do("POST", "/workflows", map[string]any{"description": "no name"}, 422, reqOpt{loose: true})
		if !contains(problemPaths(out), "name") || !contains(problemPaths(out), "definition") {
			t.Errorf("422 must point at name and definition: %v", out)
		}
		h.do("POST", "/workflows", map[string]any{"name": "x", "definition": goodDef, "bogus": 1}, 400, reqOpt{loose: true})
		h.do("POST", "/workflows", map[string]any{"name": "x", "definition": goodDef}, 400, reqOpt{headers: map[string]string{"Idempotency-Key": "bad key!"}})

		body := map[string]any{
			"name": "phishing_triage", "description": "d", "definition": goodDef,
			"triggers": []any{map[string]any{"type": "alert", "match": `alert.severity >= 3`}},
		}
		created, hdr := h.do("POST", "/workflows", body, 201, reqOpt{headers: map[string]string{"Idempotency-Key": "wf-1"}})
		id := sa(created, "id")
		if created["version"] != float64(1) || created["status"] != "draft" || hdr.Get("Location") != Prefix+"/workflows/"+id {
			t.Fatalf("unexpected create result: %v %v", created, hdr)
		}
		again, hdr2 := h.do("POST", "/workflows", body, 201, reqOpt{headers: map[string]string{"Idempotency-Key": "wf-1"}})
		if sa(again, "id") != id || hdr2.Get("Idempotent-Replayed") != "true" {
			t.Errorf("replay must return the original resource: %v %v", again, hdr2)
		}
		dup, _ := h.do("POST", "/workflows", body, 409)
		if !strings.HasSuffix(sa(dup, "type"), "workflow-exists") {
			t.Errorf("duplicate name: %v", dup)
		}

		page, _ := h.do("GET", "/workflows?name=phishing_triage&sort=-version&limit=10", nil, 200)
		if len(page["items"].([]any)) != 1 {
			t.Errorf("list: %v", page)
		}
		h.do("GET", "/workflows?sort=bogus", nil, 400, reqOpt{loose: true})
		h.do("GET", "/workflows?limit=0", nil, 400, reqOpt{loose: true})
		h.do("GET", "/workflows?surprise=1", nil, 400, reqOpt{loose: true})
		h.do("GET", "/workflows?cursor=garbage", nil, 400)

		got, ghdr := h.do("GET", "/workflows/"+id, nil, 200)
		etag := ghdr.Get("ETag")
		if sa(got, "id") != id || etag == "" {
			t.Fatalf("get: %v %q", got, etag)
		}
		h.do("GET", "/workflows/wf_missing", nil, 404)

		upd := map[string]any{"name": "phishing_triage", "description": "edited", "definition": goodDef}
		_, h2 := h.do("PUT", "/workflows/"+id, upd, 200, reqOpt{headers: map[string]string{"If-Match": etag}})
		if h2.Get("ETag") == etag {
			t.Error("ETag must change after an update")
		}
		stale, _ := h.do("PUT", "/workflows/"+id, upd, 409, reqOpt{headers: map[string]string{"If-Match": etag}})
		if !strings.HasSuffix(sa(stale, "type"), "version-conflict") {
			t.Errorf("stale If-Match: %v", stale)
		}
		h.do("PUT", "/workflows/"+id, upd, 400, reqOpt{headers: map[string]string{"If-Match": "nonsense"}})
		h.do("PUT", "/workflows/"+id, upd, 200) // without If-Match: last writer wins
		h.do("PUT", "/workflows/"+id, map[string]any{"name": "renamed", "definition": goodDef}, 422)
		h.do("PUT", "/workflows/wf_missing", upd, 404)

		// a second workflow with an invalid definition cannot be published
		bad, _ := h.do("POST", "/workflows", map[string]any{"name": "broken", "definition": cyc}, 201)
		pub, _ := h.do("POST", "/workflows/"+sa(bad, "id")+"/publish", nil, 422)
		if len(problemPaths(pub)) == 0 {
			t.Errorf("publish 422 must explain why: %v", pub)
		}

		published, _ := h.do("POST", "/workflows/"+id+"/publish", nil, 200)
		if published["status"] != "published" {
			t.Errorf("publish: %v", published)
		}
		h.do("POST", "/workflows/"+id+"/publish", nil, 409)
		imm, _ := h.do("PUT", "/workflows/"+id, upd, 409)
		if !strings.HasSuffix(sa(imm, "type"), "immutable") {
			t.Errorf("published versions must be immutable: %v", imm)
		}
		h.do("POST", "/workflows/wf_missing/publish", nil, 404)

		v2, _ := h.do("POST", "/workflows/"+id+"/versions", map[string]any{}, 201)
		if v2["version"] != float64(2) || v2["status"] != "draft" || v2["name"] != "phishing_triage" || sa(v2, "id") == id {
			t.Errorf("new version: %v", v2)
		}
		v3, _ := h.do("POST", "/workflows/"+sa(v2, "id")+"/versions", map[string]any{"description": "third"}, 201)
		if v3["version"] != float64(3) || v3["description"] != "third" {
			t.Errorf("third version: %v", v3)
		}
		h.do("POST", "/workflows/wf_missing/versions", map[string]any{}, 404)

		// running: published versions run, drafts only as a dry run
		run, rh := h.do("POST", "/workflows/"+id+"/run", map[string]any{"inputs": map[string]any{"x": 2}}, 202, reqOpt{headers: map[string]string{"Idempotency-Key": "run-1"}})
		if run["status"] != "queued" || run["workflow_name"] != "phishing_triage" || rh.Get("Location") != Prefix+"/executions/"+sa(run, "id") {
			t.Fatalf("run: %v %v", run, rh)
		}
		if m := fq.last(); m.Subject != "results."+strconv.Itoa(int(run["partition"].(float64))) || !strings.HasPrefix(m.ID, "start:") {
			t.Errorf("start message: %+v", m)
		}
		again, _ = h.do("POST", "/workflows/"+id+"/run", map[string]any{"inputs": map[string]any{"x": 2}}, 202, reqOpt{headers: map[string]string{"Idempotency-Key": "run-1"}})
		if sa(again, "id") != sa(run, "id") {
			t.Error("run with the same Idempotency-Key must not start a second execution")
		}
		h.do("POST", "/workflows/"+id+"/run", nil, 202) // empty body: no inputs
		h.do("POST", "/workflows/"+sa(v2, "id")+"/run", nil, 409)
		h.do("POST", "/workflows/"+sa(v2, "id")+"/run", map[string]any{"dry_run": true}, 202)
		h.do("POST", "/workflows/"+id+"/run", map[string]any{"alert_id": "alt_missing"}, 422)
		h.do("POST", "/workflows/"+id+"/run", map[string]any{"bogus": true}, 400, reqOpt{loose: true})
		h.do("POST", "/workflows/wf_missing/run", nil, 404)
		h.do("POST", "/workflows/"+sa(bad, "id")+"/run", map[string]any{"dry_run": true}, 422)
	})

	t.Run("executions", func(t *testing.T) {
		h.t = t
		now := time.Now().UTC()
		ex := domain.Execution{
			Meta: domain.NewMeta(domain.PrefixExecution), WorkflowID: "wf_1", WorkflowVersion: 1, Status: "running",
			Trigger: domain.ExecutionTrigger{Type: "alert", AlertID: "alt_9"}, StartedAt: &now, Partition: 3,
		}
		other := domain.Execution{Meta: domain.NewMeta(domain.PrefixExecution), WorkflowID: "wf_2", WorkflowVersion: 1, Status: "succeeded", Trigger: domain.ExecutionTrigger{Type: "manual"}}
		for _, e := range []domain.Execution{ex, other} {
			if _, err := st.Executions.Create(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 3; i++ {
			sr := domain.StepRun{Meta: domain.NewMeta(domain.PrefixStepRun), ExecutionID: ex.ID, NodeID: "n" + strconv.Itoa(i), Attempt: 1, Status: "succeeded"}
			sr.CreatedAt = now.Add(time.Duration(i) * time.Second)
			if _, err := st.StepRuns.Create(ctx, sr); err != nil {
				t.Fatal(err)
			}
		}
		page, _ := h.do("GET", "/executions?workflow_id=wf_1&status=running,queued&alert_id=alt_9", nil, 200)
		if len(page["items"].([]any)) != 1 {
			t.Errorf("filters: %v", page)
		}
		all, _ := h.do("GET", "/executions?workflow_id=wf_1,wf_2&sort=-started_at", nil, 200)
		if len(all["items"].([]any)) != 2 {
			t.Errorf("all: %v", all)
		}
		one, hdr := h.do("GET", "/executions/"+ex.ID, nil, 200)
		if one["partition"] != float64(3) || hdr.Get("ETag") == "" {
			t.Errorf("get: %v", one)
		}
		h.do("GET", "/executions/exe_missing", nil, 404)
		steps, _ := h.do("GET", "/executions/"+ex.ID+"/steps?limit=2", nil, 200)
		items := steps["items"].([]any)
		if len(items) != 2 || items[0].(map[string]any)["node_id"] != "n0" || sa(steps, "next_cursor") == "" {
			t.Errorf("steps page 1 (oldest first): %v", steps)
		}
		rest, _ := h.do("GET", "/executions/"+ex.ID+"/steps?limit=2&cursor="+url.QueryEscape(sa(steps, "next_cursor")), nil, 200)
		if len(rest["items"].([]any)) != 1 || rest["next_cursor"] != nil {
			t.Errorf("steps page 2: %v", rest)
		}
		h.do("GET", "/executions/exe_missing/steps", nil, 404)
		cx, _ := h.do("POST", "/executions/"+ex.ID+"/cancel", map[string]any{"reason": "false positive"}, 202)
		if sa(cx, "id") != ex.ID || !strings.HasPrefix(fq.last().ID, "cancel:") {
			t.Errorf("cancel: %v %+v", cx, fq.last())
		}
		h.do("POST", "/executions/"+other.ID+"/cancel", nil, 409)
		h.do("POST", "/executions/exe_missing/cancel", nil, 404)
		h.do("POST", "/executions/"+ex.ID+"/retry", nil, 409) // still running

		// retry of a failed execution starts a new one with the same inputs
		wfs, _ := h.do("GET", "/workflows?name=phishing_triage&status=published", nil, 200)
		pubID := sa(wfs["items"].([]any)[0].(map[string]any), "id")
		failed := domain.Execution{
			Meta: domain.NewMeta(domain.PrefixExecution), WorkflowID: pubID, WorkflowVersion: 1, Status: "failed",
			Trigger: domain.ExecutionTrigger{Type: "manual"}, Context: map[string]any{"status": "failed", "inputs": map[string]any{"x": 5}},
		}
		if _, err := st.Executions.Create(ctx, failed); err != nil {
			t.Fatal(err)
		}
		re, _ := h.do("POST", "/executions/"+failed.ID+"/retry", nil, 202)
		if re["trigger"].(map[string]any)["retry_of"] != failed.ID || re["status"] != "queued" {
			t.Errorf("retry: %v", re)
		}
		h.do("POST", "/executions/exe_missing/retry", nil, 404)
	})

	t.Run("approvals", func(t *testing.T) {
		h.t = t
		exe := domain.Execution{
			Meta: domain.NewMeta(domain.PrefixExecution), WorkflowID: "wf_1", WorkflowVersion: 1, Status: "waiting", Partition: 2,
			Trigger: domain.ExecutionTrigger{Type: "manual"},
		}
		if _, err := st.Executions.Create(ctx, exe); err != nil {
			t.Fatal(err)
		}
		ap := domain.Approval{
			Meta: domain.Meta{ID: "apr_1"}, ExecutionID: exe.ID, NodeID: "ask", Approvers: []string{"analyst"},
			Message: "delete 3 emails?", Status: "pending", DueAt: time.Now().Add(time.Hour), Partition: 2,
		}
		if _, err := st.Approvals.Create(ctx, ap); err != nil {
			t.Fatal(err)
		}
		list, _ := h.do("GET", "/approvals?status=pending&sort=due_at", nil, 200)
		if len(list["items"].([]any)) != 1 {
			t.Fatalf("approvals: %v", list)
		}
		h.do("GET", "/approvals/apr_1", nil, 200)
		h.do("GET", "/approvals/apr_missing", nil, 404)
		h.do("POST", "/approvals/apr_1/decision", map[string]any{"comment": "no verdict"}, 422, reqOpt{loose: true})
		dec, _ := h.do("POST", "/approvals/apr_1/decision", map[string]any{"approved": true, "comment": "ok"}, 200)
		if dec["status"] != "approved" || dec["decided_by"] != "dev" {
			t.Errorf("decision: %v", dec)
		}
		if m := fq.last(); m.Subject != "results.2" || m.ID != "approval:apr_1" {
			t.Errorf("approval message: %+v", m)
		}
		h.do("POST", "/approvals/apr_1/decision", map[string]any{"approved": false}, 409)
		h.do("POST", "/approvals/apr_missing/decision", map[string]any{"approved": false}, 404)
	})

	t.Run("connectors_and_secrets", func(t *testing.T) {
		h.t = t
		cs, _ := h.do("GET", "/connectors", nil, 200)
		ids := map[string]bool{}
		for _, c := range cs["items"].([]any) {
			ids[sa(c.(map[string]any), "id")] = true
		}
		if !ids["builtin"] || !ids["wazuh"] {
			t.Errorf("connectors: %v", ids)
		}
		acts, _ := h.do("GET", "/connectors/builtin/actions", nil, 200)
		if len(acts["items"].([]any)) < 8 {
			t.Errorf("builtin actions: %v", acts)
		}
		h.do("GET", "/connectors/wazuh/actions", nil, 200)
		h.do("GET", "/connectors/nope/actions", nil, 404)

		sec, _ := h.do("PUT", "/secrets/vt_api_key", map[string]any{"value": "super-secret-value", "scope": "virustotal"}, 200)
		if sec["name"] != "vt_api_key" || sec["ciphertext"] != nil || sec["key_id"] != "k1" {
			t.Errorf("secret metadata: %v", sec)
		}
		h.do("PUT", "/secrets/9bad", map[string]any{"value": "x"}, 422)
		h.do("PUT", "/secrets/x", map[string]any{"value": ""}, 422, reqOpt{loose: true})
		list, _ := h.do("GET", "/secrets?sort=name", nil, 200)
		raw, _ := json.Marshal(list)
		if strings.Contains(string(raw), "super-secret-value") || strings.Contains(string(raw), "ciphertext") {
			t.Errorf("secret material leaked: %s", raw)
		}
		h.do("DELETE", "/secrets/vt_api_key", nil, 204)
		h.do("DELETE", "/secrets/vt_api_key", nil, 404)
	})

	t.Run("principals_roles_audit_me", func(t *testing.T) {
		h.t = t
		me, _ := h.do("GET", "/me", nil, 200)
		if me["name"] != "dev" || me["auth_enabled"] != false {
			t.Errorf("me: %v", me)
		}
		pk, phdr := h.do("POST", "/principals", map[string]any{"type": "api_key", "name": "ci-bot", "roles": []any{"service"}}, 201)
		if !strings.HasPrefix(sa(pk, "api_key"), "soar_") || pk["key_hash"] != nil || phdr.Get("Location") == "" {
			t.Fatalf("principal: %v", pk)
		}
		h.do("POST", "/principals", map[string]any{"type": "api_key", "name": "ci-bot", "roles": []any{"service"}}, 409)
		h.do("POST", "/principals", map[string]any{"type": "robot", "name": "x", "roles": []any{"god"}}, 422, reqOpt{loose: true})
		pid := sa(pk, "id")
		got, _ := h.do("GET", "/principals/"+pid, nil, 200)
		if got["api_key"] != nil || got["key_hash"] != nil {
			t.Errorf("key material must never be returned again: %v", got)
		}
		h.do("GET", "/principals?type=api_key&sort=name", nil, 200)
		up, _ := h.do("PATCH", "/principals/"+pid, map[string]any{"disabled": true, "roles": []any{"viewer"}}, 200)
		if up["disabled"] != true {
			t.Errorf("patch: %v", up)
		}
		h.do("PATCH", "/principals/"+pid, map[string]any{"roles": []any{}}, 422, reqOpt{loose: true})
		h.do("PATCH", "/principals/prn_missing", map[string]any{"disabled": true}, 404)
		h.do("GET", "/principals/prn_missing", nil, 404)
		h.do("DELETE", "/principals/"+pid, nil, 204)
		h.do("DELETE", "/principals/"+pid, nil, 404)
		roles, _ := h.do("GET", "/roles", nil, 200)
		if len(roles["items"].([]any)) != 5 {
			t.Errorf("roles: %v", roles)
		}
		aud, _ := h.do("GET", "/audit?action=principal.create&sort=-created_at", nil, 200)
		if len(aud["items"].([]any)) != 1 || aud["items"].([]any)[0].(map[string]any)["actor"] != "dev" {
			t.Errorf("audit: %v", aud)
		}
		h.do("GET", "/audit?from=yesterday", nil, 400)
	})

	t.Run("dashboard", func(t *testing.T) {
		h.t = t
		d, _ := h.do("GET", "/dashboard", nil, 200)
		if _, ok := d["executions_last_24h"].(map[string]any)["queued"]; !ok || len(d["queues"].([]any)) != 2 || len(d["alerts_by_severity"].(map[string]any)) != 5 {
			t.Errorf("dashboard: %v", d)
		}
	})

	t.Run("stream", func(t *testing.T) {
		h.t = t
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+Prefix+"/stream?topics=executions", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		buf := make([]byte, 64)
		_, _ = resp.Body.Read(buf) // the "connected" preamble arrives before any event
		srvS.Events().Publish(UIEvent{Topic: "cases", ID: "cas_x", At: time.Now()})
		srvS.Events().Publish(UIEvent{Topic: "executions", ID: "exe_42", At: time.Now()})
		got := ""
		for !strings.Contains(got, "exe_42") {
			n, err := resp.Body.Read(buf)
			if err != nil {
				t.Fatalf("no event: %v (got %q)", err, got)
			}
			got += string(buf[:n])
		}
		if strings.Contains(got, "cas_x") {
			t.Error("topic filter ignored")
		}
		h.hit["streamEvents"] = true
	})

	t.Run("alerts", func(t *testing.T) {
		h.t = t
		h.do("POST", "/alerts", map[string]any{"title": "x", "severity": 9}, 422, reqOpt{loose: true})
		h.do("POST", "/alerts", map[string]any{"severity": 3}, 422, reqOpt{loose: true})
		h.do("POST", "/alerts", map[string]any{"title": "x", "severity": 3, "observables": []any{map[string]any{"type": "carrier-pigeon", "value": "x"}}}, 422, reqOpt{loose: true})
		var ids []string
		for i, sev := range []int{2, 4, 5} {
			a, hdr := h.do("POST", "/alerts", map[string]any{
				"title": "alert " + strconv.Itoa(i), "severity": sev, "tags": []any{"t"},
				"observables": []any{map[string]any{"type": "ip", "value": "10.0.0." + strconv.Itoa(i), "role": "source"}},
				"ext":         map[string]any{"rule": map[string]any{"id": "5715"}},
			}, 201)
			ids = append(ids, sa(a, "id"))
			if a["occurrence_count"] != float64(1) || a["source_id"] != "manual" || hdr.Get("Location") == "" {
				t.Errorf("created alert: %v", a)
			}
			time.Sleep(2 * time.Millisecond)
		}
		first, _ := h.do("GET", "/alerts?limit=2", nil, 200)
		if len(first["items"].([]any)) != 2 || sa(first, "next_cursor") == "" {
			t.Fatalf("page 1: %v", first)
		}
		if first["items"].([]any)[0].(map[string]any)["title"] != "alert 2" {
			t.Errorf("newest first expected: %v", first["items"])
		}
		second, _ := h.do("GET", "/alerts?limit=2&cursor="+url.QueryEscape(sa(first, "next_cursor")), nil, 200)
		if len(second["items"].([]any)) != 1 {
			t.Errorf("page 2: %v", second)
		}
		hi, _ := h.do("GET", "/alerts?min_severity=4&source_id=manual&sort=severity", nil, 200)
		if len(hi["items"].([]any)) != 2 {
			t.Errorf("min_severity: %v", hi)
		}
		h.do("GET", "/alerts?severity=2,5", nil, 200)
		h.do("GET", "/alerts?from=2020-01-01T00:00:00Z&to=2100-01-01T00:00:00Z", nil, 200)
		h.do("GET", "/alerts?from=yesterday", nil, 400, reqOpt{loose: true})
		h.do("GET", "/alerts?min_severity=9", nil, 400, reqOpt{loose: true})
		got, _ := h.do("GET", "/alerts/"+ids[0], nil, 200)
		if sa(got, "id") != ids[0] {
			t.Errorf("get: %v", got)
		}
		h.do("GET", "/alerts/alt_missing", nil, 404)
	})

	var caseID string
	t.Run("cases", func(t *testing.T) {
		h.t = t
		h.do("POST", "/cases", map[string]any{"severity": 3}, 422, reqOpt{loose: true})
		h.do("POST", "/cases", map[string]any{"title": "t", "severity": 0}, 422, reqOpt{loose: true})
		c, hdr := h.do("POST", "/cases", map[string]any{"title": "Phishing campaign", "severity": 4, "tags": []any{"phish"}}, 201, reqOpt{headers: map[string]string{"Idempotency-Key": "case-1"}})
		caseID = sa(c, "id")
		if c["status"] != "open" || hdr.Get("Location") != Prefix+"/cases/"+caseID {
			t.Fatalf("case: %v", c)
		}
		rep, _ := h.do("POST", "/cases", map[string]any{"title": "Phishing campaign", "severity": 4, "tags": []any{"phish"}}, 201, reqOpt{headers: map[string]string{"Idempotency-Key": "case-1"}})
		if sa(rep, "id") != caseID {
			t.Errorf("replay created another case")
		}
		list, _ := h.do("GET", "/cases?status=open&tag=phish&sort=title&severity=4", nil, 200)
		if len(list["items"].([]any)) != 1 {
			t.Errorf("list: %v", list)
		}
		got, ghdr := h.do("GET", "/cases/"+caseID, nil, 200)
		etag := ghdr.Get("ETag")
		if got["title"] != "Phishing campaign" || etag == "" {
			t.Fatalf("get: %v", got)
		}
		h.do("GET", "/cases/cas_missing", nil, 404)

		h.do("PATCH", "/cases/"+caseID, map[string]any{"status": "weird"}, 422, reqOpt{loose: true})
		h.do("PATCH", "/cases/"+caseID, map[string]any{"id": "cas_other"}, 400, reqOpt{loose: true})
		h.do("PATCH", "/cases/cas_missing", map[string]any{"title": "x"}, 404)
		upd, uhdr := h.do("PATCH", "/cases/"+caseID, map[string]any{"status": "closed", "verdict": "true_positive", "assignee": "alice"}, 200, reqOpt{headers: map[string]string{"If-Match": etag}})
		if upd["status"] != "closed" || upd["closed_at"] == nil || upd["title"] != "Phishing campaign" || uhdr.Get("ETag") == etag {
			t.Errorf("patch: %v", upd)
		}
		h.do("PATCH", "/cases/"+caseID, map[string]any{"title": "late"}, 409, reqOpt{headers: map[string]string{"If-Match": etag}})
		reopen, _ := h.do("PATCH", "/cases/"+caseID, map[string]any{"status": "open"}, 200)
		if reopen["closed_at"] != nil {
			t.Errorf("reopening must clear closed_at: %v", reopen)
		}

		h.do("POST", "/cases/"+caseID+"/tasks", map[string]any{"description": "no title"}, 422, reqOpt{loose: true})
		h.do("POST", "/cases/cas_missing/tasks", map[string]any{"title": "x"}, 404)
		task, _ := h.do("POST", "/cases/"+caseID+"/tasks", map[string]any{"title": "Block sender", "assignee": "bob", "due_at": "2026-12-01T09:00:00Z"}, 201)
		if task["case_id"] != caseID || task["status"] != "open" {
			t.Errorf("task: %v", task)
		}
		tasks, _ := h.do("GET", "/cases/"+caseID+"/tasks", nil, 200)
		if len(tasks["items"].([]any)) != 1 {
			t.Errorf("tasks: %v", tasks)
		}
		h.do("GET", "/cases/cas_missing/tasks", nil, 404)

		h.do("POST", "/cases/"+caseID+"/observables", map[string]any{"type": "carrier-pigeon", "value": "x"}, 422, reqOpt{loose: true})
		h.do("POST", "/cases/cas_missing/observables", map[string]any{"type": "ip", "value": "1.2.3.4"}, 404)
		o1, _ := h.do("POST", "/cases/"+caseID+"/observables", map[string]any{"type": "ip", "value": "203.0.113.7"}, 201)
		o2, _ := h.do("POST", "/cases/"+caseID+"/observables", map[string]any{"type": "ip", "value": " 203.0.113.7 "}, 200)
		if sa(o1, "id") != sa(o2, "id") || len(o2["case_ids"].([]any)) != 1 {
			t.Errorf("same indicator must map to one observable linked once: %v %v", o1, o2)
		}
		other, _ := h.do("POST", "/cases", map[string]any{"title": "Second case"}, 201)
		o3, _ := h.do("POST", "/cases/"+sa(other, "id")+"/observables", map[string]any{"type": "ip", "value": "203.0.113.7"}, 200)
		if len(o3["case_ids"].([]any)) != 2 {
			t.Errorf("observable must link to both cases: %v", o3)
		}

		tl, _ := h.do("GET", "/cases/"+caseID+"/timeline", nil, 200)
		var kinds []string
		for _, it := range tl["items"].([]any) {
			kinds = append(kinds, sa(it.(map[string]any), "event_type"))
		}
		want := []string{"case.created", "case.updated", "task.created", "observable.added"}
		for _, k := range want {
			if !contains(kinds, k) {
				t.Errorf("timeline lacks %s: %v", k, kinds)
			}
		}
		h.do("GET", "/cases/cas_missing/timeline", nil, 404)
	})

	t.Run("case_alerts_tasks_evidence", func(t *testing.T) {
		h.t = t
		now := time.Now().UTC()
		var ids []string
		for i := 0; i < 2; i++ {
			a := domain.Alert{
				Meta: domain.NewMeta(domain.PrefixAlert), Kind: "alert", SourceID: "wazuh-prod", Title: "SSH login " + strconv.Itoa(i),
				Severity: 3, ReceivedAt: now, Observables: []domain.Observable{{Type: "ip", Value: "10.10.192." + strconv.Itoa(i)}},
			}
			if _, err := st.Alerts.Create(ctx, a); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, a.ID)
		}
		// open a case from an alert: 201, observables follow; again: 200, same case
		c1, hdr := h.do("POST", "/alerts/"+ids[0]+"/case", nil, 201)
		caseID := sa(c1, "id")
		if hdr.Get("Location") != Prefix+"/cases/"+caseID || c1["title"] != "SSH login 0" || len(c1["alert_ids"].([]any)) != 1 {
			t.Fatalf("alert to case: %v", c1)
		}
		again, _ := h.do("POST", "/alerts/"+ids[0]+"/case", map[string]any{}, 200)
		if sa(again, "id") != caseID {
			t.Error("a second request must reuse the case opened from the alert")
		}
		h.do("POST", "/alerts/"+ids[1]+"/case", map[string]any{"case_id": "cas_missing"}, 422)
		h.do("POST", "/alerts/alt_missing/case", nil, 404)
		linked, _ := h.do("POST", "/cases/"+caseID+"/alerts", map[string]any{"alert_ids": ids}, 200)
		if len(linked["alert_ids"].([]any)) != 2 {
			t.Errorf("link: %v", linked)
		}
		h.do("POST", "/cases/"+caseID+"/alerts", map[string]any{"alert_ids": []any{"alt_missing"}}, 422)
		h.do("POST", "/cases/"+caseID+"/alerts", map[string]any{"alert_ids": []any{}}, 422, reqOpt{loose: true})
		h.do("POST", "/cases/cas_missing/alerts", map[string]any{"alert_ids": ids}, 404)
		snaps, _ := h.do("GET", "/cases/"+caseID+"/alerts?sort=linked_at", nil, 200)
		if len(snaps["items"].([]any)) != 2 || snaps["items"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)["title"] == nil {
			t.Errorf("snapshots: %v", snaps)
		}
		h.do("GET", "/cases/cas_missing/alerts", nil, 404)
		if a, _, _ := st.Alerts.Get(ctx, ids[1]); a.CaseID != caseID {
			t.Errorf("alert.case_id not set: %q", a.CaseID)
		}
		obs, _ := h.do("GET", "/observables?case_id="+caseID, nil, 200)
		if len(obs["items"].([]any)) < 1 {
			t.Errorf("observables of the alert must follow it into the case: %v", obs)
		}
		withAlerts, _ := h.do("POST", "/cases", map[string]any{"title": "Campaign", "alert_ids": []any{ids[1]}}, 201)
		if len(withAlerts["alert_ids"].([]any)) != 1 {
			t.Errorf("create with alert_ids: %v", withAlerts)
		}
		h.do("POST", "/cases", map[string]any{"title": "x", "alert_ids": []any{"alt_missing"}}, 422)

		task, _ := h.do("POST", "/cases/"+caseID+"/tasks", map[string]any{"title": "Reset password"}, 201)
		tid := sa(task, "id")
		done, _ := h.do("PATCH", "/cases/"+caseID+"/tasks/"+tid, map[string]any{"status": "done", "assignee": "carol"}, 200)
		if done["status"] != "done" || done["assignee"] != "carol" {
			t.Errorf("task patch: %v", done)
		}
		h.do("PATCH", "/cases/"+caseID+"/tasks/"+tid, map[string]any{"status": "exploded"}, 422, reqOpt{loose: true})
		h.do("PATCH", "/cases/"+sa(withAlerts, "id")+"/tasks/"+tid, map[string]any{"status": "open"}, 404) // task of another case
		h.do("PATCH", "/cases/"+caseID+"/tasks/tsk_missing", map[string]any{"status": "open"}, 404)

		ev, _ := h.do("POST", "/cases/"+caseID+"/evidence?filename=headers.eml", nil, 201, reqOpt{raw: []byte("Received: from evil\r\n"), ctype: "message/rfc822"})
		if ev["size"] != float64(21) || len(sa(ev, "sha256")) != 64 || ev["content_type"] != "message/rfc822" {
			t.Fatalf("evidence: %v", ev)
		}
		h.do("POST", "/cases/"+caseID+"/evidence?filename=empty.txt", nil, 400, reqOpt{raw: []byte{}, ctype: "text/plain"})
		h.do("POST", "/cases/"+caseID+"/evidence?filename=", nil, 422, reqOpt{raw: []byte("x"), ctype: "text/plain", loose: true})
		h.do("POST", "/cases/cas_missing/evidence?filename=a.txt", nil, 404, reqOpt{raw: []byte("x"), ctype: "text/plain"})
		link, _ := h.do("GET", "/cases/"+caseID+"/evidence/"+sa(ev, "id"), nil, 200)
		if !strings.HasPrefix(sa(link, "url"), "memory://soar-evidence/") {
			t.Errorf("evidence link: %v", link)
		}
		h.do("GET", "/cases/"+caseID+"/evidence/evd_missing", nil, 404)
		got, _ := h.do("GET", "/cases/"+caseID, nil, 200)
		if len(got["evidence"].([]any)) != 1 {
			t.Errorf("case evidence: %v", got["evidence"])
		}
	})

	t.Run("alert_triage", func(t *testing.T) {
		h.t = t
		now := time.Now().UTC()
		mk := func(title, status string, sev int) string {
			a := domain.Alert{
				Meta: domain.NewMeta(domain.PrefixAlert), Kind: "alert", SourceID: "wazuh-prod", Title: title, Status: status,
				Severity: sev, ReceivedAt: now, Entities: &domain.Entities{Hosts: []string{"web-01"}},
			}
			if _, err := st.Alerts.Create(ctx, a); err != nil {
				t.Fatal(err)
			}
			return a.ID
		}
		legacy := mk("Legacy brute force", "", 4) // written before alerts had a status
		fresh := mk("Fresh brute force", "new", 5)

		news, _ := h.do("GET", "/alerts?status=new&q=brute+force", nil, 200)
		got := map[string]bool{}
		for _, it := range news["items"].([]any) {
			got[sa(it.(map[string]any), "id")] = true
		}
		if !got[legacy] || !got[fresh] {
			t.Errorf("status=new must include alerts without a status; q must match titles: %v", news)
		}
		none, _ := h.do("GET", "/alerts?q=no-such-words-anywhere", nil, 200)
		if len(none["items"].([]any)) != 0 {
			t.Errorf("q matched nothing yet returned: %v", none)
		}

		h.do("PATCH", "/alerts/"+fresh, map[string]any{}, 422, reqOpt{loose: true})
		h.do("PATCH", "/alerts/"+fresh, map[string]any{"status": "snoozed"}, 422, reqOpt{loose: true})
		h.do("PATCH", "/alerts/"+fresh, map[string]any{"priority": 1}, 400, reqOpt{loose: true})
		h.do("PATCH", "/alerts/alt_missing", map[string]any{"status": "acknowledged"}, 404)
		ack, ahdr := h.do("PATCH", "/alerts/"+fresh, map[string]any{"status": "acknowledged", "assignee": " dana "}, 200)
		if ack["status"] != "acknowledged" || ack["assignee"] != "dana" || ahdr.Get("ETag") == "" {
			t.Errorf("acknowledge: %v", ack)
		}
		bad, _ := h.do("PATCH", "/alerts/"+fresh, map[string]any{"status": "closed"}, 422)
		if !contains(problemPaths(bad), "verdict") {
			t.Errorf("closing without a verdict must point at verdict: %v", bad)
		}
		closed, _ := h.do("PATCH", "/alerts/"+fresh, map[string]any{"status": "closed", "verdict": "false_positive"}, 200)
		if closed["status"] != "closed" || closed["verdict"] != "false_positive" {
			t.Errorf("close: %v", closed)
		}

		// linking moves a new alert to in_progress; closing the case closes it with the case verdict
		c, _ := h.do("POST", "/alerts/"+legacy+"/case", nil, 201)
		if a, _, _ := st.Alerts.Get(ctx, legacy); a.Status != "in_progress" || a.CaseID != sa(c, "id") {
			t.Errorf("linked alert: status %q case %q", a.Status, a.CaseID)
		}
		h.do("PATCH", "/cases/"+sa(c, "id"), map[string]any{"status": "closed", "verdict": "true_positive"}, 200)
		if a, _, _ := st.Alerts.Get(ctx, legacy); a.Status != "closed" || a.Verdict != "true_positive" {
			t.Errorf("closing the case must close its alert: status %q verdict %q", a.Status, a.Verdict)
		}

		names, _ := h.do("GET", "/assignees", nil, 200)
		var list []string
		for _, n := range names["items"].([]any) {
			list = append(list, n.(string))
		}
		if !contains(list, "alice") || !contains(list, "dana") {
			t.Errorf("assignees must include names used on cases and alerts: %v", list)
		}

		d, _ := h.do("GET", "/dashboard?range=24h", nil, 200)
		rg, _ := d["range"].(map[string]any)
		if rg == nil || rg["name"] != "24h" || rg["interval_seconds"] != float64(3600) {
			t.Fatalf("dashboard range: %v", d["range"])
		}
		if n := len(rg["alert_series"].([]any)); n < 24 || n > 25 {
			t.Errorf("24h at 1h buckets must give 24-25 points, got %d", n)
		}
		if rg["alerts"].(map[string]any)["current"].(float64) < 2 {
			t.Errorf("range alerts: %v", rg["alerts"])
		}
		hosts := rg["top_hosts"].([]any)
		if len(hosts) == 0 || hosts[0].(map[string]any)["value"] != "web-01" {
			t.Errorf("top hosts: %v", hosts)
		}
		h.do("GET", "/dashboard?range=30d", nil, 400, reqOpt{loose: true})
		h.do("GET", "/dashboard?bogus=1", nil, 400, reqOpt{loose: true})
	})

	t.Run("observables", func(t *testing.T) {
		h.t = t
		list, _ := h.do("GET", "/observables?type=ip&case_id="+caseID+"&sort=-first_seen", nil, 200)
		items := list["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("list: %v", list)
		}
		id := sa(items[0].(map[string]any), "id")
		one, _ := h.do("GET", "/observables/"+id, nil, 200)
		if one["value"] != "203.0.113.7" {
			t.Errorf("get: %v", one)
		}
		h.do("GET", "/observables/obs_missing", nil, 404)
		h.do("GET", "/observables?value=198.51.100.1", nil, 200)
	})

	t.Run("connections", func(t *testing.T) {
		h.t = t
		h.do("POST", "/connections", map[string]any{
			"connector_id": "iris", "name": "iris-prod", "auth_type": "bearer",
			"secret_refs": map[string]any{"token": "a literal secret with spaces!"},
		}, 422, reqOpt{loose: true})
		h.do("POST", "/connections", map[string]any{"connector_id": "iris", "name": "x", "auth_type": "telepathy"}, 422, reqOpt{loose: true})
		c, hdr := h.do("POST", "/connections", map[string]any{
			"connector_id": "iris", "name": "iris-prod", "auth_type": "bearer",
			"secret_refs": map[string]any{"token": "iris_service_token"}, "config": map[string]any{"base_url": "https://iris.example"},
		}, 201)
		id := sa(c, "id")
		if hdr.Get("Location") != Prefix+"/connections/"+id {
			t.Errorf("location: %v", hdr)
		}
		list, _ := h.do("GET", "/connections?connector_id=iris&sort=name", nil, 200)
		if len(list["items"].([]any)) != 1 {
			t.Errorf("list: %v", list)
		}
		h.do("GET", "/connections/"+id, nil, 200)
		h.do("POST", "/connections/"+id+"/test", nil, 422) // "iris" is not an installed connector (the package is dfir_iris)
		wz, _ := h.do("POST", "/connections", map[string]any{
			"connector_id": "wazuh", "name": "wazuh-mgr", "auth_type": "basic",
			"secret_refs": map[string]any{"password": "wazuh_api_password"}, "config": map[string]any{"base_url": "https://wazuh.example:55000", "username": "soar"},
		}, 201)
		res, _ := h.do("POST", "/connections/"+sa(wz, "id")+"/test", nil, 200)
		if res["ok"] != false || sa(res, "execution_id") == "" || !strings.HasPrefix(fq.last().ID, "start:") {
			t.Errorf("connection test (nothing processes it in this test): %v", res)
		}
		h.do("POST", "/connections/con_missing/test", nil, 404)
		h.do("DELETE", "/connections/"+id, nil, 204)
		h.do("GET", "/connections/"+id, nil, 404)
		h.do("DELETE", "/connections/"+id, nil, 404)
	})

	t.Run("sources_mappings_filters_lists", func(t *testing.T) {
		h.t = t
		all, _ := h.do("GET", "/sources?sort=name", nil, 200)
		if n := len(all["items"].([]any)); n != 2 {
			t.Fatalf("the Wazuh pack installs two sources, got %d", n)
		}
		push, _ := h.do("GET", "/sources?adapter=webhook&enabled=true&system=wazuh-prod", nil, 200)
		pushItem := push["items"].([]any)[0].(map[string]any)
		pid := sa(pushItem, "id")
		if len(push["items"].([]any)) != 1 || pushItem["name"] != "wazuh-prod" {
			t.Fatalf("push source: %v", push)
		}
		got, ghdr := h.do("GET", "/sources/"+pid, nil, 200)
		if got["mapping_id"] != "wazuh_alert@1" {
			t.Errorf("source: %v", got)
		}
		h.do("GET", "/sources/sr_missing", nil, 404)
		hl, _ := h.do("GET", "/sources/"+pid+"/health", nil, 200)
		if hl["source"] != "wazuh-prod" {
			t.Errorf("health: %v", hl)
		}

		// creating sources: every mistake is reported with a path, secrets are never inline
		bad, _ := h.do("POST", "/sources", map[string]any{"name": "Bad Name", "adapter": "carrier-pigeon", "parser": "xml", "mapping": "wazuh_alert@1"}, 422, reqOpt{loose: true})
		for _, want := range []string{"name", "adapter", "parser"} {
			if !contains(problemPaths(bad), want) {
				t.Errorf("422 must point at %s: %v", want, bad)
			}
		}
		lit, _ := h.do("POST", "/sources", map[string]any{
			"name": "acme", "adapter": "webhook", "parser": "json", "mapping": "wazuh_alert@1",
			"auth": map[string]any{"type": "token", "token_ref": "my-literal-secret!"},
		}, 422)
		if !contains(problemPaths(lit), "auth.token_ref") {
			t.Errorf("inline secrets must be refused: %v", lit)
		}
		h.do("POST", "/sources", map[string]any{
			"name": "acme", "adapter": "webhook", "parser": "json", "mapping": "nope@3",
			"auth": map[string]any{"type": "none"},
		}, 422)
		h.do("POST", "/sources", map[string]any{"name": "acme", "adapter": "poll_http", "parser": "json", "mapping": "wazuh_alert@1", "schedule": "whenever"}, 422)
		src := map[string]any{
			"name": "acme-siem", "adapter": "webhook", "parser": "json", "mapping": "wazuh_alert@1", "filters": []any{"wazuh-default@1"},
			"auth": map[string]any{"type": "token", "token_ref": "env:SOAR_SOURCE_TOKEN_ACME"}, "dedup": map[string]any{"window": "5m"}, "rate_limit": map[string]any{"rps": 50, "burst": 100},
		}
		created, chdr := h.do("POST", "/sources", src, 201)
		sid := sa(created, "id")
		if created["system"] != "acme-siem" || created["enabled"] != true || chdr.Get("Location") != Prefix+"/sources/"+sid {
			t.Errorf("created source: %v", created)
		}
		dupe, _ := h.do("POST", "/sources", src, 409)
		if !strings.HasSuffix(sa(dupe, "type"), "already-exists") {
			t.Errorf("one source per name: %v", dupe)
		}
		etag := chdr.Get("ETag")
		h.do("PATCH", "/sources/"+sid, map[string]any{"enabled": false, "rate_limit": map[string]any{"rps": 10, "burst": 20}}, 200, reqOpt{headers: map[string]string{"If-Match": etag}})
		h.do("PATCH", "/sources/"+sid, map[string]any{"enabled": true}, 409, reqOpt{headers: map[string]string{"If-Match": etag}})
		h.do("PATCH", "/sources/"+sid, map[string]any{"mapping": "wazuh_alert@9"}, 422)
		h.do("PATCH", "/sources/"+sid, map[string]any{"rate_limit": map[string]any{"rps": 0}}, 422)
		h.do("PATCH", "/sources/sr_missing", map[string]any{"enabled": true}, 404)
		_ = ghdr

		// testing a source: sample payloads go through parser, filters and mapping, nothing is published
		alert := func(level int, id string) map[string]any {
			return map[string]any{
				"id": "t." + id, "timestamp": "2026-10-29T12:48:31.597+0600", "rule": map[string]any{"level": level, "id": id, "description": "sshd: authentication success."},
				"agent": map[string]any{"id": "001", "name": "web01"}, "data": map[string]any{"srcip": "10.10.192.1"},
			}
		}
		tr, _ := h.do("POST", "/sources/"+pid+"/test", map[string]any{"payload": []any{alert(3, "5715"), alert(1, "100"), map[string]any{"id": "x", "rule": map[string]any{"level": 5, "id": "1", "description": "no time"}}}}, 200)
		if len(tr["events"].([]any)) != 1 || tr["filtered"] != float64(1) || len(tr["failures"].([]any)) != 1 || tr["rules_fired"].(map[string]any)["wazuh-default/min-level|drop"] != float64(1) {
			t.Errorf("source test: %v", tr)
		}
		h.do("POST", "/sources/"+pid+"/test", map[string]any{"payload": "{not json"}, 422)
		h.do("POST", "/sources/"+pid+"/test", map[string]any{}, 422, reqOpt{loose: true})
		h.do("POST", "/sources/sr_missing/test", map[string]any{"payload": alert(3, "1")}, 404)

		// mappings: published versions are immutable, every publish makes the next version
		def := map[string]any{"fields": map[string]any{"title": "raw.msg", "severity": "3"}, "dedup_key": []any{"raw.msg"}}
		m1, _ := h.do("POST", "/mappings", map[string]any{"name": "acme_alert", "definition": def}, 201)
		m2, _ := h.do("POST", "/mappings", map[string]any{"name": "acme_alert", "definition": def}, 201)
		if m1["version"] != float64(1) || m2["version"] != float64(2) {
			t.Errorf("versions: %v %v", m1["version"], m2["version"])
		}
		badDef, _ := h.do("POST", "/mappings", map[string]any{"name": "broken", "definition": map[string]any{"fields": map[string]any{"title": "raw.msg >=", "bogus": "1"}}}, 422)
		if len(problemPaths(badDef)) < 2 {
			t.Errorf("every problem in a definition is listed: %v", badDef)
		}
		h.do("POST", "/mappings", map[string]any{"name": "x"}, 422, reqOpt{loose: true})
		ml, _ := h.do("GET", "/mappings?name=acme_alert&sort=-version", nil, 200)
		if len(ml["items"].([]any)) != 2 || ml["items"].([]any)[0].(map[string]any)["version"] != float64(2) {
			t.Errorf("mapping list: %v", ml)
		}
		h.do("GET", "/mappings/"+sa(m1, "id"), nil, 200)
		h.do("GET", "/mappings/map_missing", nil, 404)

		ok, _ := h.do("POST", "/mappings/test", map[string]any{"mapping": "wazuh_alert@1", "sample": alert(3, "5715")}, 200)
		if ok["valid"] != true || ok["alert"].(map[string]any)["severity"] != float64(1) {
			t.Errorf("mapping test: %v", ok)
		}
		fail, _ := h.do("POST", "/mappings/test", map[string]any{"mapping": "wazuh_alert@1", "sample": map[string]any{"id": "x", "rule": map[string]any{"level": 5, "id": "1", "description": "d"}}}, 200)
		if fail["valid"] != false || fail["error"].(map[string]any)["field"] != "fields.occurred_at" || fail["error"].(map[string]any)["expr"] == nil {
			t.Errorf("a failing mapping must name the expression: %v", fail)
		}
		inline, _ := h.do("POST", "/mappings/test", map[string]any{"definition": def, "sample": map[string]any{"msg": "hello"}}, 200)
		if inline["valid"] != true || inline["alert"].(map[string]any)["title"] != "hello" {
			t.Errorf("inline mapping: %v", inline)
		}
		h.do("POST", "/mappings/test", map[string]any{"definition": map[string]any{"fields": map[string]any{"title": "(("}}, "sample": map[string]any{}}, 200)
		h.do("POST", "/mappings/test", map[string]any{"mapping": "wazuh_alert@1", "definition": def, "sample": map[string]any{}}, 422, reqOpt{loose: true})
		h.do("POST", "/mappings/test", map[string]any{"mapping": "wazuh_alert", "sample": map[string]any{}}, 422)
		h.do("POST", "/mappings/test", map[string]any{"mapping": "nope@4", "sample": map[string]any{}}, 404)

		// filter sets
		rules := []any{map[string]any{"name": "no-lab", "stage": "pre_map", "action": "drop", "when": `get_path(raw, "agent.name") == "lab01"`}}
		f1, _ := h.do("POST", "/filters", map[string]any{"name": "acme-default", "rules": rules}, 201)
		f2, _ := h.do("POST", "/filters", map[string]any{"name": "acme-default", "mode": "all", "default": "keep", "rules": rules}, 201)
		if f1["version"] != float64(1) || f2["version"] != float64(2) || f1["mode"] != "first_match" {
			t.Errorf("filter versions: %v %v", f1, f2)
		}
		h.do("POST", "/filters", map[string]any{"name": "bad", "rules": []any{map[string]any{"name": "r", "stage": "pre_map", "action": "explode", "when": "true"}}}, 422)
		h.do("POST", "/filters", map[string]any{"name": "bad", "rules": []any{map[string]any{"name": "r", "stage": "pre_map", "action": "drop", "script": "x.py"}}}, 422)
		h.do("POST", "/filters", map[string]any{"name": "bad"}, 422, reqOpt{loose: true})
		fl, _ := h.do("GET", "/filters?name=acme-default", nil, 200)
		if len(fl["items"].([]any)) != 2 {
			t.Errorf("filter list: %v", fl)
		}
		h.do("GET", "/filters/"+sa(f1, "id"), nil, 200)
		h.do("GET", "/filters/flt_missing", nil, 404)

		// managed lists feed in_list() in filters, without a deploy
		put, phdr := h.do("PUT", "/lists/wazuh_noisy_rule_ids", map[string]any{"items": []any{"1002", "1003"}}, 200)
		if len(put["items"].([]any)) != 2 || phdr.Get("ETag") == "" {
			t.Errorf("put list: %v", put)
		}
		h.do("PUT", "/lists/wazuh_noisy_rule_ids", map[string]any{"items": []any{""}}, 422, reqOpt{loose: true})
		h.do("GET", "/lists/wazuh_noisy_rule_ids", nil, 200)
		h.do("GET", "/lists/nope", nil, 404)
		ll, _ := h.do("GET", "/lists", nil, 200)
		if len(ll["items"].([]any)) != 2 {
			t.Errorf("lists: %v", ll)
		}
		noisy := alert(12, "1002")
		ft, _ := h.do("POST", "/filters/test", map[string]any{"filter_set": "wazuh-default@1", "sample": noisy}, 200)
		if ft["drop"] != true || ft["rule"] != "drop-noisy-rule-ids" || len(ft["trace"].([]any)) == 0 {
			t.Errorf("the edited list must take effect immediately: %v", ft)
		}
		ft, _ = h.do("POST", "/filters/test", map[string]any{"filter_set": "wazuh-default@1", "sample": alert(12, "7")}, 200)
		if ft["drop"] != false {
			t.Errorf("a high-level alert is kept: %v", ft)
		}
		ft, _ = h.do("POST", "/filters/test", map[string]any{"inline": map[string]any{"rules": []any{map[string]any{"name": "tagit", "stage": "pre_map", "action": "tag", "tag": "vip", "when": "true"}}}, "sample": noisy}, 200)
		if ft["drop"] != false || ft["tags"].([]any)[0] != "vip" {
			t.Errorf("inline filter: %v", ft)
		}
		h.do("POST", "/filters/test", map[string]any{"filter_set": "wazuh-default@1", "inline": map[string]any{}, "sample": noisy}, 422, reqOpt{loose: true})
		h.do("POST", "/filters/test", map[string]any{"filter_set": "wazuh-default@1", "stage": "later", "sample": noisy}, 422, reqOpt{loose: true})
		h.do("POST", "/filters/test", map[string]any{"filter_set": "nope@1", "sample": noisy}, 404)
	})

	t.Run("dead_letters_and_queue_stats", func(t *testing.T) {
		h.t = t
		page, _ := h.do("GET", "/dlq?limit=2", nil, 200)
		if len(page["items"].([]any)) != 2 || sa(page, "next_cursor") != "7" {
			t.Errorf("dlq page: %v", page)
		}
		h.do("GET", "/dlq?cursor=7", nil, 200)
		h.do("GET", "/dlq?cursor=abc", nil, 400, reqOpt{loose: true})
		h.do("GET", "/dlq?bogus=1", nil, 400, reqOpt{loose: true})
		rp, _ := h.do("POST", "/dlq/8/replay", nil, 200)
		if rp["replayed"] != float64(8) {
			t.Errorf("replay: %v", rp)
		}
		h.do("POST", "/dlq/999/replay", nil, 404)
		h.do("POST", "/dlq/abc/replay", nil, 400)
		qs, _ := h.do("GET", "/queues/stats", nil, 200)
		if len(qs["streams"].([]any)) != 2 {
			t.Errorf("stats: %v", qs)
		}
	})

	t.Run("every_operation_was_exercised", func(t *testing.T) {
		var missing []string
		for _, op := range sp.operations() {
			if !h.hit[op.id] {
				missing = append(missing, op.id)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("operations in openapi.yaml never called by the contract test: %v", missing)
		}
	})
}

func TestRoutesMatchSpec(t *testing.T) {
	sp, err := loadSpec()
	if err != nil {
		t.Fatal(err)
	}
	inSpec := map[string]bool{}
	for _, op := range sp.operations() {
		inSpec[op.method+" "+op.template] = true
	}
	inCode := map[string]bool{}
	for _, rt := range New(memory.NewStore("soar"), slog.Default()).Routes() {
		inCode[rt.Method+" "+rt.Path] = true
	}
	for k := range inCode {
		if !inSpec[k] {
			t.Errorf("route %q exists in code but not in api/openapi.yaml", k)
		}
	}
	for k := range inSpec {
		if !inCode[k] {
			t.Errorf("operation %q is in api/openapi.yaml but has no route", k)
		}
	}
	ids := map[string]bool{}
	for _, op := range sp.operations() {
		if ids[op.id] {
			t.Errorf("duplicate operationId %s", op.id)
		}
		ids[op.id] = true
		if asMap(op.def["responses"])["default"] == nil {
			t.Errorf("%s: declare a default problem response", op.id)
		}
	}
}

func TestUnknownPathsAndMethodsAreProblems(t *testing.T) {
	srv := newTestServer(t, memory.NewStore("soar"))
	for _, tc := range []struct {
		method, path string
		status       int
		allow        string
	}{
		{"GET", "/api/v1/nope", 404, ""},
		{"GET", "/api/v1/workflows/a/b/c", 404, ""},
		{"DELETE", "/api/v1/alerts", 405, "GET, POST"},
		{"PUT", "/api/v1/cases/cas_1", 405, "GET, PATCH"},
	} {
		req, _ := http.NewRequestWithContext(context.Background(), tc.method, srv.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var p Problem
		_ = json.NewDecoder(resp.Body).Decode(&p)
		resp.Body.Close()
		if resp.StatusCode != tc.status || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") || p.Status != tc.status || p.RequestID == "" {
			t.Errorf("%s %s: status=%d ct=%q problem=%+v", tc.method, tc.path, resp.StatusCode, resp.Header.Get("Content-Type"), p)
		}
		if tc.allow != "" && resp.Header.Get("Allow") != tc.allow {
			t.Errorf("%s %s: Allow=%q want %q", tc.method, tc.path, resp.Header.Get("Allow"), tc.allow)
		}
	}
}

// failingRepo makes every call fail with an error whose text must never reach clients.
type failingRepo struct{ store.Repo[domain.Workflow] }

var errSecret = errors.New("dial tcp 10.9.8.7:9200: password=hunter2")

func (failingRepo) Get(context.Context, string) (domain.Workflow, store.Version, error) {
	return domain.Workflow{}, store.Version{}, errSecret
}

func TestInternalErrorsDoNotLeakDetails(t *testing.T) {
	st := memory.NewStore("soar")
	st.Workflows = failingRepo{}
	srv := newTestServer(t, st)
	resp, err := http.Get(srv.URL + Prefix + "/workflows/wf_1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 500 || strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "10.9.8.7") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var p Problem
	_ = json.Unmarshal(b, &p)
	if p.RequestID == "" || !strings.HasSuffix(p.Type, "internal") {
		t.Errorf("problem: %+v", p)
	}
}

func TestOversizedBodyIs413(t *testing.T) {
	srv := newTestServer(t, memory.NewStore("soar"))
	big := `{"title":"` + strings.Repeat("a", maxBodyBytes+10) + `","severity":3}`
	resp, err := http.Post(srv.URL+Prefix+"/alerts", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestConcurrentCreateOfSameWorkflowNameYieldsOneWinner(t *testing.T) {
	srv := newTestServer(t, memory.NewStore("soar"))
	body, _ := json.Marshal(map[string]any{"name": "contended", "definition": goodDef})
	codes := make(chan int, 12)
	for i := 0; i < 12; i++ {
		go func() {
			resp, err := http.Post(srv.URL+Prefix+"/workflows", "application/json", bytes.NewReader(body))
			if err != nil {
				codes <- -1
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	created, conflicts := 0, 0
	for i := 0; i < 12; i++ {
		switch <-codes {
		case 201:
			created++
		case 409:
			conflicts++
		}
	}
	if created != 1 || conflicts != 11 {
		t.Fatalf("want exactly one 201 and eleven 409s, got %d and %d", created, conflicts)
	}
}

// fakeQueue stands in for NATS: dead letters 6-8, replay of 8 works, anything else is unknown.
// Published messages are recorded.
type fakeQueue struct {
	mu  sync.Mutex
	pub []queue.Msg
}

func (f *fakeQueue) Publish(_ context.Context, m queue.Msg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pub = append(f.pub, m)
	return nil
}

func (f *fakeQueue) last() queue.Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pub) == 0 {
		return queue.Msg{}
	}
	return f.pub[len(f.pub)-1]
}

func (*fakeQueue) DLQList(_ context.Context, limit int, before uint64) ([]queue.DLQEntry, error) {
	all := []queue.DLQEntry{}
	for seq := uint64(8); seq >= 6; seq-- {
		all = append(all, queue.DLQEntry{Seq: seq, Subject: "dlq.events", OriginSubject: "events.wazuh-prod", Reason: "max deliveries exceeded", Deliveries: 5, StoredAt: time.Now().UTC(), Size: 100})
	}
	var out []queue.DLQEntry
	for _, e := range all {
		if before == 0 || e.Seq < before {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (*fakeQueue) DLQReplay(_ context.Context, seq uint64) error {
	if seq == 8 {
		return nil
	}
	return queue.ErrNotInDLQ
}

func (*fakeQueue) Stats(context.Context) ([]queue.StreamStats, error) {
	return []queue.StreamStats{
		{Name: "EVENTS", Messages: 3, Bytes: 900, MaxBytes: 1 << 30, Consumers: []queue.ConsumerStats{{Name: "processor", Pending: 3}}},
		{Name: "DLQ", Messages: 3, Bytes: 300},
	}, nil
}

func TestQueueEndpointsAnswer503WithoutNATS(t *testing.T) {
	srv := newTestServer(t, memory.NewStore("soar"))
	for _, tc := range [][2]string{{"GET", "/dlq"}, {"POST", "/dlq/1/replay"}, {"GET", "/queues/stats"}} {
		req, _ := http.NewRequestWithContext(context.Background(), tc[0], srv.URL+Prefix+tc[1], nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 503 {
			t.Errorf("%s %s: %d, want 503", tc[0], tc[1], resp.StatusCode)
		}
	}
}
