package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/yourorg/soar/internal/store"
)

type dashboardOut struct {
	At               time.Time         `json:"at"`
	AlertsLast24h    int64             `json:"alerts_last_24h"`
	AlertsBySeverity map[string]int64  `json:"alerts_by_severity"`
	OpenCases        int64             `json:"open_cases"`
	CasesBySeverity  map[string]int64  `json:"cases_by_severity"`
	Executions24h    map[string]int64  `json:"executions_last_24h"`
	PendingApprovals int64             `json:"pending_approvals"`
	MTTRHours        *float64          `json:"mttr_hours,omitempty"`
	AutomationRate   *float64          `json:"automation_rate,omitempty"`
	Queues           []map[string]any  `json:"queues,omitempty"`
	Sources          []map[string]any  `json:"sources,omitempty"`
	Errors           map[string]string `json:"errors,omitempty"`
}

// dashboard returns the numbers of the dashboard page (spec 12): alert volume, open
// cases, MTTR, automation rate and queue health. Each figure is computed
// independently; one failing figure does not hide the others.
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	now := s.now()
	since := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	out := dashboardOut{At: now, AlertsBySeverity: map[string]int64{}, CasesBySeverity: map[string]int64{}, Executions24h: map[string]int64{}, Errors: map[string]string{}}
	fail := func(what string, err error) { out.Errors[what] = err.Error() }

	day := store.Range{Field: "received_at", Gte: since}
	if n, err := store.Count(ctx, s.st.Alerts, store.ListQuery{Ranges: []store.Range{day}}); err == nil {
		out.AlertsLast24h = n
	} else {
		fail("alerts", err)
	}
	for sev := 1; sev <= 5; sev++ {
		k := strconv.Itoa(sev)
		if n, err := store.Count(ctx, s.st.Alerts, store.ListQuery{Terms: map[string][]string{"severity": {k}}, Ranges: []store.Range{day}}); err == nil {
			out.AlertsBySeverity[k] = n
		}
		if n, err := store.Count(ctx, s.st.Cases, store.ListQuery{Terms: map[string][]string{"severity": {k}, "status": {"open", "in_progress"}}}); err == nil {
			out.CasesBySeverity[k] = n
		}
	}
	if n, err := store.Count(ctx, s.st.Cases, store.ListQuery{Terms: map[string][]string{"status": {"open", "in_progress"}}}); err == nil {
		out.OpenCases = n
	} else {
		fail("cases", err)
	}
	var execTotal, execAlert int64
	for _, st := range []string{"queued", "running", "waiting", "succeeded", "failed", "cancelled", "timed_out"} {
		if n, err := store.Count(ctx, s.st.Executions, store.ListQuery{
			Terms:  map[string][]string{"status": {st}},
			Ranges: []store.Range{{Field: "created_at", Gte: since}},
		}); err == nil {
			out.Executions24h[st] = n
			execTotal += n
		}
	}
	if n, err := store.Count(ctx, s.st.Executions, store.ListQuery{
		Terms:  map[string][]string{"trigger.type": {"alert"}},
		Ranges: []store.Range{{Field: "created_at", Gte: since}},
	}); err == nil {
		execAlert = n
	}
	if out.AlertsLast24h > 0 {
		rate := float64(execAlert) / float64(out.AlertsLast24h)
		if rate > 1 {
			rate = 1
		}
		out.AutomationRate = &rate
	}
	_ = execTotal
	if n, err := store.Count(ctx, s.st.Approvals, store.ListQuery{Terms: map[string][]string{"status": {"pending"}}}); err == nil {
		out.PendingApprovals = n
	}
	// MTTR over cases closed in the last 7 days (sampled: up to 200)
	page, err := s.st.Cases.List(ctx, store.ListQuery{
		Terms:  map[string][]string{"status": {"closed"}},
		Ranges: []store.Range{{Field: "closed_at", Gte: now.Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)}}, Sort: "closed_at", Desc: true, Limit: 200,
	})
	if err == nil && len(page.Items) > 0 {
		var sum float64
		var n int
		for _, c := range page.Items {
			if c.ClosedAt != nil {
				sum += c.ClosedAt.Sub(c.CreatedAt).Hours()
				n++
			}
		}
		if n > 0 {
			m := sum / float64(n)
			out.MTTRHours = &m
		}
	}
	if s.q != nil {
		if stats, err := s.q.Stats(ctx); err == nil {
			for _, st := range stats {
				var pending uint64
				for _, c := range st.Consumers {
					pending += c.Pending + uint64(c.AckPending)
				}
				out.Queues = append(out.Queues, map[string]any{"name": st.Name, "messages": st.Messages, "bytes": st.Bytes, "max_bytes": st.MaxBytes, "pending": pending})
			}
		} else {
			fail("queues", err)
		}
	}
	if srcs, err := s.st.Sources.List(ctx, store.ListQuery{Limit: 50, Sort: "name"}); err == nil {
		for _, src := range srcs.Items {
			m := map[string]any{"name": src.Name, "enabled": src.Enabled, "adapter": src.Adapter}
			if src.Health != nil {
				m["status"], m["last_event_at"], m["rate_per_sec"], m["error_rate"] = src.Health.Status, src.Health.LastEventAt, src.Health.RatePerSec, src.Health.ErrorRate
			}
			out.Sources = append(out.Sources, m)
		}
	}
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	return writeJSON(w, http.StatusOK, out)
}
