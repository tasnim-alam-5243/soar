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
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests need the full stack from `make up` (ingest, processor, NATS, OpenSearch, seeded Wazuh pack).

func ingestURL() string {
	if v := os.Getenv("SOAR_INGEST_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:8081"
}

func ingestToken() string {
	if v := os.Getenv("SOAR_SOURCE_TOKEN_WAZUH_PROD"); v != "" {
		return v
	}
	return "dev-wazuh-token-change-me"
}

func ingestPost(t *testing.T, body any, headers ...string) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ingestURL()+"/ingest/wazuh-prod", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ingestToken())
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func wazuhAlert(id, title, host string, level int, extra map[string]any) map[string]any {
	a := map[string]any{
		"timestamp": time.Now().Format("2006-01-02T15:04:05.000-0700"), "id": id,
		"rule":    map[string]any{"level": level, "id": "5715", "description": title, "groups": []string{"syslog", "sshd"}, "mitre": map[string]any{"id": []string{"T1078"}, "tactic": []string{"Initial Access"}, "technique": []string{"Valid Accounts"}}},
		"agent":   map[string]any{"id": host, "name": host}, // the dedup key includes agent.id: one agent per test keeps their alerts apart
		"decoder": map[string]any{"name": "sshd"},
		"data":    map[string]any{"srcip": "10.10.192.1", "dstuser": "root"},
	}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

func eventuallyFor(t *testing.T, d time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", d, what)
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// findAlert looks an alert up by its (unique) title.
func findAlert(t *testing.T, title string) map[string]any {
	t.Helper()
	r := call(t, "GET", "/alerts?source_id=wazuh-prod&sort=-received_at&limit=200", nil)
	items, _ := r.Body["items"].([]any)
	for _, it := range items {
		m := it.(map[string]any)
		if m["title"] == title {
			return m
		}
	}
	return nil
}

func TestIngestAlertBecomesAnAlertWithObservables(t *testing.T) {
	title := uniq("it ssh success")
	host := uniq("it-host")
	code, body := ingestPost(t, wazuhAlert(uniq("it.id"), title, host, 3, nil))
	if code != 202 || len(body["event_ids"].([]any)) != 1 {
		t.Fatalf("ingest: %d %v", code, body)
	}
	var alert map[string]any
	eventuallyFor(t, 30*time.Second, "alert searchable", func() bool { alert = findAlert(t, title); return alert != nil })
	if alert["severity"] != float64(1) || alert["original_severity"] != "3" || alert["occurrence_count"] != float64(1) ||
		alert["fingerprint"] == "" || alert["external_id"] == "" || alert["source_id"] != "wazuh-prod" || alert["system"] != "wazuh-prod" {
		t.Errorf("alert fields: %v", alert)
	}
	if m, _ := alert["mitre"].(map[string]any); m == nil || m["technique_ids"].([]any)[0] != "T1078" {
		t.Errorf("mitre: %v", alert["mitre"])
	}
	if ext, _ := alert["ext"].(map[string]any); ext == nil || ext["decoder"] == nil {
		t.Errorf("vendor fields must be kept under ext: %v", alert["ext"])
	}
	eventuallyFor(t, 30*time.Second, "host observable created", func() bool {
		r := call(t, "GET", "/observables?type=host&value="+url.QueryEscape(host), nil)
		items, _ := r.Body["items"].([]any)
		return len(items) == 1
	})
}

func TestRepeatsOfTheSameAlertAreGroupedAndSeverityRises(t *testing.T) {
	title := uniq("it repeated")
	host := uniq("it-rep")
	for i := 0; i < 3; i++ {
		level := 3
		if i == 2 {
			level = 10 // a later, worse occurrence raises the group's severity
		}
		if code, _ := ingestPost(t, wazuhAlert(uniq(fmt.Sprint("it.rep", i)), title, host, level, nil)); code != 202 {
			t.Fatal(code)
		}
	}
	eventuallyFor(t, 30*time.Second, "three occurrences in one alert", func() bool {
		a := findAlert(t, title)
		return a != nil && a["occurrence_count"] == float64(3)
	})
	if a := findAlert(t, title); a["severity"] != float64(4) {
		t.Errorf("severity must rise to the worst occurrence (level 10 -> 4): %v", a["severity"])
	}
	n := 0
	r := call(t, "GET", "/alerts?source_id=wazuh-prod&limit=200", nil)
	for _, it := range r.Body["items"].([]any) {
		if it.(map[string]any)["title"] == title {
			n++
		}
	}
	if n != 1 {
		t.Errorf("repeats must collapse into one alert document, found %d", n)
	}
}

func TestSameAlertArrivingTwiceIsCountedOnce(t *testing.T) {
	title := uniq("it dup")
	host := uniq("it-dup")
	a := wazuhAlert(uniq("it.dup"), title, host, 3, nil)
	// distinct Idempotency-Keys defeat the queue's own deduplication, so the second copy
	// reaches the processor and only the duplicate guard (soar-seen) can stop it
	for _, key := range []string{uniq("k1"), uniq("k2")} {
		if code, _ := ingestPost(t, a, "Idempotency-Key", key); code != 202 {
			t.Fatal(code)
		}
	}
	eventuallyFor(t, 30*time.Second, "alert stored", func() bool { return findAlert(t, title) != nil })
	time.Sleep(4 * time.Second) // give a wrongly counted second copy time to show up
	if a := findAlert(t, title); a["occurrence_count"] != float64(1) {
		t.Errorf("the same Wazuh alert id arriving twice must be stored once, count=%v", a["occurrence_count"])
	}
}

func TestFilteredAndUnmappableRecords(t *testing.T) {
	low := uniq("it low")
	code, body := ingestPost(t, wazuhAlert(uniq("it.low"), low, "it-low", 1, nil))
	if code != 202 || body["filtered"] != float64(1) || len(body["event_ids"].([]any)) != 0 {
		t.Fatalf("level 1 is below the floor: %d %v", code, body)
	}
	broken := wazuhAlert(uniq("it.bad"), uniq("it broken"), "it-bad", 5, nil)
	delete(broken, "timestamp")
	code, body = ingestPost(t, broken)
	if code != 202 || body["failed"] != float64(1) {
		t.Fatalf("an unmappable record is accepted for handling, not dropped: %d %v", code, body)
	}
	eventuallyFor(t, 15*time.Second, "unmappable record in the dead-letter queue", func() bool {
		r := call(t, "GET", "/dlq?limit=50", nil)
		items, _ := r.Body["items"].([]any)
		for _, it := range items {
			m := it.(map[string]any)
			if m["origin_subject"] == "events.wazuh-prod" && strings.Contains(fmt.Sprint(m["reason"]), "map") {
				return true
			}
		}
		return false
	})
	time.Sleep(1500 * time.Millisecond)
	if findAlert(t, low) != nil {
		t.Error("a filtered alert must not be stored")
	}
}

func TestRejectedRequests(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ingestURL()+"/ingest/wazuh-prod", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, ingestURL()+"/ingest/nosuch", strings.NewReader(`{}`))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown source: %d", resp.StatusCode)
	}
}

