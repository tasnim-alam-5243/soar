package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/yourorg/soar/internal/cases"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

// alertPatch is a triage update: fields that are absent stay unchanged.
type alertPatch struct {
	Status   *string `json:"status"`
	Verdict  *string `json:"verdict"`
	Assignee *string `json:"assignee"`
}

// patchAlert records triage: status, verdict and assignee. Closing needs a verdict (in
// the request or already on the alert) so every closed alert carries an outcome.
func (s *Server) patchAlert(w http.ResponseWriter, r *http.Request) error {
	var p alertPatch
	if err := decode(r, &p); err != nil {
		return err
	}
	var v verr
	if p.Status == nil && p.Verdict == nil && p.Assignee == nil {
		v.add("", "give at least one of status, verdict, assignee")
	}
	if p.Status != nil {
		v.oneOf("status", *p.Status, cases.AlertStatuses...)
	}
	if p.Verdict != nil {
		v.oneOf("verdict", *p.Verdict, cases.AlertVerdicts...)
	}
	if p.Assignee != nil {
		*p.Assignee = strings.TrimSpace(*p.Assignee)
		v.maxLen("assignee", *p.Assignee, 200)
	}
	if err := v.err(); err != nil {
		return err
	}
	var before domain.Alert
	var changed []string
	doc, ver, err := mutate(r, s.st.Alerts, r.PathValue("id"), func(a *domain.Alert) error {
		before, changed = *a, changed[:0] // mutate may run again after a conflict
		if p.Verdict != nil && *p.Verdict != a.Verdict {
			a.Verdict, changed = *p.Verdict, append(changed, "verdict")
		}
		if p.Assignee != nil && *p.Assignee != a.Assignee {
			a.Assignee, changed = *p.Assignee, append(changed, "assignee")
		}
		if p.Status != nil && *p.Status != a.Status {
			a.Status, changed = *p.Status, append(changed, "status")
		}
		if a.Status == cases.AlertClosed && a.Verdict == "" && (slices.Contains(changed, "status") || slices.Contains(changed, "verdict")) {
			return unprocessable([]FieldError{{Path: "verdict", Message: "is required to close an alert"}})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		s.audit(r, "alert.triage", "alert:"+doc.ID,
			map[string]any{"status": before.Status, "verdict": before.Verdict, "assignee": before.Assignee},
			map[string]any{"status": doc.Status, "verdict": doc.Verdict, "assignee": doc.Assignee})
		s.notifyUI("alerts", doc.ID, map[string]any{"status": doc.Status, "verdict": doc.Verdict, "assignee": doc.Assignee, "fields": changed})
	}
	return writeResource(w, http.StatusOK, doc, ver)
}

// assignees lists names to offer when assigning alerts and cases: the assignees already
// in use plus the user principals. OIDC users are not principals, so names seen on
// cases and alerts are what makes them pickable.
func (s *Server) assignees(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	if t, err := store.TopTerms(ctx, s.st.Cases, store.ListQuery{}, "assignee", 100); err == nil {
		for _, x := range t {
			add(x.Value)
		}
	}
	if t, err := store.TopTerms(ctx, s.st.Alerts, store.ListQuery{}, "assignee", 100); err == nil {
		for _, x := range t {
			add(x.Value)
		}
	}
	if page, err := s.st.Principals.List(ctx, store.ListQuery{Terms: map[string][]string{"type": {"user"}}, Limit: 200, Sort: "name"}); err == nil {
		for _, p := range page.Items {
			if !p.Disabled {
				add(p.Name)
			}
		}
	}
	add(s.actor(r))
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return writeJSON(w, http.StatusOK, map[string]any{"items": names})
}

var observableTypes = []string{"ip", "domain", "url", "hash", "email", "host", "user", "file_path"}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{
		fields:      map[string]string{"received_at": "received_at", "severity": "severity", "occurrence_count": "occurrence_count", "last_seen": "last_seen"},
		defaultName: "received_at", defaultDesc: true,
	}, "source_id", "system", "kind", "status", "verdict", "severity", "min_severity", "case_id", "assignee", "q", "from", "to")
	if err != nil {
		return err
	}
	for _, f := range []string{"source_id", "system", "kind", "status", "verdict", "severity", "case_id", "assignee"} {
		term(&q, v, f, f)
	}
	if slices.Contains(q.Terms["status"], cases.AlertNew) {
		q.OrMissing = append(q.OrMissing, "status") // alerts from before statuses existed count as new
	}
	if err := search(&q, v, "title", "description", "summary", "fingerprint", "observables.value", "entities.hosts", "entities.users", "tags"); err != nil {
		return err
	}
	if m := v.Get("min_severity"); m != "" {
		n, err := strconv.Atoi(m)
		if err != nil || n < 1 || n > 5 {
			return badRequest("min_severity must be an integer from 1 to 5")
		}
		q.Ranges = append(q.Ranges, store.Range{Field: "severity", Gte: n})
	}
	if err := timeRange(&q, v, "received_at"); err != nil {
		return err
	}
	page, err := s.st.Alerts.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) getAlert(w http.ResponseWriter, r *http.Request) error {
	return getOne(w, r, s.st.Alerts)
}

