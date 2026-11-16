// Package cases holds case management logic shared by the REST API and the built-in
// case actions: creating cases, linking alerts (with snapshots that outlive the
// alert's own retention, spec 5.2), observables, closing, and the timeline.
package cases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

// Statuses and verdicts.
var (
	Statuses = []string{"open", "in_progress", "closed"}
	Verdicts = []string{"", "true_positive", "false_positive", "benign", "unknown"}
)

// ObservableTypes are the indicator types the platform understands.
var ObservableTypes = []string{"ip", "domain", "url", "hash", "email", "host", "user", "file_path"}

// Service implements case operations on the store.
type Service struct {
	St  *store.Store
	Log *slog.Logger
	Now func() time.Time
}

// New returns a Service.
func New(st *store.Store, log *slog.Logger) *Service {
	return &Service{St: st, Log: log, Now: func() time.Time { return time.Now().UTC() }}
}

// Timeline records an event. It is best effort: the main write already succeeded.
func (s *Service) Timeline(ctx context.Context, subjectID, eventType, actor string, data map[string]any) {
	ev := domain.TimelineEvent{Meta: domain.NewMeta("evt"), SubjectID: subjectID, EventType: eventType, Actor: actor, Data: data}
	if _, err := s.St.Timeline.Create(ctx, ev); err != nil && s.Log != nil {
		s.Log.WarnContext(ctx, "could not write timeline event", "subject", subjectID, "event", eventType, "error", err.Error())
	}
}

// Create stores c (whose ID the caller chose) and links alertIDs. If a case with the
// same ID already exists it is returned unchanged with created=false, which makes
// retried creates with a deterministic ID idempotent.
func (s *Service) Create(ctx context.Context, c domain.Case, alertIDs []string, actor string) (domain.Case, bool, error) {
	if c.ID == "" {
		return domain.Case{}, false, fmt.Errorf("%w: case id is required", store.ErrInvalid)
	}
	if c.Status == "" {
		c.Status = "open"
	}
	if c.Severity == 0 {
		c.Severity = 3
	}
	_, err := s.St.Cases.Create(ctx, c)
	created := err == nil
	switch {
	case created:
		s.Timeline(ctx, c.ID, "case.created", actor, map[string]any{"title": c.Title})
	case errors.Is(err, store.ErrAlreadyExists):
	default:
		return domain.Case{}, false, err
	}
	if len(alertIDs) > 0 {
		if _, err := s.LinkAlerts(ctx, c.ID, alertIDs, actor); err != nil {
			return domain.Case{}, created, err
		}
	}
	out, _, err := s.St.Cases.Get(ctx, c.ID)
	return out, created, err
}

// LinkAlerts links alerts to a case: it copies each alert into soar-case-alerts (so the
// case keeps it after the alert expires), sets alert.case_id and adds the id to the case.
// Alerts that are already linked are skipped. It returns the ids it linked.
func (s *Service) LinkAlerts(ctx context.Context, caseID string, alertIDs []string, actor string) ([]string, error) {
	c, _, err := s.St.Cases.Get(ctx, caseID)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, id := range c.AlertIDs {
		have[id] = true
	}
	var linked []string
	for _, aid := range alertIDs {
		if aid == "" || have[aid] {
			continue
		}
		if err := s.SnapshotAlert(ctx, caseID, aid); err != nil {
			return linked, err
		}
		have[aid] = true
		linked = append(linked, aid)
	}
	if len(linked) == 0 {
		return nil, nil
	}
	if _, _, err := s.St.Cases.Update(ctx, caseID, func(x *domain.Case) error {
		seen := map[string]bool{}
		for _, id := range x.AlertIDs {
			seen[id] = true
		}
		for _, id := range linked {
			if !seen[id] {
				x.AlertIDs = append(x.AlertIDs, id)
			}
		}
		return nil
	}); err != nil {
		return linked, err
	}
	s.Timeline(ctx, caseID, "alerts.linked", actor, map[string]any{"alert_ids": strings.Join(linked, ",")})
	return linked, nil
}

