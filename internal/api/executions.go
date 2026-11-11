package api

import (
	"net/http"
)

func (s *Server) listExecutions(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{
		fields:      map[string]string{"created_at": "created_at", "started_at": "started_at", "ended_at": "ended_at"},
		defaultName: "created_at", defaultDesc: true,
	}, "workflow_id", "status", "alert_id")
	if err != nil {
		return err
	}
	term(&q, v, "workflow_id", "workflow_id")
	term(&q, v, "status", "status")
	term(&q, v, "alert_id", "trigger.alert_id")
	page, err := s.st.Executions.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) getExecution(w http.ResponseWriter, r *http.Request) error {
	return getOne(w, r, s.st.Executions)
}

func (s *Server) listSteps(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if _, _, err := s.st.Executions.Get(r.Context(), id); err != nil {
		return err
	}
	q, _, err := listQuery(r, sortSpec{fields: map[string]string{"created_at": "created_at"}, defaultName: "created_at"})
	if err != nil {
		return err
	}
	q.Terms = map[string][]string{"execution_id": {id}}
	page, err := s.st.StepRuns.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}
