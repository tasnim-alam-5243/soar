//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests talk to the real API container from `make up` (real OpenSearch behind it).
// Searches are eventually consistent (indices refresh every 1-5 s), so list checks poll.

type apiResp struct {
	Status int
	Header http.Header
	Body   map[string]any
}

func call(t *testing.T, method, path string, body any, headers ...string) apiResp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, apiURL()+"/api/v1"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := apiResp{Status: resp.StatusCode, Header: resp.Header}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func want(t *testing.T, r apiResp, status int) apiResp {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status %d, want %d: %v", r.Status, status, r.Body)
	}
	return r
}

func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()) }

var itDef = map[string]any{
	"nodes": []any{map[string]any{"id": "a", "action": "http.request"}, map[string]any{"id": "b", "action": "case.close"}},
	"edges": []any{"a -> b"},
}

func TestAPIWorkflowLifecycleOnRealStack(t *testing.T) {
	name := uniq("it-wf")
	created := want(t, call(t, "POST", "/workflows", map[string]any{"name": name, "definition": itDef}), 201)
	id := created.Body["id"].(string)
	etag := created.Header.Get("ETag")
	if etag == "" || created.Header.Get("X-Request-Id") == "" {
		t.Fatalf("missing headers: %v", created.Header)
	}

	want(t, call(t, "POST", "/workflows", map[string]any{"name": name, "definition": itDef}), 409) // name taken

	// optimistic concurrency end to end: the second writer holding the old ETag loses
	want(t, call(t, "PUT", "/workflows/"+id, map[string]any{"name": name, "description": "one", "definition": itDef}, "If-Match", etag), 200)
	lost := want(t, call(t, "PUT", "/workflows/"+id, map[string]any{"name": name, "description": "two", "definition": itDef}, "If-Match", etag), 409)
	if !strings.HasSuffix(lost.Body["type"].(string), "version-conflict") {
		t.Errorf("want version-conflict, got %v", lost.Body)
	}

	want(t, call(t, "POST", "/workflows/"+id+"/publish", nil), 200)
	want(t, call(t, "PUT", "/workflows/"+id, map[string]any{"name": name, "definition": itDef}), 409) // immutable now
	v2 := want(t, call(t, "POST", "/workflows/"+id+"/versions", map[string]any{}), 201)
	if v2.Body["version"] != float64(2) {
		t.Errorf("want version 2, got %v", v2.Body["version"])
	}

	eventually(t, "both versions listed", func() bool {
		r := call(t, "GET", "/workflows?name="+url.QueryEscape(name)+"&sort=version", nil)
		items, _ := r.Body["items"].([]any)
		return r.Status == 200 && len(items) == 2
	})
}

func TestAPIIdempotencyOnRealStack(t *testing.T) {
	key := uniq("idem")
	body := map[string]any{"title": uniq("idem-case"), "severity": 2}
	a := want(t, call(t, "POST", "/cases", body, "Idempotency-Key", key), 201)
	b := want(t, call(t, "POST", "/cases", body, "Idempotency-Key", key), 201)
	if a.Body["id"] != b.Body["id"] || b.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay must return the first case: %v vs %v", a.Body["id"], b.Body["id"])
	}
}

func TestAPICaseWorkflowOnRealStack(t *testing.T) {
	c := want(t, call(t, "POST", "/cases", map[string]any{"title": uniq("it-case"), "severity": 4, "tags": []any{"it"}}), 201)
	id := c.Body["id"].(string)
	etag := c.Header.Get("ETag")

	want(t, call(t, "POST", "/cases/"+id+"/tasks", map[string]any{"title": "block sender"}), 201)
	ip := fmt.Sprintf("203.0.113.%d", time.Now().UnixNano()%250)
	want(t, call(t, "POST", "/cases/"+id+"/observables", map[string]any{"type": "ip", "value": ip}), 201)
	again := want(t, call(t, "POST", "/cases/"+id+"/observables", map[string]any{"type": "ip", "value": ip}), 200)
	if n := len(again.Body["case_ids"].([]any)); n != 1 {
		t.Errorf("observable linked %d times", n)
	}

	closed := want(t, call(t, "PATCH", "/cases/"+id, map[string]any{"status": "closed", "verdict": "benign"}, "If-Match", etag), 200)
	if closed.Body["closed_at"] == nil {
		t.Errorf("closing must set closed_at: %v", closed.Body)
	}
	want(t, call(t, "PATCH", "/cases/"+id, map[string]any{"title": "late"}, "If-Match", etag), 409)

	eventually(t, "timeline has creation, task, observable and update events", func() bool {
		r := call(t, "GET", "/cases/"+id+"/timeline", nil)
		items, _ := r.Body["items"].([]any)
		return r.Status == 200 && len(items) >= 4
	})
	eventually(t, "case searchable by tag and status", func() bool {
		r := call(t, "GET", "/cases?tag=it&status=closed&limit=200", nil)
		items, _ := r.Body["items"].([]any)
		for _, it := range items {
			if it.(map[string]any)["id"] == id {
				return true
			}
		}
		return false
	})
	eventually(t, "observable searchable by case", func() bool {
		r := call(t, "GET", "/observables?case_id="+id, nil)
		items, _ := r.Body["items"].([]any)
		return r.Status == 200 && len(items) == 1
	})
}