// SnapshotAlert copies an alert into soar-case-alerts and points the alert at the
// case. It is idempotent (the snapshot id is derived from case and alert).
func (s *Service) SnapshotAlert(ctx context.Context, caseID, alertID string) error {
	a, _, err := s.St.Alerts.Get(ctx, alertID)
	if err != nil {
		return fmt.Errorf("alert %s: %w", alertID, err)
	}
	snap := map[string]any{}
	b, _ := json.Marshal(a)
	_ = json.Unmarshal(b, &snap)
	ca := domain.CaseAlert{Meta: domain.Meta{ID: domain.DeterministicID("cal", caseID, alertID)}, CaseID: caseID, AlertID: alertID, Snapshot: snap, LinkedAt: s.Now()}
	if _, err := s.St.CaseAlerts.Put(ctx, ca); err != nil {
		return err
	}
	if a.CaseID != caseID || AlertTakenUp(a.Status) {
		if _, _, err := s.St.Alerts.Update(ctx, alertID, func(x *domain.Alert) error {
			x.CaseID = caseID
			if AlertTakenUp(x.Status) {
				x.Status = AlertInProgress // a case now owns the investigation
			}
			return nil
		}); err != nil && s.Log != nil {
			s.Log.WarnContext(ctx, "could not set case_id on alert; the case still holds its snapshot", "alert", alertID, "error", err.Error())
		}
	}
	return nil
}

// Alert statuses and verdicts. Alerts written before statuses existed have none and
// count as new.
const (
	AlertNew          = "new"
	AlertAcknowledged = "acknowledged"
	AlertInProgress   = "in_progress"
	AlertClosed       = "closed"
)

var (
	AlertStatuses = []string{AlertNew, AlertAcknowledged, AlertInProgress, AlertClosed}
	AlertVerdicts = []string{"", "true_positive", "false_positive", "benign", "unknown"}
)

// AlertTakenUp reports whether linking an alert in this status to a case moves it to
// in_progress (new and acknowledged alerts; in_progress and closed ones stay).
func AlertTakenUp(status string) bool {
	return status == "" || status == AlertNew || status == AlertAcknowledged
}

// CloseAlerts closes the still-open alerts linked to a closed case. Each gets the case's
// verdict, or keeps its own, or "unknown" when neither exists, so every closed alert
// carries an outcome. Alerts that expired (retention) are skipped. It returns the ids
// it closed; failures are logged, not returned, because the case is already closed.
func (s *Service) CloseAlerts(ctx context.Context, c domain.Case) []string {
	var closed []string
	for _, id := range c.AlertIDs {
		changed := false
		_, _, err := s.St.Alerts.Update(ctx, id, func(a *domain.Alert) error {
			changed = false
			if a.Status == AlertClosed {
				return nil
			}
			a.Status, changed = AlertClosed, true
			switch {
			case c.Verdict != "":
				a.Verdict = c.Verdict
			case a.Verdict == "":
				a.Verdict = "unknown"
			}
			return nil
		})
		switch {
		case err == nil && changed:
			closed = append(closed, id)
		case err != nil && !errors.Is(err, store.ErrNotFound) && s.Log != nil:
			s.Log.WarnContext(ctx, "case closed but one of its alerts could not be closed", "case", c.ID, "alert", id, "error", err.Error())
		}
	}
	return closed
}

// ApplyStatus sets a case's status and keeps closed_at in step (closing starts the
// retention clock, spec 5.2; reopening stops it).
func ApplyStatus(c *domain.Case, status string, now time.Time) {
	if c.Status == status {
		return
	}
	c.Status = status
	if status == "closed" {
		c.ClosedAt = &now
	} else {
		c.ClosedAt = nil
	}
}

// Patch is a partial case update (nil fields stay unchanged).
type Patch struct {
	Title    *string
	Summary  *string
	Status   *string
	Severity *int
	Assignee *string
	Tags     *[]string
	Verdict  *string
}

