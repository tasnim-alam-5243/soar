package domain

import "testing"

func TestSplitAndMergeTriggers(t *testing.T) {
	def := JSON{"nodes": []any{}, "triggers": []any{
		map[string]any{"type": "alert", "match": "alert.severity >= 3"},
		map[string]any{"type": "schedule", "cron": "@every 15m"},
	}}
	rest, ts, declared, err := SplitTriggers(def)
	if err != nil || !declared || len(ts) != 2 || ts[1].Cron != "@every 15m" {
		t.Fatalf("split: %v %v %v", ts, declared, err)
	}
	if _, ok := rest["triggers"]; ok {
		t.Fatal("rest must not carry the triggers")
	}
	if _, ok := def["triggers"]; !ok {
		t.Fatal("the input definition must not be modified")
	}
	if _, _, declared, _ := SplitTriggers(JSON{"nodes": []any{}}); declared {
		t.Fatal("no triggers key: not declared")
	}
	if _, _, _, err := SplitTriggers(JSON{"triggers": []any{map[string]any{"type": "alert", "condition": "x"}}}); err == nil {
		t.Fatal("an unknown trigger field must be an error")
	}

	a := []Trigger{{Type: "alert", Match: "x"}}
	if got := MergeTriggers(a, []Trigger{{Type: "alert", Match: "x"}, {Type: "schedule", Cron: "@daily"}}); len(got) != 2 || got[1].Cron != "@daily" {
		t.Fatalf("merge must drop duplicates and keep order: %v", got)
	}

	w := Workflow{Definition: def, Triggers: []Trigger{{Type: "alert", Match: "alert.severity >= 3"}, {Type: "manual"}}}
	if all := w.AllTriggers(); len(all) != 3 {
		t.Fatalf("AllTriggers = document + definition without duplicates, got %v", all)
	}
}
