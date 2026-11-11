package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

type workflowIn struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Definition  map[string]any   `json:"definition"`
	Triggers    []domain.Trigger `json:"triggers"`
}

type workflowVersionIn struct {
	Description *string          `json:"description"`
	Definition  map[string]any   `json:"definition"`
	Triggers    []domain.Trigger `json:"triggers"`
}

func (in workflowIn) validate() error {
	var v verr
	v.required("name", in.Name)
	v.maxLen("name", in.Name, 200)
	v.maxLen("description", in.Description, 2000)
	if in.Definition == nil {
		v.add("definition", "is required and must be an object")
	}
	validateTriggers(&v, in.Triggers)
	return v.err()
}

func validateTriggers(v *verr, ts []domain.Trigger) {
	for i, t := range ts {
		p := "triggers[" + itoa(i) + "]"
		v.oneOf(p+".type", t.Type, "alert", "schedule", "manual", "event")
		v.maxLen(p+".match", t.Match, 4000)
		v.maxLen(p+".cron", t.Cron, 200)
	}
	for _, e := range triggerErrors(triggersAny(ts)) {
		v.add(e.Path, "%s", e.Message)
	}
}

func triggersAny(ts []domain.Trigger) []any {
	out := make([]any, len(ts))
	for i, t := range ts {
		out[i] = toJSONMap(t)
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }

func checksum(def map[string]any, triggers []domain.Trigger) string {
	b, _ := json.Marshal(struct {
		D map[string]any   `json:"d"`
		T []domain.Trigger `json:"t"`
	}{def, triggers}) // map keys marshal sorted, so equal content gives an equal hash
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// workflowID is deterministic in (name, version). Creating the same pair twice fails
// atomically in the store (a search could lag behind recent writes), so names and
// version numbers stay unique even under concurrent requests.
func workflowID(name string, version int) string {
	sum := sha256.Sum256([]byte(name + "\x00" + strconv.Itoa(version)))
	return domain.PrefixWorkflow + "_" + hex.EncodeToString(sum[:])[:26]
}

func (s *Server) workflowDoc(name, description string, version int, def map[string]any, triggers []domain.Trigger, r *http.Request) domain.Workflow {
	meta := domain.NewMeta(domain.PrefixWorkflow)
	meta.ID = workflowID(name, version)
	return domain.Workflow{
		Meta: meta, Name: name, Description: description, Version: version,
		Status: "draft", Definition: def, Triggers: triggers, Checksum: checksum(def, triggers), CreatedBy: s.actor(r),
	}
}

func (s *Server) createWorkflow(w http.ResponseWriter, r *http.Request) error {
	return createResource(s, w, r, s.st.Workflows, "/workflows", func() (domain.Workflow, string, error) {
		var in workflowIn
		if err := decode(r, &in); err != nil {
			return domain.Workflow{}, "", err
		}
		if err := in.validate(); err != nil {
			return domain.Workflow{}, "", err
		}
		_, _, err := s.st.Workflows.Get(r.Context(), workflowID(in.Name, 1))
		if err == nil {
			return domain.Workflow{}, "", conflict("workflow-exists", "a workflow named "+in.Name+" already exists; create a new version with POST /workflows/{id}/versions")
		}
		if !errors.Is(err, store.ErrNotFound) {
			return domain.Workflow{}, "", err
		}
		def, triggers, _, err := liftTriggers(in.Definition, in.Triggers)
		if err != nil {
			return domain.Workflow{}, "", err
		}
		doc := s.workflowDoc(in.Name, in.Description, 1, def, triggers, r)
		return doc, doc.ID, nil
	})
}

func (s *Server) listWorkflows(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{
		fields:      map[string]string{"created_at": "created_at", "name": "name.raw", "version": "version"},
		defaultName: "created_at", defaultDesc: true,
	}, "name", "status")
	if err != nil {
		return err
	}
	term(&q, v, "name", "name.raw")
	term(&q, v, "status", "status")
	page, err := s.st.Workflows.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) getWorkflow(w http.ResponseWriter, r *http.Request) error {
	return getOne(w, r, s.st.Workflows)
}

func (s *Server) updateWorkflow(w http.ResponseWriter, r *http.Request) error {
	var in workflowIn
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := in.validate(); err != nil {
		return err
	}
	def, triggers, _, err := liftTriggers(in.Definition, in.Triggers)
	if err != nil {
		return err
	}
	doc, ver, err := mutate(r, s.st.Workflows, r.PathValue("id"), func(d *domain.Workflow) error {
		if d.Status != "draft" {
			return conflict("immutable", "only draft versions can be edited; "+d.Status+" versions are immutable (create a new version instead)")
		}
		if in.Name != d.Name {
			return unprocessable([]FieldError{{Path: "name", Message: "a workflow's name cannot be changed"}})
		}
		d.Description, d.Definition, d.Triggers = in.Description, def, triggers
		d.Checksum = checksum(def, triggers)
		return nil
	})
	if err != nil {
		return err
	}
	return writeResource(w, http.StatusOK, doc, ver)
}

func (s *Server) newWorkflowVersion(w http.ResponseWriter, r *http.Request) error {
	return createResource(s, w, r, s.st.Workflows, "/workflows", func() (domain.Workflow, string, error) {
		var in workflowVersionIn
		if err := decode(r, &in); err != nil {
			return domain.Workflow{}, "", err
		}
		var v verr
		validateTriggers(&v, in.Triggers)
		if err := v.err(); err != nil {
			return domain.Workflow{}, "", err
		}
		src, _, err := s.st.Workflows.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			return domain.Workflow{}, "", err
		}
		latest, err := s.st.Workflows.List(r.Context(), store.ListQuery{
			Terms: map[string][]string{"name.raw": {src.Name}}, Sort: "version", Desc: true, Limit: 1,
		})
		if err != nil {
			return domain.Workflow{}, "", err
		}
		next := src.Version + 1
		if len(latest.Items) > 0 && latest.Items[0].Version >= next {
			next = latest.Items[0].Version + 1
		}
		// The search above may lag behind recent writes, so probe upwards with real-time reads.
		for ; next < src.Version+1000; next++ {
			_, _, err := s.st.Workflows.Get(r.Context(), workflowID(src.Name, next))
			if errors.Is(err, store.ErrNotFound) {
				break
			}
			if err != nil {
				return domain.Workflow{}, "", err
			}
		}
		def, triggers, desc := src.Definition, src.Triggers, src.Description
		if in.Triggers != nil {
			triggers = in.Triggers
		}
		if in.Definition != nil {
			rest, lifted, declared, err := liftTriggers(in.Definition, in.Triggers)
			if err != nil {
				return domain.Workflow{}, "", err
			}
			def = rest
			if declared {
				triggers = lifted // the new definition declares its triggers (merged with any given at top level)
			}
		} else if rest, lifted, declared, err := liftTriggers(src.Definition, triggers); err == nil && declared {
			def, triggers = rest, lifted // a version stored before triggers were lifted: normalize it now
		}
		if in.Description != nil {
			desc = *in.Description
		}
		doc := s.workflowDoc(src.Name, desc, next, def, triggers, r)
		return doc, doc.ID, nil
	})
}