func TestPublishedWorkflowIsTriggeredByAnIngestedAlert(t *testing.T) {
	title := uniq("it trigger")
	wf := call(t, "POST", "/workflows", map[string]any{
		"name": uniq("it-trigger-wf"), "definition": itDef,
		"triggers": []any{map[string]any{"type": "alert", "match": fmt.Sprintf(`alert.title == %q`, title)}},
	})
	want(t, wf, 201)
	wfID := wf.Body["id"].(string)
	want(t, call(t, "POST", "/workflows/"+wfID+"/publish", nil), 200)
	time.Sleep(11 * time.Second) // the processor reloads workflow triggers every 10 s

	if code, _ := ingestPost(t, wazuhAlert(uniq("it.trg"), title, uniq("it-trg"), 5, nil)); code != 202 {
		t.Fatal(code)
	}
	var alertID string
	eventuallyFor(t, 30*time.Second, "alert stored", func() bool {
		a := findAlert(t, title)
		if a != nil {
			alertID = a["id"].(string)
		}
		return a != nil
	})
	var exeID string
	eventuallyFor(t, 30*time.Second, "execution created for the alert", func() bool {
		r := call(t, "GET", "/executions?alert_id="+alertID, nil)
		items, _ := r.Body["items"].([]any)
		if len(items) != 1 {
			return false
		}
		e := items[0].(map[string]any)
		exeID, _ = e["id"].(string)
		return e["workflow_id"] == wfID
	})
	// Steps 6-7: the orchestrator runs it and the worker executes both actions.
	var exe apiResp
	eventuallyFor(t, 60*time.Second, "execution finished", func() bool {
		exe = call(t, "GET", "/executions/"+exeID, nil)
		switch exe.Body["status"] {
		case "succeeded", "failed", "cancelled", "timed_out":
			return true
		}
		return false
	})
	if exe.Body["status"] != "succeeded" {
		t.Fatalf("execution %s ended %v: %v", exeID, exe.Body["status"], exe.Body)
	}
	// step runs are time-series documents: searchable after the next refresh (5 s)
	done := map[string]string{}
	eventuallyFor(t, 15*time.Second, "both steps recorded as succeeded", func() bool {
		steps := want(t, call(t, "GET", "/executions/"+exeID+"/steps", nil), 200)
		items, _ := steps.Body["items"].([]any)
		done = map[string]string{}
		for _, it := range items {
			s := it.(map[string]any)
			done[fmt.Sprint(s["node_id"])] = fmt.Sprint(s["status"])
		}
		return len(items) == 2 && done["a"] == "succeeded" && done["b"] == "succeeded"
	})
}

