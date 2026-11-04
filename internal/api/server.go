// Package api serves the REST API (/api/v1). The contract is api/openapi.yaml;
// contract tests fail if a route and the document ever disagree.
//
// Authentication and authorisation arrive in Step 9. Until then the API is open:
// the dev Compose file publishes it on 127.0.0.1 only, so do not expose it.
package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	apispec "github.com/yourorg/soar/api"
	"github.com/yourorg/soar/internal/store"
)

// Prefix is the path prefix of every API route.
const Prefix = "/api/v1"

type handler func(w http.ResponseWriter, r *http.Request) error

// Route is one operation. Path uses net/http pattern syntax, which is also OpenAPI syntax.
type Route struct {
	Method string
	Path   string // relative to Prefix
	H      handler
}

// Server holds the dependencies of every handler.
type Server struct {
	st  *store.Store
	log *slog.Logger
	now func() time.Time
}

// New builds a Server.
func New(st *store.Store, log *slog.Logger) *Server {
	return &Server{st: st, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// actor identifies the caller for audit trails and created_by. Real identities come in Step 9.
func (s *Server) actor(*http.Request) string { return "anonymous" }

// Routes lists every operation. The contract test compares it with the OpenAPI document.
func (s *Server) Routes() []Route {
	return []Route{
		{"GET", "/openapi.yaml", s.openapi},

		{"POST", "/workflows", s.createWorkflow},
		{"GET", "/workflows", s.listWorkflows},
		{"POST", "/workflows/validate", s.validateWorkflow},
		{"GET", "/workflows/{id}", s.getWorkflow},
		{"PUT", "/workflows/{id}", s.updateWorkflow},
		{"POST", "/workflows/{id}/versions", s.newWorkflowVersion},
		{"POST", "/workflows/{id}/publish", s.publishWorkflow},
		{"POST", "/workflows/{id}/run", s.runWorkflow},

		{"GET", "/executions", s.listExecutions},
		{"GET", "/executions/{id}", s.getExecution},
		{"GET", "/executions/{id}/steps", s.listSteps},
		{"POST", "/executions/{id}/cancel", s.cancelExecution},

		{"GET", "/alerts", s.listAlerts},
		{"POST", "/alerts", s.createAlert},
		{"GET", "/alerts/{id}", s.getAlert},

		{"POST", "/cases", s.createCase},
		{"GET", "/cases", s.listCases},
		{"GET", "/cases/{id}", s.getCase},
		{"PATCH", "/cases/{id}", s.patchCase},
		{"POST", "/cases/{id}/tasks", s.createTask},
		{"GET", "/cases/{id}/tasks", s.listTasks},
		{"POST", "/cases/{id}/observables", s.addCaseObservable},
		{"GET", "/cases/{id}/timeline", s.caseTimeline},

		{"GET", "/observables", s.listObservables},
		{"GET", "/observables/{id}", s.getObservable},

		{"POST", "/connections", s.createConnection},
		{"GET", "/connections", s.listConnections},
		{"GET", "/connections/{id}", s.getConnection},
		{"DELETE", "/connections/{id}", s.deleteConnection},
	}
}

// Mount registers every route on mux, plus a catch-all that answers unknown
// API paths with problem+json (404, or 405 with an Allow header).
func (s *Server) Mount(mux *http.ServeMux) {
	routes := s.Routes()
	for _, rt := range routes {
		mux.HandleFunc(rt.Method+" "+Prefix+rt.Path, s.wrap(rt.H))
	}
	mux.HandleFunc(Prefix+"/", s.wrap(func(_ http.ResponseWriter, r *http.Request) error {
		var allowed []string
		for _, rt := range routes {
			if pathMatches(Prefix+rt.Path, r.URL.Path) {
				allowed = append(allowed, rt.Method)
			}
		}
		if len(allowed) > 0 {
			return methodNotAllowed(strings.Join(allowed, ", "))
		}
		return notFound("no such API path")
	}))
}

// pathMatches reports whether a concrete path fits a pattern with {name} segments.
func pathMatches(pattern, path string) bool {
	ps, qs := strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(strings.Trim(path, "/"), "/")
	if len(ps) != len(qs) {
		return false
	}
	for i := range ps {
		if strings.HasPrefix(ps[i], "{") {
			if qs[i] == "" {
				return false
			}
			continue
		}
		if ps[i] != qs[i] {
			return false
		}
	}
	return true
}

func (s *Server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.log.ErrorContext(r.Context(), "panic in api handler", "panic", p, "path", r.URL.Path)
				writeProblem(r.Context(), w, newProblem(http.StatusInternalServerError, "internal", "Internal error", "an unexpected error occurred; quote the request_id when reporting it"))
			}
		}()
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		err := h(w, r)
		if err == nil {
			return
		}
		p, unexpected := toProblem(err)
		if unexpected {
			s.log.ErrorContext(r.Context(), "api handler failed", "method", r.Method, "path", r.URL.Path, "error", err.Error())
		}
		writeProblem(r.Context(), w, p)
	}
}

func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Content-Type", "application/yaml")
	_, err := w.Write(apispec.Spec)
	return err
}

func (s *Server) runWorkflow(http.ResponseWriter, *http.Request) error {
	return notImplemented("running workflows needs the execution engine (Steps 6-7)")
}

func (s *Server) cancelExecution(http.ResponseWriter, *http.Request) error {
	return notImplemented("cancelling executions needs the execution engine (Steps 6-8)")
}
