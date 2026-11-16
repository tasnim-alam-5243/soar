package cases

import (
	"context"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store/memory"
)

func TestLinkTakesAlertsUpAndCloseClosesThem(t *testing.T) {
	ctx := context.Background()
	st := memory.NewStore("t")
	svc := New(st, nil)
	mk := func(id, status, verdict string) {
		a := domain.Alert{Meta: domain.Meta{ID: id}, Kind: "alert", SourceID: "s", Title: id, Severity: 3, Status: status, Verdict: verdict, ReceivedAt: time.Now().UTC()}
		if _, err := st.Alerts.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	mk("alt_new", "new", "")
	mk("alt_legacy", "", "")
	mk("alt_ack", "acknowledged", "")
	mk("alt_done", "closed", "benign")
	mk("alt_own", "in_progress", "false_positive")

	c, _, err := svc.Create(ctx, domain.Case{Meta: domain.Meta{ID: "cas_1"}, Title: "c"}, []string{"alt_new", "alt_legacy", "alt_ack", "alt_done", "alt_own"}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	status := func(id string) (string, string) {
		a, _, err := st.Alerts.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return a.Status, a.Verdict
	}
	for id, want := range map[string]string{"alt_new": "in_progress", "alt_legacy": "in_progress", "alt_ack": "in_progress", "alt_done": "closed", "alt_own": "in_progress"} {
		if got, _ := status(id); got != want {
			t.Errorf("after linking %s: %q, want %q", id, got, want)
		}
	}

	// the built-in case.close action closes through Service.Update; no verdict on the case
	closed := "closed"
	if _, _, err := svc.Update(ctx, c.ID, Patch{Status: &closed}, "tester"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string][2]string{
		"alt_new":  {"closed", "unknown"},        // no verdict anywhere: unknown
		"alt_own":  {"closed", "false_positive"}, // keeps its own verdict when the case has none
		"alt_done": {"closed", "benign"},         // already closed: untouched
	} {
		if s, v := status(id); s != want[0] || v != want[1] {
			t.Errorf("after closing %s: %q/%q, want %q/%q", id, s, v, want[0], want[1])
		}
	}

	// reopen, then close with a verdict: the case verdict wins over the alert's own
	open, verdict := "open", "true_positive"
	_, _, _ = svc.Update(ctx, c.ID, Patch{Status: &open}, "tester")
	_, _, _ = st.Alerts.Update(ctx, "alt_own", func(a *domain.Alert) error { a.Status = "in_progress"; return nil })
	if _, _, err := svc.Update(ctx, c.ID, Patch{Status: &closed, Verdict: &verdict}, "tester"); err != nil {
		t.Fatal(err)
	}
	if s, v := status("alt_own"); s != "closed" || v != "true_positive" {
		t.Errorf("case verdict must be copied: %q/%q", s, v)
	}
}