// The spec 6 DSL form, used by examples/workflows: triggers written inside the
// definition. The API lifts them into the workflow document, where the processor reads them.
func TestTriggerInsideTheDefinitionFires(t *testing.T) {
	title := uniq("it def trigger")
	def := map[string]any{
		"triggers": []any{map[string]any{"type": "alert", "match": fmt.Sprintf(`alert.title == %q`, title)}},
		"nodes":    []any{map[string]any{"id": "mark", "type": "set", "vars": map[string]any{"seen": "alert.title"}}},
	}
	wf := want(t, call(t, "POST", "/workflows", map[string]any{"name": uniq("it-def-trigger"), "definition": def}), 201)
	if ts, _ := wf.Body["triggers"].([]any); len(ts) != 1 {
		t.Fatalf("the definition's trigger must be stored on the workflow: %v", wf.Body["triggers"])
	}
	wfID := wf.Body["id"].(string)
	want(t, call(t, "POST", "/workflows/"+wfID+"/publish", nil), 200)
	time.Sleep(11 * time.Second) // the processor reloads workflow triggers every 10 s

	if code, _ := ingestPost(t, wazuhAlert(uniq("it.deftrg"), title, uniq("it-deftrg"), 5, nil)); code != 202 {
		t.Fatal(code)
	}
	eventuallyFor(t, 45*time.Second, "an execution of the workflow, finished", func() bool {
		r := call(t, "GET", "/executions?workflow_id="+wfID, nil)
		items, _ := r.Body["items"].([]any)
		return len(items) == 1 && items[0].(map[string]any)["status"] == "succeeded"
	})
}

func TestManagedListChangesTakeEffectWithoutADeploy(t *testing.T) {
	rule := fmt.Sprint(70000 + time.Now().UnixNano()%9000)
	want(t, call(t, "PUT", "/lists/wazuh_noisy_rule_ids", map[string]any{"items": []any{rule}}), 200)
	t.Cleanup(func() { call(t, "PUT", "/lists/wazuh_noisy_rule_ids", map[string]any{"items": []any{}}) })

	alert := wazuhAlert(uniq("it.noisy"), uniq("it noisy"), "it-noisy", 5, nil)
	alert["rule"].(map[string]any)["id"] = rule
	tr := want(t, call(t, "POST", "/filters/test", map[string]any{"filter_set": "wazuh-default@1", "sample": alert}), 200)
	if tr.Body["drop"] != true || tr.Body["rule"] != "drop-noisy-rule-ids" {
		t.Fatalf("filter test must show the drop immediately: %v", tr.Body)
	}
	eventuallyFor(t, 40*time.Second, "ingest picks up the edited list (reload every 10 s)", func() bool {
		a := wazuhAlert(uniq("it.noisy2"), uniq("it noisy"), "it-noisy", 5, nil)
		a["rule"].(map[string]any)["id"] = rule
		code, body := ingestPost(t, a)
		return code == 202 && body["filtered"] == float64(1)
	})
}

