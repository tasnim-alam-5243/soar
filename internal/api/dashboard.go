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

	// Figures for the selected range (?range=1h|24h|7d), each with the previous period
	// of the same length for comparison. The fields above keep their fixed meaning.
	Range *rangeOut `json:"range,omitempty"`
}

type rangeOut struct {
	Name            string       `json:"name"`
	From            time.Time    `json:"from"`
	To              time.Time    `json:"to"`
	IntervalSeconds int64        `json:"interval_seconds"`
	Alerts          periodCount  `json:"alerts"`
	AlertSeries     []seriesItem `json:"alert_series"` // counts per severity ("1".."5")
	Executions      periodCount  `json:"executions"`
	Failed          periodCount  `json:"failed_executions"` // failed and timed_out
	ExecutionSeries []seriesItem `json:"execution_series"`  // counts per status
	CasesOpened     periodCount  `json:"cases_opened"`
	AutomationRate  periodFloat  `json:"automation_rate"`
	MTTRHours       periodFloat  `json:"mttr_hours"` // cases closed in the period
	TopRules        []termOut    `json:"top_rules"`
	TopSources      []termOut    `json:"top_sources"`
	TopHosts        []termOut    `json:"top_hosts"`
}

type periodCount struct {
	Current  int64 `json:"current"`
	Previous int64 `json:"previous"`
}

type periodFloat struct {
	Current  *float64 `json:"current"`
	Previous *float64 `json:"previous"`
}

type seriesItem struct {
	T      time.Time        `json:"t"`
	Counts map[string]int64 `json:"counts"`
}

type termOut struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// dashboardRanges are the selectable ranges and their histogram intervals. There is no
// 30-day range: retention keeps 7 days of data (spec 5.2).
var dashboardRanges = map[string]struct{ span, interval time.Duration }{
	"1h":  {time.Hour, 5 * time.Minute},
	"24h": {24 * time.Hour, time.Hour},
	"7d":  {7 * 24 * time.Hour, 6 * time.Hour},
}

