package api

import "net/http"

func (s *Server) listObservables(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{
		fields:      map[string]string{"last_seen": "last_seen", "first_seen": "first_seen"},
		defaultName: "last_seen", defaultDesc: true,
	}, "type", "value", "case_id")
	if err != nil {
		return err
	}
	term(&q, v, "type", "type")
	term(&q, v, "value", "value")
	term(&q, v, "case_id", "case_ids")
	page, err := s.st.Observables.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) getObservable(w http.ResponseWriter, r *http.Request) error {
	return getOne(w, r, s.st.Observables)
}
