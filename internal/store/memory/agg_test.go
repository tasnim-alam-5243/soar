package memory

import (
	"context"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

func seedAlerts(t *testing.T, base time.Time) *store.Store {
	t.Helper()
	st := NewStore("t")
	for i, a := range []domain.Alert{
		{Title: "SSH brute force", Severity: 5, Status: "new", Entities: &domain.Entities{Hosts: []string{"web-01"}}, ReceivedAt: base.Add(5 * time.Minute)},
		{Title: "SSH brute force", Severity: 4, Status: "", Entities: &domain.Entities{Hosts: []string{"web-01"}}, ReceivedAt: base.Add(65 * time.Minute)},
		{Title: "Malware found", Severity: 4, Status: "closed", Entities: &domain.Entities{Hosts: []string{"db-02"}}, ReceivedAt: base.Add(70 * time.Minute)},
		{Title: "Port scan", Severity: 2, Status: "acknowledged", ReceivedAt: base.Add(-3 * time.Hour)},
	} {
		a.Meta = domain.Meta{ID: "alt_" + string(rune('a'+i))}
		a.Kind, a.SourceID = "alert", "wazuh"
		if _, err := st.Alerts.Create(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestHistogramAndTopTerms(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	st := seedAlerts(t, base)
	from, to := base, base.Add(2*time.Hour)
	q := store.ListQuery{Ranges: []store.Range{{Field: "received_at", Gte: from.Format(time.RFC3339Nano), Lte: to.Format(time.RFC3339Nano)}}}

	b, err := store.Histogram(ctx, st.Alerts, q, "received_at", from, to, time.Hour, "severity")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 3 || !b[0].Start.Equal(base) || !b[2].Start.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("buckets: %+v", b)
	}
	if b[0].Counts["5"] != 1 || b[1].Counts["4"] != 2 || len(b[2].Counts) != 0 {
		t.Errorf("counts: %+v", b)
	}
	b, _ = store.Histogram(ctx, st.Alerts, q, "received_at", from, to, time.Hour, "")
	if b[0].Counts[""] != 1 || b[1].Counts[""] != 2 || b[2].Counts[""] != 0 {
		t.Errorf("unsplit counts: %+v", b)
	}
	if _, err := store.Histogram(ctx, st.Alerts, q, "received_at", to, from, time.Hour, ""); err == nil {
		t.Error("from after to must be rejected")
	}

	top, err := store.TopTerms(ctx, st.Alerts, q, "entities.hosts", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0] != (store.Term{Value: "web-01", Count: 2}) || top[1] != (store.Term{Value: "db-02", Count: 1}) {
		t.Errorf("top hosts: %+v", top)
	}
	rules, _ := store.TopTerms(ctx, st.Alerts, store.ListQuery{}, "title.raw", 1)
	if len(rules) != 1 || rules[0].Value != "SSH brute force" {
		t.Errorf("top rules over the keyword sub-field: %+v", rules)
	}
}

func TestOrMissingAndSearch(t *testing.T) {
	ctx := context.Background()
	st := seedAlerts(t, time.Now().UTC())
	ids := func(q store.ListQuery) map[string]bool {
		p, err := st.Alerts.List(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, a := range p.Items {
			out[a.ID] = true
		}
		return out
	}
	strict := ids(store.ListQuery{Terms: map[string][]string{"status": {"new"}}})
	if len(strict) != 1 || !strict["alt_a"] {
		t.Errorf("plain terms: %v", strict)
	}
	loose := ids(store.ListQuery{Terms: map[string][]string{"status": {"new"}}, OrMissing: []string{"status"}})
	if len(loose) != 2 || !loose["alt_a"] || !loose["alt_b"] {
		t.Errorf("or-missing must add the alert without a status: %v", loose)
	}
	if n, _ := store.Count(ctx, st.Alerts, store.ListQuery{Terms: map[string][]string{"status": {"new"}}, OrMissing: []string{"status"}}); n != 2 {
		t.Errorf("count with or-missing: %d", n)
	}
	found := ids(store.ListQuery{Search: "brute SSH", SearchFields: []string{"title"}})
	if len(found) != 2 || !found["alt_a"] {
		t.Errorf("search all words: %v", found)
	}
	if hosts := ids(store.ListQuery{Search: "db-02", SearchFields: []string{"title", "entities.hosts"}}); len(hosts) != 1 || !hosts["alt_c"] {
		t.Errorf("search over hosts: %v", hosts)
	}
	if none := ids(store.ListQuery{Search: "brute malware", SearchFields: []string{"title"}}); len(none) != 0 {
		t.Errorf("every word must match: %v", none)
	}
}
