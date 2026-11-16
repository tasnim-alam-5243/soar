package integrations

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/ingest"
	"github.com/yourorg/soar/internal/ingest/pack"
)

func TestOutlookReportedEmailMapping(t *testing.T) {
	b, err := pack.Load(Packs, "outlook")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Sources) != 1 || b.Sources[0].Adapter != "graph_mail" || b.Sources[0].Enabled {
		t.Fatalf("outlook pack sources: %+v", b.Sources)
	}
	p, err := ingest.BuildPipeline(b.Sources[0], lists{}, ingest.Resolver{
		Mappings: func(n string, v int) (domain.Mapping, error) {
			for _, m := range b.Mappings {
				if m.Name == n && m.Version == v {
					return m, nil
				}
			}
			return domain.Mapping{}, fmt.Errorf("not in pack")
		},
		Filters: func(string, int) (domain.FilterSet, error) { return domain.FilterSet{}, fmt.Errorf("none") },
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile("outlook/testdata/reported_email.json")
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	out := p.ProcessRecords([]map[string]any{rec})
	if len(out.Failures) > 0 || len(out.Events) != 1 {
		t.Fatalf("events=%d failures=%+v", len(out.Events), out.Failures)
	}
	a := out.Events[0].Alert
	if a.Title != "Reported email: Your mailbox is full" || a.ExternalID != "AAMkAGI2TG93AAA=" || a.Severity != 3 {
		t.Errorf("alert: %+v", a)
	}
	want := map[string]bool{"email:it-support@evil.example": false, "url:https://evil.example/login?u=bob": false, "ip:203.0.113.7": false}
	for _, o := range a.Observables {
		if _, ok := want[o.Type+":"+o.Value]; ok {
			want[o.Type+":"+o.Value] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing observable %s in %+v", k, a.Observables)
		}
	}
}

// Every integration manifest must load and pass the action-manifest rules (spec 10:
// description, side_effect, risk and supports_dry_run are required).
func TestIntegrationManifests(t *testing.T) {
	pkgs, err := actions.LoadPackages(Packs, ".")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]actions.Package{}
	for _, p := range pkgs {
		byName[p.Name] = p
	}
	for _, name := range []string{"wazuh", "outlook", "dfir_iris"} {
		p, ok := byName[name]
		if !ok {
			t.Fatalf("package %s not embedded", name)
		}
		hasTest := false
		for _, a := range p.Actions {
			if a.ID == name+".connection.test" || a.ID == "iris.connection.test" {
				hasTest = true
			}
			if a.SideEffect == "destructive" && !a.SupportsDryRun {
				t.Errorf("%s: destructive actions must support dry runs", a.ID)
			}
			if _, err := os.Stat(name + "/" + entryFile(a.Entrypoint)); err != nil {
				t.Errorf("%s: entrypoint file missing: %v", a.ID, err)
			}
		}
		if !hasTest {
			t.Errorf("%s has no connection test action", name)
		}
	}
	reg := actions.NewRegistry()
	if err := actions.RegisterPackages(reg, pkgs, nil); err != nil {
		t.Fatal(err)
	}
	send, ok := reg.Get("outlook.mail.send")
	if !ok || !send.Manifest().AtMostOnce || send.Manifest().ApprovalPolicy["recipients_over"] != float64(50) {
		t.Errorf("outlook.mail.send must be at-most-once with a 50-recipient approval threshold: %+v", send.Manifest())
	}
	if del, _ := reg.Get("outlook.mail.purge"); del.Manifest().SideEffect != "destructive" {
		t.Error("purge must be destructive")
	}
}

func entryFile(ep string) string {
	for i := 0; i < len(ep); i++ {
		if ep[i] == ':' {
			return ep[:i]
		}
	}
	return ep
}
