package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yourorg/soar/internal/store/memory"
)

func postJSON(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
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

func withTriggers(ts ...any) map[string]any {
	def := map[string]any{}
	for k, v := range goodDef {
		def[k] = v
	}
	def["triggers"] = ts
	return def
}

// Triggers written inside the definition (the spec 6 DSL form of examples/workflows)
// must end up where the processor and the cron scheduler read them: the document.
func TestTriggersInsideTheDefinitionAreLifted(t *testing.T) {
	srv := newTestServer(t, memory.NewStore("soar"))
	base := srv.URL + Prefix

	alert := map[string]any{"type": "alert", "match": `alert.severity >= 3`}
	cron := map[string]any{"type": "schedule", "cron": "@every 15m"}
	code, wf := postJSON(t, "POST", base+"/workflows", map[string]any{
		"name": "lifted", "definition": withTriggers(alert, cron),
		"triggers": []any{alert},
	}) // the same alert trigger at top level must not be doubled
	if code != 201 {
		t.Fatalf("create: %d %v", code, wf)
	}
	ts, _ := wf["triggers"].([]any)
	if len(ts) != 2 || ts[0].(map[string]any)["match"] != alert["match"] || ts[1].(map[string]any)["cron"] != "@every 15m" {
		t.Fatalf("document triggers must be the merged set, got %v", wf["triggers"])
	}
	if _, still := wf["definition"].(map[string]any)["triggers"]; still {
		t.Fatal("the stored definition must not keep a second copy of the triggers")
	}
	id := wf["id"].(string)

	// PUT replaces both: triggers only in the new definition
	code, wf = postJSON(t, "PUT", base+"/workflows/"+id, map[string]any{"name": "lifted", "definition": withTriggers(cron)})
	if code != 200 || len(wf["triggers"].([]any)) != 1 {
		t.Fatalf("update: %d %v", code, wf["triggers"])
	}
	if code, out := postJSON(t, "POST", base+"/workflows/"+id+"/publish", nil); code != 200 || out["status"] != "published" {
		t.Fatalf("publish: %d %v", code, out)
	}

	// a new version whose definition declares different triggers takes those
	code, v2 := postJSON(t, "POST", base+"/workflows/"+id+"/versions", map[string]any{"definition": withTriggers(alert)})
	if code != 201 {
		t.Fatalf("new version: %d %v", code, v2)
	}
	if ts := v2["triggers"].([]any); len(ts) != 1 || ts[0].(map[string]any)["type"] != "alert" {
		t.Fatalf("version 2 triggers: %v", v2["triggers"])
	}

	// invalid triggers in the definition are reported at their definition path
	code, prob := postJSON(t, "POST", base+"/workflows", map[string]any{
		"name":       "bad-trigger",
		"definition": withTriggers(map[string]any{"type": "alert", "match": "alert.severity >"}),
	})
	if code != 422 || !strings.Contains(toJSON(prob), "definition.triggers[0].match") {
		t.Fatalf("bad CEL in a definition trigger: %d %v", code, prob)
	}
	code, prob = postJSON(t, "POST", base+"/workflows", map[string]any{
		"name":       "typo-trigger",
		"definition": withTriggers(map[string]any{"type": "alert", "condition": "true"}),
	})
	if code != 422 || !strings.Contains(toJSON(prob), "condition") {
		t.Fatalf("an unknown trigger field must be rejected, not dropped: %d %v", code, prob)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
