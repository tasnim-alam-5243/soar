package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/blob"
	"github.com/yourorg/soar/internal/cases"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

// maxEvidenceBytes bounds one evidence upload (email attachments, screenshots, logs).
const maxEvidenceBytes = 25 << 20

type linkIn struct {
	AlertIDs []string `json:"alert_ids"`
}

// linkAlerts links alerts to a case and keeps a snapshot of each (spec 5.2: a case
// does not lose its alerts when they expire).
func (s *Server) linkAlerts(w http.ResponseWriter, r *http.Request) error {
	var in linkIn
	if err := decode(r, &in); err != nil {
		return err
	}
	var v verr
	if len(in.AlertIDs) == 0 || len(in.AlertIDs) > 200 {
		v.add("alert_ids", "give 1 to 200 alert ids")
	}
	if err := v.err(); err != nil {
		return err
	}
	id := r.PathValue("id")
	linked, err := s.cases.LinkAlerts(r.Context(), id, in.AlertIDs, s.actor(r))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) && strings.HasPrefix(err.Error(), "alert ") {
			return unprocessable([]FieldError{{Path: "alert_ids", Message: err.Error()}})
		}
		return err
	}
	s.audit(r, "case.link_alerts", "case:"+id, nil, map[string]any{"alert_ids": linked})
	s.notifyUI("cases", id, map[string]any{"linked": linked})
	return getOne(w, r, s.st.Cases)
}