func (s *Server) publishWorkflow(w http.ResponseWriter, r *http.Request) error {
	doc, ver, err := mutate(r, s.st.Workflows, r.PathValue("id"), func(d *domain.Workflow) error {
		if d.Status != "draft" {
			return conflict("not-draft", "only draft versions can be published; this one is "+d.Status)
		}
		errs := s.validateDefinition(d.Definition)
		errs = append(errs, triggerErrors(triggersAny(d.Triggers))...)
		if len(errs) > 0 {
			return unprocessable(errs)
		}
		d.Status = "published"
		d.Checksum = checksum(d.Definition, d.Triggers)
		return nil
	})
	if err != nil {
		return err
	}
	return writeResource(w, http.StatusOK, doc, ver)
}

type validateIn struct {
	Definition map[string]any `json:"definition"`
}

type validateOut struct {
	Valid  bool         `json:"valid"`
	Errors []FieldError `json:"errors"`
}

func (s *Server) validateWorkflow(w http.ResponseWriter, r *http.Request) error {
	var in validateIn
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Definition == nil {
		return unprocessable([]FieldError{{Path: "definition", Message: "is required and must be an object"}})
	}
	errs := s.validateDefinition(in.Definition)
	if errs == nil {
		errs = []FieldError{}
	}
	return writeJSON(w, http.StatusOK, validateOut{Valid: len(errs) == 0, Errors: errs})
}