func TestSourceTestEndpointDoesNotPublish(t *testing.T) {
	r := call(t, "GET", "/sources?adapter=webhook", nil)
	var id string
	for _, it := range r.Body["items"].([]any) {
		if it.(map[string]any)["name"] == "wazuh-prod" {
			id = it.(map[string]any)["id"].(string)
		}
	}
	if id == "" {
		t.Fatal("wazuh-prod is not seeded: run `make seed`")
	}
	title := uniq("it dry run")
	out := want(t, call(t, "POST", "/sources/"+id+"/test", map[string]any{"payload": wazuhAlert(uniq("it.dry"), title, "it-dry", 5, nil)}), 200)
	if len(out.Body["events"].([]any)) != 1 {
		t.Fatalf("dry run: %v", out.Body)
	}
	time.Sleep(7 * time.Second)
	if findAlert(t, title) != nil {
		t.Error("a test run must never store or publish anything")
	}
}

func TestQueueStatsAreAvailable(t *testing.T) {
	r := want(t, call(t, "GET", "/queues/stats", nil), 200)
	streams, _ := r.Body["streams"].([]any)
	names := map[string]bool{}
	for _, s := range streams {
		names[s.(map[string]any)["name"].(string)] = true
	}
	if !names["EVENTS"] || !names["DLQ"] {
		t.Errorf("streams: %v", r.Body)
	}
}

// A short version of the Step 5 exit gate: 3,000 unique events at 1,000/s, then prove in
// OpenSearch that every acknowledged event is stored exactly once. `make loadtest` runs the real thing.
func TestShortLoadHasZeroLoss(t *testing.T) {
	const rate, seconds, groups = 1000, 3, 50
	total := rate * seconds
	run := fmt.Sprintf("it%d", time.Now().UnixNano()%1_000_000)
	var next, accepted atomic.Int64
	var wg sync.WaitGroup
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}, Timeout: 30 * time.Second}
	start := time.Now()
	for w := 0; w < 64; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n := int(next.Add(1) - 1)
				if n >= total {
					return
				}
				if d := time.Until(start.Add(time.Duration(n) * time.Second / rate)); d > 0 {
					time.Sleep(d)
				}
				g := n % groups
				b, _ := json.Marshal(wazuhAlert(fmt.Sprintf("load.%s.%d", run, n), "it load", "load-"+run, 5, map[string]any{"rule": map[string]any{"level": 5, "id": fmt.Sprint(9000 + g), "description": "it load"}}))
				req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ingestURL()+"/ingest/wazuh-prod", bytes.NewReader(b))
				req.Header.Set("Authorization", "Bearer "+ingestToken())
				resp, err := client.Do(req)
				if err != nil {
					continue
				}
				resp.Body.Close()
				if resp.StatusCode == 202 {
					accepted.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != int64(total) {
		t.Fatalf("accepted %d of %d (no 5xx or 429 expected at 1,000/s)", accepted.Load(), total)
	}
	oc := osClient()
	eventuallyFor(t, 90*time.Second, "all events stored exactly once", func() bool {
		ctx := context.Background()
		for _, idx := range []string{index("alerts"), index("seen")} {
			_, _, _ = oc.Do(ctx, http.MethodPost, "/"+idx+"/_refresh", nil)
		}
		_, out, err := oc.Do(ctx, http.MethodPost, "/"+index("alerts")+"/_search", map[string]any{
			"size":  0,
			"query": map[string]any{"term": map[string]any{"entities.hosts": "load-" + run}},
			"aggs":  map[string]any{"occ": map[string]any{"sum": map[string]any{"field": "occurrence_count"}}},
		})
		if err != nil {
			return false
		}
		var a struct {
			Aggregations struct {
				Occ struct{ Value float64 } `json:"occ"`
			} `json:"aggregations"`
		}
		_ = json.Unmarshal(out, &a)
		_, out, err = oc.Do(ctx, http.MethodPost, "/"+index("seen")+"/_count", map[string]any{"query": map[string]any{"prefix": map[string]any{"key": "wazuh-prod:load." + run + "."}}})
		if err != nil {
			return false
		}
		var s struct{ Count int }
		_ = json.Unmarshal(out, &s)
		if int(a.Aggregations.Occ.Value) > total || s.Count > total {
			t.Fatalf("more stored than sent: occurrences=%v seen=%d sent=%d", a.Aggregations.Occ.Value, s.Count, total)
		}
		return int(a.Aggregations.Occ.Value) == total && s.Count == total
	})
}