// rangeFigures computes the range section. Like the rest of the dashboard, a failing
// figure is reported in errors and leaves the others intact.
func (s *Server) rangeFigures(r *http.Request, name string, now time.Time, fail func(string, error)) *rangeOut {
	ctx := r.Context()
	spec := dashboardRanges[name]
	to, from := now, now.Add(-spec.span)
	prevFrom := from.Add(-spec.span)
	ts := func(t time.Time) string { return t.Format(time.RFC3339Nano) }
	cur := func(field string) store.Range { return store.Range{Field: field, Gte: ts(from), Lte: ts(to)} }
	prev := func(field string) store.Range { return store.Range{Field: field, Gte: ts(prevFrom), Lte: ts(from)} }
	count := func(what string, repo store.Counter, q store.ListQuery, field string) periodCount {
		var pc periodCount
		var err error
		qc, qp := q, q
		qc.Ranges = append(append([]store.Range(nil), q.Ranges...), cur(field))
		qp.Ranges = append(append([]store.Range(nil), q.Ranges...), prev(field))
		if pc.Current, err = repo.Count(ctx, qc); err != nil {
			fail(what, err)
			return pc
		}
		if pc.Previous, err = repo.Count(ctx, qp); err != nil {
			fail(what, err)
		}
		return pc
	}
	out := &rangeOut{Name: name, From: from, To: to, IntervalSeconds: int64(spec.interval / time.Second)}
	alerts, ok1 := s.st.Alerts.(store.Counter)
	execs, ok2 := s.st.Executions.(store.Counter)
	casesC, ok3 := s.st.Cases.(store.Counter)
	if !ok1 || !ok2 || !ok3 {
		fail("range", store.ErrUnsupported)
		return out
	}
	out.Alerts = count("range.alerts", alerts, store.ListQuery{}, "received_at")
	out.Executions = count("range.executions", execs, store.ListQuery{}, "created_at")
	out.Failed = count("range.executions", execs, store.ListQuery{Terms: map[string][]string{"status": {"failed", "timed_out"}}}, "created_at")
	out.CasesOpened = count("range.cases", casesC, store.ListQuery{}, "created_at")
	auto := count("range.automation", execs, store.ListQuery{Terms: map[string][]string{"trigger.type": {"alert"}}}, "created_at")
	rate := func(n, d int64) *float64 {
		if d == 0 {
			return nil
		}
		v := min(float64(n)/float64(d), 1)
		return &v
	}
	out.AutomationRate = periodFloat{Current: rate(auto.Current, out.Alerts.Current), Previous: rate(auto.Previous, out.Alerts.Previous)}
	out.MTTRHours = periodFloat{Current: s.mttr(r, from, to), Previous: s.mttr(r, prevFrom, from)}

	series := func(what string, b []store.TimeBucket, err error) []seriesItem {
		if err != nil {
			fail(what, err)
			return []seriesItem{}
		}
		res := make([]seriesItem, 0, len(b))
		for _, x := range b {
			res = append(res, seriesItem{T: x.Start, Counts: x.Counts})
		}
		return res
	}
	b, err := store.Histogram(ctx, s.st.Alerts, store.ListQuery{Ranges: []store.Range{cur("received_at")}}, "received_at", from, to, spec.interval, "severity")
	out.AlertSeries = series("range.alert_series", b, err)
	b, err = store.Histogram(ctx, s.st.Executions, store.ListQuery{Ranges: []store.Range{cur("created_at")}}, "created_at", from, to, spec.interval, "status")
	out.ExecutionSeries = series("range.execution_series", b, err)

	top := func(what, field string) []termOut {
		t, err := store.TopTerms(ctx, s.st.Alerts, store.ListQuery{Ranges: []store.Range{cur("received_at")}}, field, 5)
		if err != nil {
			fail(what, err)
		}
		res := make([]termOut, 0, len(t))
		for _, x := range t {
			res = append(res, termOut{Value: x.Value, Count: x.Count})
		}
		return res
	}
	out.TopRules = top("range.top_rules", "title.raw")
	out.TopSources = top("range.top_sources", "source_id")
	out.TopHosts = top("range.top_hosts", "entities.hosts")
	return out
}

// mttr is the mean time to resolve, in hours, of cases closed between from and to
// (sampled: the latest 200).
func (s *Server) mttr(r *http.Request, from, to time.Time) *float64 {
	page, err := s.st.Cases.List(r.Context(), store.ListQuery{
		Terms:  map[string][]string{"status": {"closed"}},
		Ranges: []store.Range{{Field: "closed_at", Gte: from.Format(time.RFC3339Nano), Lte: to.Format(time.RFC3339Nano)}}, Sort: "closed_at", Desc: true, Limit: 200,
	})
	if err != nil {
		return nil
	}
	var sum float64
	var n int
	for _, c := range page.Items {
		if c.ClosedAt != nil {
			sum += c.ClosedAt.Sub(c.CreatedAt).Hours()
			n++
		}
	}
	if n == 0 {
		return nil
	}
	m := sum / float64(n)
	return &m
}

// dashboard returns the numbers of the dashboard page (spec 12): alert volume, open
// cases, MTTR, automation rate and queue health. Each figure is computed
// independently; one failing figure does not hide the others.
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	now := s.now()
	v := r.URL.Query()
	for k := range v {
		if k != "range" {
			return badRequest("unknown query parameter %q", k)
		}
	}
	rangeName := v.Get("range")
	if _, ok := dashboardRanges[rangeName]; rangeName != "" && !ok {
		return badRequest("range must be one of 1h, 24h, 7d")
	}
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
	if rangeName != "" {
		out.Range = s.rangeFigures(r, rangeName, now, fail)
	}
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	return writeJSON(w, http.StatusOK, out)
}
