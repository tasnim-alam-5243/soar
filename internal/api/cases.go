package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

var (
	caseStatuses = []string{"open", "in_progress", "closed"}
	verdicts     = []string{"", "true_positive", "false_positive", "benign", "unknown"}
)

type caseIn struct {
	Title    string   `json:"title"`
	Summary  string   `json:"summary"`
	Severity *int     `json:"severity"`
	Assignee string   `json:"assignee"`
	Tags     []string `json:"tags"`
	AlertIDs []string `json:"alert_ids"`
}

func validateTags(v *verr, tags []string) {
	if len(tags) > 50 {
		v.add("tags", "at most 50 tags")
	}
	for i, t := range tags {
		v.maxLen("tags["+strconv.Itoa(i)+"]", t, 100)
	}
}

// timeline records an event on a case. It is best effort: the main write already succeeded.
func (s *Server) timeline(r *http.Request, subjectID, eventType string, data map[string]any) {
	ev := domain.TimelineEvent{Meta: domain.NewMeta("evt"), SubjectID: subjectID, EventType: eventType, Actor: s.actor(r), Data: data}
	if _, err := s.st.Timeline.Create(r.Context(), ev); err != nil {
		s.log.WarnContext(r.Context(), "could not write timeline event", "subject", subjectID, "event", eventType, "error", err.Error())
	}
}

func (s *Server) createCase(w http.ResponseWriter, r *http.Request) error {
	var created domain.Case
	var alertIDs []string
	err := createResource(s, w, r, s.st.Cases, "/cases", func() (domain.Case, string, error) {
		var in caseIn
		if err := decode(r, &in); err != nil {
			return domain.Case{}, "", err
		}
		var v verr
		v.required("title", in.Title)
		v.maxLen("title", in.Title, 300)
		v.maxLen("summary", in.Summary, 20000)
		v.maxLen("assignee", in.Assignee, 200)
		sev := 3
		if in.Severity != nil {
			sev = *in.Severity
		}
		v.intRange("severity", sev, 1, 5)
		validateTags(&v, in.Tags)
		if len(in.AlertIDs) > 200 {
			v.add("alert_ids", "at most 200 alerts")
		}
		for i, id := range in.AlertIDs {
			if _, _, err := s.st.Alerts.Get(r.Context(), id); err != nil {
				v.add("alert_ids["+itoa(i)+"]", "no such alert")
			}
		}
		if err := v.err(); err != nil {
			return domain.Case{}, "", err
		}
		alertIDs = in.AlertIDs
		created = domain.Case{
			Meta: domain.NewMeta(domain.PrefixCase), Title: in.Title, Summary: in.Summary, Status: "open",
			Severity: sev, Assignee: in.Assignee, Tags: in.Tags, AlertIDs: in.AlertIDs,
		}
		return created, created.ID, nil
	})
	if err == nil && created.ID != "" { // not a replay: record the creation
		s.timeline(r, created.ID, "case.created", map[string]any{"title": created.Title})
		for _, aid := range alertIDs { // listed on the case already; now the snapshots
			if lerr := s.cases.SnapshotAlert(r.Context(), created.ID, aid); lerr != nil {
				s.log.WarnContext(r.Context(), "case created but an alert snapshot failed", "case", created.ID, "alert", aid, "error", lerr.Error())
			}
		}
		s.notifyUI("cases", created.ID, map[string]any{"status": created.Status})
	}
	return err
}

func (s *Server) listCases(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{
		fields:      map[string]string{"created_at": "created_at", "updated_at": "updated_at", "severity": "severity", "title": "title.raw"},
		defaultName: "created_at", defaultDesc: true,
	}, "status", "severity", "assignee", "tag", "verdict", "q")
	if err != nil {
		return err
	}
	for _, f := range []string{"status", "severity", "assignee", "verdict"} {
		term(&q, v, f, f)
	}
	term(&q, v, "tag", "tags")
	if err := search(&q, v, "title", "summary", "tags"); err != nil {
		return err
	}
	page, err := s.st.Cases.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) getCase(w http.ResponseWriter, r *http.Request) error {
	return getOne(w, r, s.st.Cases)
}

// casePatch is a partial update: fields that are absent stay unchanged.
type casePatch struct {
	Title    *string   `json:"title"`
	Summary  *string   `json:"summary"`
	Status   *string   `json:"status"`
	Severity *int      `json:"severity"`
	Assignee *string   `json:"assignee"`
	Tags     *[]string `json:"tags"`
	Verdict  *string   `json:"verdict"`
}

func (p casePatch) validate() error {
	var v verr
	if p.Title != nil {
		v.required("title", *p.Title)
		v.maxLen("title", *p.Title, 300)
	}
	if p.Summary != nil {
		v.maxLen("summary", *p.Summary, 20000)
	}
	if p.Status != nil {
		v.oneOf("status", *p.Status, caseStatuses...)
	}
	if p.Severity != nil {
		v.intRange("severity", *p.Severity, 1, 5)
	}
	if p.Assignee != nil {
		v.maxLen("assignee", *p.Assignee, 200)
	}
	if p.Tags != nil {
		validateTags(&v, *p.Tags)
	}
	if p.Verdict != nil {
		v.oneOf("verdict", *p.Verdict, verdicts...)
	}
	return v.err()
}

