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
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/memory"
)

func newTestServer(t *testing.T, st *store.Store) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	New(st, log).Mount(mux)
	srv := httptest.NewServer(obs.Middleware(log, mux))
	t.Cleanup(srv.Close)
	return srv
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
	req, _ := http.NewRequestWithContext(context.Background(), method, h.url+Prefix+path, rd)
	if body != nil {
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
		map[string]any{"id": "check", "action": "http.request"},
		map[string]any{"id": "decide", "type": "branch", "cases": []any{map[string]any{"when": "x > 1", "next": []any{"block"}}}, "default": []any{"close"}},
		map[string]any{"id": "block", "action": "connector.firewall.block_url"},
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
	srv := newTestServer(t, st)
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

		body := map[string]any{"name": "phishing_triage", "description": "d", "definition": goodDef,
			"triggers": []any{map[string]any{"type": "alert", "match": `alert.severity >= 3`}}}
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

		run, _ := h.do("POST", "/workflows/"+id+"/run", nil, 501)
		if !strings.HasSuffix(sa(run, "type"), "not-implemented") {
			t.Errorf("run: %v", run)
		}
	})

	t.Run("executions", func(t *testing.T) {
		h.t = t
		now := time.Now().UTC()
		ex := domain.Execution{Meta: domain.NewMeta(domain.PrefixExecution), WorkflowID: "wf_1", WorkflowVersion: 1, Status: "running",
			Trigger: domain.ExecutionTrigger{Type: "alert", AlertID: "alt_9"}, StartedAt: &now, Partition: 3}
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
		all, _ := h.do("GET", "/executions?sort=-started_at", nil, 200)
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
		h.do("POST", "/executions/"+ex.ID+"/cancel", nil, 501)
	})

	t.Run("alerts", func(t *testing.T) {
		h.t = t
		h.do("POST", "/alerts", map[string]any{"title": "x", "severity": 9}, 422, reqOpt{loose: true})
		h.do("POST", "/alerts", map[string]any{"severity": 3}, 422, reqOpt{loose: true})
		h.do("POST", "/alerts", map[string]any{"title": "x", "severity": 3, "observables": []any{map[string]any{"type": "carrier-pigeon", "value": "x"}}}, 422, reqOpt{loose: true})
		var ids []string
		for i, sev := range []int{2, 4, 5} {
			a, hdr := h.do("POST", "/alerts", map[string]any{"title": "alert " + strconv.Itoa(i), "severity": sev, "tags": []any{"t"},
				"observables": []any{map[string]any{"type": "ip", "value": "10.0.0." + strconv.Itoa(i), "role": "source"}},
				"ext":         map[string]any{"rule": map[string]any{"id": "5715"}}}, 201)
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
		h.do("POST", "/connections", map[string]any{"connector_id": "iris", "name": "iris-prod", "auth_type": "bearer",
			"secret_refs": map[string]any{"token": "a literal secret with spaces!"}}, 422, reqOpt{loose: true})
		h.do("POST", "/connections", map[string]any{"connector_id": "iris", "name": "x", "auth_type": "telepathy"}, 422, reqOpt{loose: true})
		c, hdr := h.do("POST", "/connections", map[string]any{"connector_id": "iris", "name": "iris-prod", "auth_type": "bearer",
			"secret_refs": map[string]any{"token": "iris_service_token"}, "config": map[string]any{"base_url": "https://iris.example"}}, 201)
		id := sa(c, "id")
		if hdr.Get("Location") != Prefix+"/connections/"+id {
			t.Errorf("location: %v", hdr)
		}
		list, _ := h.do("GET", "/connections?connector_id=iris&sort=name", nil, 200)
		if len(list["items"].([]any)) != 1 {
			t.Errorf("list: %v", list)
		}
		h.do("GET", "/connections/"+id, nil, 200)
		h.do("DELETE", "/connections/"+id, nil, 204)
		h.do("GET", "/connections/"+id, nil, 404)
		h.do("DELETE", "/connections/"+id, nil, 404)
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