type alertIn struct {
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Severity    *int                `json:"severity"`
	Kind        string              `json:"kind"`
	SourceID    string              `json:"source_id"`
	ExternalID  string              `json:"external_id"`
	Category    string              `json:"category"`
	Tags        []string            `json:"tags"`
	Observables []domain.Observable `json:"observables"`
	Mitre       *domain.Mitre       `json:"mitre"`
	Ext         map[string]any      `json:"ext"`
}

// createAlert records an alert by hand (analyst-created or test data). Alerts from
// monitored systems arrive through the ingestion pipeline (Step 5) instead.
func (s *Server) createAlert(w http.ResponseWriter, r *http.Request) error {
	return createResource(s, w, r, s.st.Alerts, "/alerts", func() (domain.Alert, string, error) {
		var in alertIn
		if err := decode(r, &in); err != nil {
			return domain.Alert{}, "", err
		}
		var v verr
		v.required("title", in.Title)
		v.maxLen("title", in.Title, 500)
		v.maxLen("description", in.Description, 20000)
		if in.Severity == nil {
			v.add("severity", "is required")
		} else {
			v.intRange("severity", *in.Severity, 1, 5)
		}
		if in.Kind == "" {
			in.Kind = "alert"
		}
		v.oneOf("kind", in.Kind, "alert", "case_event", "indicator", "asset", "custom")
		if in.SourceID == "" {
			in.SourceID = "manual"
		}
		v.maxLen("source_id", in.SourceID, 100)
		if len(in.Tags) > 50 {
			v.add("tags", "at most 50 tags")
		}
		if len(in.Observables) > 200 {
			v.add("observables", "at most 200 observables")
		}
		for i, o := range in.Observables {
			p := "observables[" + strconv.Itoa(i) + "]"
			v.oneOf(p+".type", o.Type, observableTypes...)
			v.required(p+".value", o.Value)
			v.maxLen(p+".value", o.Value, 2048)
		}
		if err := v.err(); err != nil {
			return domain.Alert{}, "", err
		}
		now := s.now()
		doc := domain.Alert{
			Meta: domain.NewMeta(domain.PrefixAlert), Kind: in.Kind, SourceID: in.SourceID, ExternalID: in.ExternalID,
			Title: in.Title, Description: in.Description, Severity: *in.Severity, OriginalSeverity: strconv.Itoa(*in.Severity),
			Status: "new", Category: in.Category, Tags: in.Tags, Observables: in.Observables, Mitre: in.Mitre, Ext: in.Ext,
			OccurredAt: &now, ReceivedAt: now, LastSeen: &now, OccurrenceCount: 1,
		}
		return doc, doc.ID, nil
	})
}