func (s *Server) caseAlerts(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if _, _, err := s.st.Cases.Get(r.Context(), id); err != nil {
		return err
	}
	q, _, err := listQuery(r, sortSpec{fields: map[string]string{"linked_at": "linked_at"}, defaultName: "linked_at"})
	if err != nil {
		return err
	}
	q.Terms = map[string][]string{"case_id": {id}}
	page, err := s.st.CaseAlerts.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

type alertCaseIn struct {
	CaseID   string `json:"case_id"`
	Title    string `json:"title"`
	Assignee string `json:"assignee"`
}

// alertToCase links an alert to an existing case, or opens a new case from it.
func (s *Server) alertToCase(w http.ResponseWriter, r *http.Request) error {
	var in alertCaseIn
	if err := decodeOptional(r, &in); err != nil {
		return err
	}
	var v verr
	v.maxLen("title", in.Title, 300)
	v.maxLen("assignee", in.Assignee, 200)
	if err := v.err(); err != nil {
		return err
	}
	ctx := r.Context()
	a, _, err := s.st.Alerts.Get(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	status := http.StatusOK
	caseID := in.CaseID
	if caseID == "" {
		title := in.Title
		if title == "" {
			title = a.Title
		}
		c := domain.Case{Meta: domain.NewMeta(domain.PrefixCase), Title: title, Severity: a.Severity, Status: "open", Assignee: in.Assignee, Tags: a.Tags}
		c.ID = domain.DeterministicID(domain.PrefixCase, "from-alert", a.ID) // one case per alert, even on retries
		created, isNew, err := s.cases.Create(ctx, c, nil, s.actor(r))
		if err != nil {
			return err
		}
		if isNew {
			status = http.StatusCreated
		}
		caseID = created.ID
	}
	if _, err := s.cases.LinkAlerts(ctx, caseID, []string{a.ID}, s.actor(r)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return unprocessable([]FieldError{{Path: "case_id", Message: "no such case"}})
		}
		return err
	}
	for _, o := range a.Observables {
		if cases.ValidObservableType(o.Type) {
			if _, _, err := s.cases.AddObservable(ctx, caseID, o.Type, o.Value, s.actor(r)); err != nil {
				s.log.WarnContext(ctx, "could not link observable to the case", "case", caseID, "error", err.Error())
			}
		}
	}
	doc, ver, err := s.st.Cases.Get(ctx, caseID)
	if err != nil {
		return err
	}
	s.audit(r, "alert.to_case", "case:"+caseID, nil, map[string]any{"alert_id": a.ID})
	s.notifyUI("cases", caseID, map[string]any{"alert_id": a.ID})
	w.Header().Set("Location", Prefix+"/cases/"+caseID)
	return writeResource(w, status, doc, ver)
}

type taskPatch struct {
	Title       *string    `json:"title"`
	Description *string    `json:"description"`
	Status      *string    `json:"status"`
	Assignee    *string    `json:"assignee"`
	DueAt       *time.Time `json:"due_at"`
}

func (s *Server) patchTask(w http.ResponseWriter, r *http.Request) error {
	var in taskPatch
	if err := decode(r, &in); err != nil {
		return err
	}
	var v verr
	if in.Title != nil {
		v.required("title", *in.Title)
		v.maxLen("title", *in.Title, 300)
	}
	if in.Status != nil {
		v.oneOf("status", *in.Status, "open", "in_progress", "done", "cancelled")
	}
	if in.Assignee != nil {
		v.maxLen("assignee", *in.Assignee, 200)
	}
	if err := v.err(); err != nil {
		return err
	}
	caseID := r.PathValue("id")
	doc, ver, err := mutate(r, s.st.CaseTasks, r.PathValue("task_id"), func(t *domain.CaseTask) error {
		if t.CaseID != caseID {
			return store.ErrNotFound
		}
		if in.Title != nil {
			t.Title = *in.Title
		}
		if in.Description != nil {
			t.Description = *in.Description
		}
		if in.Status != nil {
			t.Status = *in.Status
		}
		if in.Assignee != nil {
			t.Assignee = *in.Assignee
		}
		if in.DueAt != nil {
			t.DueAt = in.DueAt
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.cases.Timeline(r.Context(), caseID, "task.updated", s.actor(r), map[string]any{"task_id": doc.ID, "status": doc.Status})
	return writeResource(w, http.StatusOK, doc, ver)
}

// uploadEvidence stores the raw request body in soar-evidence and records it on the case.
// The object is written first, then the case (spec 5.1 write order).
func (s *Server) uploadEvidence(w http.ResponseWriter, r *http.Request) error {
	if s.blob == nil {
		return newProblem(http.StatusServiceUnavailable, "blob-unavailable", "Object storage unavailable", "configure SOAR_BLOB_ENDPOINT (S3-compatible object storage) to store evidence")
	}
	ctx := r.Context()
	caseID := r.PathValue("id")
	if _, _, err := s.st.Cases.Get(ctx, caseID); err != nil {
		return err
	}
	name := path.Base(strings.TrimSpace(r.URL.Query().Get("filename")))
	if name == "" || name == "." || name == "/" || len(name) > 255 {
		return unprocessable([]FieldError{{Path: "filename", Message: "a file name (query parameter) is required"}})
	}
	ct := r.Header.Get("Content-Type")
	if _, _, err := mime.ParseMediaType(ct); err != nil {
		ct = "application/octet-stream"
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxEvidenceBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return badRequest("the request body (the file) is empty")
	}
	ref, err := blob.PutBytes(ctx, s.blob, blob.BucketEvidence, data, ct)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	ev := domain.Evidence{
		ID: domain.NewID("evd"), Name: name, Ref: ref.String(), SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data)),
		ContentType: ct, AddedBy: s.actor(r), AddedAt: s.now(),
	}
	if _, _, err := s.st.Cases.Update(ctx, caseID, func(c *domain.Case) error { c.Evidence = append(c.Evidence, ev); return nil }); err != nil {
		return err
	}
	s.cases.Timeline(ctx, caseID, "evidence.added", s.actor(r), map[string]any{"name": name, "sha256": ev.SHA256, "size": ev.Size})
	s.audit(r, "case.evidence_add", "case:"+caseID, nil, map[string]any{"sha256": ev.SHA256})
	return writeJSON(w, http.StatusCreated, ev)
}

type evidenceLink struct {
	URL       string          `json:"url"`
	ExpiresAt time.Time       `json:"expires_at"`
	Evidence  domain.Evidence `json:"evidence"`
}

// evidenceURL returns a short-lived presigned download URL (spec 5.1).
func (s *Server) evidenceURL(w http.ResponseWriter, r *http.Request) error {
	if s.blob == nil {
		return newProblem(http.StatusServiceUnavailable, "blob-unavailable", "Object storage unavailable", "configure SOAR_BLOB_ENDPOINT (S3-compatible object storage)")
	}
	c, _, err := s.st.Cases.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	for _, ev := range c.Evidence {
		if ev.ID != r.PathValue("eid") {
			continue
		}
		ref, err := blob.ParseRef(ev.Ref)
		if err != nil {
			return err
		}
		ttl := 5 * time.Minute
		u, err := s.blob.PresignGet(r.Context(), ref, ttl)
		if err != nil {
			return newProblem(http.StatusServiceUnavailable, "presign-unavailable", "Download link unavailable", err.Error())
		}
		s.audit(r, "case.evidence_read", "case:"+c.ID, nil, nil)
		return writeJSON(w, http.StatusOK, evidenceLink{URL: u, ExpiresAt: s.now().Add(ttl), Evidence: ev})
	}
	return notFound("no such evidence")
}

// notifyUI forwards a change to stream clients of this replica and, through NATS, all others.
func (s *Server) notifyUI(topic, id string, data map[string]any) {
	ev := UIEvent{Topic: topic, ID: id, Data: data, At: s.now()}
	if n, ok := s.pub.(interface{ Notify(string, any) }); ok {
		n.Notify("ui."+topic, ev) // the API's own subscription delivers it locally too
		return
	}
	s.events.Publish(ev)
}