func TestAPIAlertsOnRealStack(t *testing.T) {
	src := uniq("it-src")
	var ids []string
	for i := 1; i <= 5; i++ {
		a := want(t, call(t, "POST", "/alerts", map[string]any{"title": fmt.Sprint("it alert ", i), "severity": i, "source_id": src,
			"observables": []any{map[string]any{"type": "ip", "value": "198.51.100.1"}}, "ext": map[string]any{"rule": map[string]any{"id": "5715"}}}), 201)
		ids = append(ids, a.Body["id"].(string))
	}
	eventually(t, "alerts searchable (time-series refresh is 5 s)", func() bool {
		r := call(t, "GET", "/alerts?source_id="+src, nil)
		items, _ := r.Body["items"].([]any)
		return len(items) == 5
	})
	// cursor pagination across real search_after
	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ {
		path := "/alerts?source_id=" + src + "&sort=-severity&limit=2"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		r := want(t, call(t, "GET", path, nil), 200)
		for _, it := range r.Body["items"].([]any) {
			seen = append(seen, it.(map[string]any)["id"].(string))
		}
		next, _ := r.Body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 5 {
		t.Errorf("pagination returned %d alerts, want 5: %v", len(seen), seen)
	}
	one := want(t, call(t, "GET", "/alerts/"+ids[0], nil), 200)
	if one.Header.Get("ETag") == "" {
		t.Error("alert needs an ETag")
	}
	hi := want(t, call(t, "GET", "/alerts?source_id="+src+"&min_severity=4", nil), 200)
	if n := len(hi.Body["items"].([]any)); n != 2 {
		t.Errorf("min_severity=4 returned %d, want 2", n)
	}
}

func TestAPIErrorModelOnRealStack(t *testing.T) {
	nf := want(t, call(t, "GET", "/cases/cas_does_not_exist", nil), 404)
	if nf.Header.Get("Content-Type") != "application/problem+json" || nf.Body["request_id"] == "" {
		t.Errorf("problem details expected: %v %v", nf.Header, nf.Body)
	}
	v := want(t, call(t, "POST", "/cases", map[string]any{"severity": 9}), 422)
	if errs, _ := v.Body["errors"].([]any); len(errs) < 2 {
		t.Errorf("validation must list every problem: %v", v.Body)
	}
	want(t, call(t, "GET", "/nope", nil), 404)
	want(t, call(t, "GET", "/alerts?cursor=garbage", nil), 400)
	want(t, call(t, "POST", "/workflows/wf_x/run", nil), 501)
	spec, err := http.Get(apiURL() + "/api/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec.Body.Close()
	if spec.StatusCode != 200 {
		t.Errorf("openapi.yaml: %d", spec.StatusCode)
	}
}

func TestAPIConcurrentTaskCreationOnRealStack(t *testing.T) {
	c := want(t, call(t, "POST", "/cases", map[string]any{"title": uniq("it-conc")}), 201)
	id := c.Body["id"].(string)
	var wg sync.WaitGroup
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := call(t, "POST", "/cases/"+id+"/tasks", map[string]any{"title": fmt.Sprint("task ", i)})
			if r.Status != 201 {
				t.Errorf("task %d: %d %v", i, r.Status, r.Body)
			}
		}(i)
	}
	wg.Wait()
	eventually(t, "all 15 tasks stored", func() bool {
		r := call(t, "GET", "/cases/"+id+"/tasks?limit=200", nil)
		items, _ := r.Body["items"].([]any)
		return len(items) == 15
	})
}