// Apply applies p and returns the names of the fields that changed.
func (p Patch) Apply(c *domain.Case, now time.Time) []string {
	var changed []string
	if p.Title != nil && *p.Title != c.Title {
		c.Title, changed = *p.Title, append(changed, "title")
	}
	if p.Summary != nil && *p.Summary != c.Summary {
		c.Summary, changed = *p.Summary, append(changed, "summary")
	}
	if p.Severity != nil && *p.Severity != c.Severity {
		c.Severity, changed = *p.Severity, append(changed, "severity")
	}
	if p.Assignee != nil && *p.Assignee != c.Assignee {
		c.Assignee, changed = *p.Assignee, append(changed, "assignee")
	}
	if p.Tags != nil {
		c.Tags, changed = *p.Tags, append(changed, "tags")
	}
	if p.Verdict != nil && *p.Verdict != c.Verdict {
		c.Verdict, changed = *p.Verdict, append(changed, "verdict")
	}
	if p.Status != nil && *p.Status != c.Status {
		ApplyStatus(c, *p.Status, now)
		changed = append(changed, "status")
	}
	return changed
}

// Update applies a patch with optimistic concurrency and records the change.
func (s *Service) Update(ctx context.Context, caseID string, p Patch, actor string) (domain.Case, []string, error) {
	var changed []string
	doc, _, err := s.St.Cases.Update(ctx, caseID, func(c *domain.Case) error {
		changed = p.Apply(c, s.Now())
		return nil
	})
	if err != nil {
		return domain.Case{}, nil, err
	}
	if len(changed) > 0 {
		s.Timeline(ctx, caseID, "case.updated", actor, map[string]any{"fields": strings.Join(changed, ",")})
	}
	if doc.Status == "closed" && slices.Contains(changed, "status") {
		if ids := s.CloseAlerts(ctx, doc); len(ids) > 0 {
			s.Timeline(ctx, caseID, "alerts.closed", actor, map[string]any{"alert_ids": strings.Join(ids, ",")})
		}
	}
	return doc, changed, nil
}

// AddObservable links an indicator to a case, creating it on first sight.
func (s *Service) AddObservable(ctx context.Context, caseID, typ, value, actor string) (domain.ObservableDoc, bool, error) {
	value = strings.TrimSpace(value)
	if caseID != "" {
		if _, _, err := s.St.Cases.Get(ctx, caseID); err != nil {
			return domain.ObservableDoc{}, false, err
		}
	}
	now := s.Now()
	id := domain.ObservableID(typ, value)
	doc := domain.ObservableDoc{Meta: domain.Meta{ID: id}, Type: typ, Value: value, FirstSeen: now, LastSeen: now}
	if caseID != "" {
		doc.CaseIDs = []string{caseID}
	}
	_, err := s.St.Observables.Create(ctx, doc)
	created := err == nil
	switch {
	case created:
	case errors.Is(err, store.ErrAlreadyExists):
		if _, _, err = s.St.Observables.Update(ctx, id, func(o *domain.ObservableDoc) error {
			o.LastSeen = now
			if caseID == "" {
				return nil
			}
			for _, c := range o.CaseIDs {
				if c == caseID {
					return nil
				}
			}
			o.CaseIDs = append(o.CaseIDs, caseID)
			return nil
		}); err != nil {
			return domain.ObservableDoc{}, false, err
		}
	default:
		return domain.ObservableDoc{}, false, err
	}
	stored, _, err := s.St.Observables.Get(ctx, id)
	if err == nil && caseID != "" {
		s.Timeline(ctx, caseID, "observable.added", actor, map[string]any{"type": typ, "value": value})
	}
	return stored, created, err
}

// ValidObservableType reports whether t is a known indicator type.
func ValidObservableType(t string) bool {
	for _, x := range ObservableTypes {
		if x == t {
			return true
		}
	}
	return false
}
