package integrations

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/ingest"
	"github.com/yourorg/soar/internal/ingest/pack"
)

var update = flag.Bool("update", false, "rewrite the .expected.json golden files")

type lists map[string][]string

func (l lists) Contains(name, v string) bool {
	for _, x := range l[name] {
		if x == v {
			return true
		}
	}
	return false
}

func pipeline(t *testing.T, l lists, src string) *ingest.Pipeline {
	t.Helper()
	b, err := pack.Load(Packs, "wazuh")
	if err != nil {
		t.Fatal(err)
	}
	var source domain.Source
	for _, s := range b.Sources {
		if s.Name == src {
			source = s
		}
	}
	p, err := ingest.BuildPipeline(source, l, ingest.Resolver{
		Mappings: func(n string, v int) (domain.Mapping, error) {
			for _, m := range b.Mappings {
				if m.Name == n && m.Version == v {
					return m, nil
				}
			}
			return domain.Mapping{}, fmt.Errorf("not in pack")
		},
		Filters: func(n string, v int) (domain.FilterSet, error) {
			for _, f := range b.Filters {
				if f.Name == n && f.Version == v {
					return f, nil
				}
			}
			return domain.FilterSet{}, fmt.Errorf("not in pack")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// normalized is the part of an event that must be stable across runs.
func normalized(ev domain.Event) map[string]any {
	a := ev.Alert
	m := map[string]any{
		"external_id": a.ExternalID, "title": a.Title, "description": a.Description, "severity": a.Severity,
		"original_severity": a.OriginalSeverity, "tags": a.Tags, "observables": a.Observables, "entities": a.Entities,
		"mitre": a.Mitre, "fingerprint": a.Fingerprint, "kind": a.Kind, "source_id": a.SourceID, "system": a.System,
		"dedup_window_sec": ev.DedupWindowSec, "route": ev.Route,
	}
	if a.OccurredAt != nil {
		m["occurred_at"] = a.OccurredAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func loadFixture(t *testing.T, file string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("wazuh", "testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if src, ok := m["_source"].(map[string]any); ok { // an indexer hit: the poller hands over _source
		return src
	}
	return m
}

func TestWazuhGoldenFixtures(t *testing.T) {
	p := pipeline(t, lists{}, "wazuh-prod")
	files, _ := filepath.Glob("wazuh/testdata/*.json")
	files = filterOut(files, ".expected.json")
	if len(files) != 6 {
		t.Fatalf("want 6 fixtures (five manager alerts and one indexer hit), found %d", len(files))
	}
	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			out := p.ProcessRecords([]map[string]any{loadFixture(t, name)})
			if len(out.Failures) > 0 || len(out.Events) != 1 {
				t.Fatalf("events=%d filtered=%d failures=%+v", len(out.Events), out.Filtered, out.Failures)
			}
			ev := out.Events[0]
			if !reflect.DeepEqual(ev.Alert.Ext, loadFixture(t, name)) {
				t.Error("ext must keep the complete raw record")
			}
			got, _ := json.MarshalIndent(normalized(ev), "", "  ")
			golden := strings.TrimSuffix(f, ".json") + ".expected.json"
			if *update {
				if err := os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("no golden file (run go test ./integrations -update): %v", err)
			}
			if strings.TrimSpace(string(want)) != strings.TrimSpace(string(got)) {
				t.Errorf("normalized output changed.\n--- want\n%s\n--- got\n%s", want, got)
			}
		})
	}
}

// The expectations below are the table in spec 10.1, independent of the golden snapshots.
func TestWazuhSpecTable(t *testing.T) {
	p := pipeline(t, lists{}, "wazuh-prod")
	type obs = [2]string
	cases := []struct {
		file     string
		sev      int
		orig     string
		observed []obs
		mitre    []string
	}{
		{"01_fim_added_make_dummy_cert.json", 2, "5", []obs{{"host", "web01"}, {"file_path", "/usr/bin/make-dummy-cert"}, {"hash", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"}, {"hash", "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"}, {"hash", "5d41402abc4b2a76b9719d911017c592"}}, nil},
		{"02_fim_added_npx_symlink_empty_hashes.json", 2, "5", []obs{{"host", "web01"}, {"file_path", "/usr/bin/npx-22"}}, nil}, // empty-input hashes are skipped
		{"03_systemd_service_failure.json", 2, "5", []obs{{"host", "web01"}}, nil},
		{"04_pam_session_opened.json", 1, "3", []obs{{"host", "web01"}, {"user", "root"}}, []string{"T1078"}},
		{"05_sshd_auth_success.json", 1, "3", []obs{{"host", "web01"}, {"ip", "10.10.192.1"}, {"user", "root"}}, []string{"T1078", "T1021"}},
		{"06_indexer_hit_fim_modified.json", 3, "7", []obs{{"host", "web01"}, {"file_path", "/etc/ld.so.cache"}, {"hash", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"}, {"hash", "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"}, {"hash", "5d41402abc4b2a76b9719d911017c592"}}, []string{"T1565.001"}},
	}
	for _, c := range cases {
		out := p.ProcessRecords([]map[string]any{loadFixture(t, c.file)})
		if len(out.Events) != 1 {
			t.Errorf("%s: not kept (filtered=%d failures=%+v): the default filter must keep levels 3 and above", c.file, out.Filtered, out.Failures)
			continue
		}
		a := out.Events[0].Alert
		if a.Severity != c.sev || a.OriginalSeverity != c.orig {
			t.Errorf("%s: severity %d (%s), want %d (%s)", c.file, a.Severity, a.OriginalSeverity, c.sev, c.orig)
		}
		var got []obs
		for _, o := range a.Observables {
			got = append(got, obs{o.Type, o.Value})
		}
		sortObs(got)
		sortObs(c.observed)
		if !reflect.DeepEqual(got, c.observed) {
			t.Errorf("%s: observables %v, want %v", c.file, got, c.observed)
		}
		var ids []string
		if a.Mitre != nil {
			ids = a.Mitre.TechniqueIDs
		}
		if !reflect.DeepEqual(ids, c.mitre) {
			t.Errorf("%s: MITRE ids %v, want %v", c.file, ids, c.mitre)
		}
	}
	// the two PAM/sshd samples must not be hidden by the level filter (spec: rule 5715 is level 3)
	if a := p.ProcessRecords([]map[string]any{loadFixture(t, "05_sshd_auth_success.json")}); len(a.Events) != 1 {
		t.Error("rule 5715 (level 3) must be kept")
	}
	// tactics are not paired with techniques: they stay a separate list
	out := p.ProcessRecords([]map[string]any{loadFixture(t, "05_sshd_auth_success.json")})
	if m := out.Events[0].Alert.Mitre; len(m.Tactics) != 5 || len(m.Techniques) != 2 {
		t.Errorf("mitre lists must stay separate: %+v", m)
	}
	// push and pull deduplicate through the same external id and system
	push := p.ProcessRecords([]map[string]any{loadFixture(t, "05_sshd_auth_success.json")}).Events[0].Alert
	pull := pipeline(t, lists{}, "wazuh-pull").ProcessRecords([]map[string]any{loadFixture(t, "05_sshd_auth_success.json")}).Events[0].Alert
	if push.ExternalID == "" || push.ExternalID != pull.ExternalID || push.System != pull.System || push.Fingerprint != pull.Fingerprint {
		t.Errorf("push and pull must agree: %+v vs %+v", push, pull)
	}
}

func TestWazuhFilters(t *testing.T) {
	rec := func(level int, id, desc, decoder string) map[string]any {
		return map[string]any{
			"id": "x." + id, "timestamp": "2026-10-29T12:48:31.597+0600", "rule": map[string]any{"level": float64(level), "id": id, "description": desc},
			"agent": map[string]any{"id": "001", "name": "h"}, "decoder": map[string]any{"name": decoder},
		}
	}
	p := pipeline(t, lists{"wazuh_noisy_rule_ids": {"1002"}, "ignored_decoders": {"noisy-decoder"}}, "wazuh-prod")
	cases := []struct {
		name string
		rec  map[string]any
		keep bool
		tag  string
	}{
		{"level 2 is below the floor", rec(2, "100", "x", "d"), false, ""},
		{"level 3 is kept", rec(3, "100", "x", "d"), true, ""},
		{"noisy rule id from the managed list", rec(12, "1002", "x", "d"), false, ""},
		{"rule id compare is text based", rec(12, "1003", "x", "d"), true, ""},
		{"ignored decoder", rec(12, "100", "x", "noisy-decoder"), false, ""},
		{"ransomware below the floor is kept and tagged", rec(1, "100", "Ransomware behaviour detected", "d"), true, "priority-pattern"},
		{"brute force wording", rec(5, "100", "Multiple brute-force attempts", "d"), true, "priority-pattern"},
	}
	for _, c := range cases {
		out := p.ProcessRecords([]map[string]any{c.rec})
		if c.keep != (len(out.Events) == 1) || len(out.Failures) > 0 {
			t.Errorf("%s: events=%d filtered=%d failures=%v", c.name, len(out.Events), out.Filtered, out.Failures)
			continue
		}
		if c.tag != "" && !contains(out.Events[0].Alert.Tags, c.tag) {
			t.Errorf("%s: missing tag %q in %v", c.name, c.tag, out.Events[0].Alert.Tags)
		}
	}
	if counts := p.Stats.RuleCounts(); counts["wazuh-default/min-level|drop"] != 1 || counts["wazuh-default/drop-noisy-rule-ids|drop"] != 1 {
		t.Errorf("filter drops must be counted per rule, never silent: %v", counts)
	}
}

func TestWazuhRecordsThatCannotBeMappedAreFailuresNotDrops(t *testing.T) {
	p := pipeline(t, lists{}, "wazuh-prod")
	out := p.ProcessRecords([]map[string]any{{"id": "1", "rule": map[string]any{"level": float64(5), "id": "1", "description": "d"}}}) // no timestamp, no agent
	if len(out.Events) != 0 || len(out.Failures) != 1 || out.Failures[0].Stage != "map" || !strings.Contains(out.Failures[0].Err, "occurred_at") {
		t.Fatalf("a record missing required fields must be reported with the failing expression: %+v", out)
	}
}

func TestPackInstallsAndMatchesTheDocumentedFiles(t *testing.T) {
	b, err := pack.Load(Packs, "wazuh")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Mappings) != 1 || len(b.Filters) != 1 || len(b.Sources) != 2 || len(b.Lists) != 2 {
		t.Fatalf("pack content: %d mappings %d filters %d sources %d lists", len(b.Mappings), len(b.Filters), len(b.Sources), len(b.Lists))
	}
	for _, s := range b.Sources {
		if s.System != "wazuh-prod" {
			t.Errorf("%s: push and pull must share system wazuh-prod", s.Name)
		}
	}
}

func sortObs(o [][2]string) {
	sort.Slice(o, func(i, j int) bool { return o[i][0]+o[i][1] < o[j][0]+o[j][1] })
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func filterOut(files []string, suffix string) []string {
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, suffix) {
			out = append(out, f)
		}
	}
	return out
}