func (s *Server) patchCase(w http.ResponseWriter, r *http.Request) error {
	var p casePatch
	if err := decode(r, &p); err != nil {
		return err
	}
	if err := p.validate(); err != nil {
		return err
	}
	var changed []string
	doc, ver, err := mutate(r, s.st.Cases, r.PathValue("id"), func(c *domain.Case) error {
		changed = changed[:0] // mutate may run again after a conflict
		set := func(name string, apply func()) { apply(); changed = append(changed, name) }
		if p.Title != nil {
			set("title", func() { c.Title = *p.Title })
		}
		if p.Summary != nil {
			set("summary", func() { c.Summary = *p.Summary })
		}
		if p.Severity != nil {
			set("severity", func() { c.Severity = *p.Severity })
		}
		if p.Assignee != nil {
			set("assignee", func() { c.Assignee = *p.Assignee })
		}
		if p.Tags != nil {
			set("tags", func() { c.Tags = *p.Tags })
		}
		if p.Verdict != nil {
			set("verdict", func() { c.Verdict = *p.Verdict })
		}
		if p.Status != nil && *p.Status != c.Status {
			set("status", func() {
				c.Status = *p.Status
				if c.Status == "closed" {
					now := s.now()
					c.ClosedAt = &now // starts the 3-day retention clock (spec section 5.2)
				} else {
					c.ClosedAt = nil
				}
			})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		s.timeline(r, doc.ID, "case.updated", map[string]any{"fields": strings.Join(changed, ",")})
		s.notifyUI("cases", doc.ID, map[string]any{"status": doc.Status, "fields": changed})
	}
	if doc.Status == "closed" && slices.Contains(changed, "status") {
		if ids := s.cases.CloseAlerts(r.Context(), doc); len(ids) > 0 {
			s.timeline(r, doc.ID, "alerts.closed", map[string]any{"alert_ids": strings.Join(ids, ",")})
			for _, id := range ids {
				s.notifyUI("alerts", id, map[string]any{"status": "closed", "case_id": doc.ID})
			}
		}
	}
	return writeResource(w, http.StatusOK, doc, ver)
}

type taskIn struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Assignee    string     `json:"assignee"`
	DueAt       *time.Time `json:"due_at"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) error {
	caseID := r.PathValue("id")
	if _, _, err := s.st.Cases.Get(r.Context(), caseID); err != nil {
		return err
	}
	var created domain.CaseTask
	err := createResource(s, w, r, s.st.CaseTasks, "", func() (domain.CaseTask, string, error) {
		var in taskIn
		if err := decode(r, &in); err != nil {
			return domain.CaseTask{}, "", err
		}
		var v verr
		v.required("title", in.Title)
		v.maxLen("title", in.Title, 300)
		v.maxLen("description", in.Description, 20000)
		v.maxLen("assignee", in.Assignee, 200)
		if err := v.err(); err != nil {
			return domain.CaseTask{}, "", err
		}
		created = domain.CaseTask{
			Meta: domain.NewMeta(domain.PrefixTask), CaseID: caseID, Title: in.Title,
			Description: in.Description, Status: "open", Assignee: in.Assignee, DueAt: in.DueAt,
		}
		return created, created.ID, nil
	})
	if err == nil && created.ID != "" {
		s.timeline(r, caseID, "task.created", map[string]any{"task_id": created.ID, "title": created.Title})
	}
	return err
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) error {
	caseID := r.PathValue("id")
	if _, _, err := s.st.Cases.Get(r.Context(), caseID); err != nil {
		return err
	}
	q, _, err := listQuery(r, sortSpec{fields: map[string]string{"created_at": "created_at"}, defaultName: "created_at"})
	if err != nil {
		return err
	}
	q.Terms = map[string][]string{"case_id": {caseID}}
	page, err := s.st.CaseTasks.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) caseTimeline(w http.ResponseWriter, r *http.Request) error {
	caseID := r.PathValue("id")
	if _, _, err := s.st.Cases.Get(r.Context(), caseID); err != nil {
		return err
	}
	q, _, err := listQuery(r, sortSpec{fields: map[string]string{"created_at": "created_at"}, defaultName: "created_at"})
	if err != nil {
		return err
	}
	q.Terms = map[string][]string{"subject_id": {caseID}}
	page, err := s.st.Timeline.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

type caseObservableIn struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// addCaseObservable links an indicator to a case, creating the observable on first sight.
func (s *Server) addCaseObservable(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	caseID := r.PathValue("id")
	if _, _, err := s.st.Cases.Get(ctx, caseID); err != nil {
		return err
	}
	var in caseObservableIn
	if err := decode(r, &in); err != nil {
		return err
	}
	var v verr
	v.oneOf("type", in.Type, observableTypes...)
	in.Value = strings.TrimSpace(in.Value)
	v.required("value", in.Value)
	v.maxLen("value", in.Value, 2048)
	if err := v.err(); err != nil {
		return err
	}

	now := s.now()
	id := domain.ObservableID(in.Type, in.Value)
	status := http.StatusOK
	doc := domain.ObservableDoc{Meta: domain.Meta{ID: id}, Type: in.Type, Value: in.Value, FirstSeen: now, LastSeen: now, CaseIDs: []string{caseID}}
	_, err := s.st.Observables.Create(ctx, doc)
	switch {
	case err == nil:
		status = http.StatusCreated
	case errors.Is(err, store.ErrAlreadyExists):
		_, _, err = s.st.Observables.Update(ctx, id, func(o *domain.ObservableDoc) error {
			o.LastSeen = now
			for _, c := range o.CaseIDs {
				if c == caseID {
					return nil
				}
			}
			o.CaseIDs = append(o.CaseIDs, caseID)
			return nil
		})
		if err != nil {
			return err
		}
	default:
		return err
	}
	stored, ver, err := s.st.Observables.Get(ctx, id)
	if err != nil {
		return err
	}
	s.timeline(r, caseID, "observable.added", map[string]any{"type": in.Type, "value": in.Value})
	return writeResource(w, status, stored, ver)
}
