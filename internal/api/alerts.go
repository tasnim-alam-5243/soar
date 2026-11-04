package api

import (
	"net/http"
	"strconv"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

var observableTypes = []string{"ip", "domain", "url", "hash", "email", "host", "user", "file_path"}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{
		fields:      map[string]string{"received_at": "received_at", "severity": "severity", "occurrence_count": "occurrence_count", "last_seen": "last_seen"},
		defaultName: "received_at", defaultDesc: true,
	}, "source_id", "system", "kind", "status", "verdict", "severity", "min_severity", "case_id", "from", "to")
	if err != nil {
		return err
	}
	for _, f := range []string{"source_id", "system", "kind", "status", "verdict", "severity", "case_id"} {
		term(&q, v, f, f)
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
