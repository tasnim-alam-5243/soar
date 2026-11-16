// Package api serves the REST API (/api/v1). The contract is api/openapi.yaml;
// contract tests fail if a route and the document ever disagree.
//
// Every route is authenticated (OIDC access token or API key) and authorised by
// role (spec section 13); every successful mutation is audited.
package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	apispec "github.com/yourorg/soar/api"
	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/blob"
	"github.com/yourorg/soar/internal/cases"
	"github.com/yourorg/soar/internal/security"
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
	st         *store.Store
	log        *slog.Logger
	now        func() time.Time
	q          QueueOps
	pub        Publisher
	reg        *actions.Registry
	secrets    *security.SecretStore
	auth       *security.Authenticator
	cases      *cases.Service
	blob       blob.Blob
	events     *Broker
	partitions int
	testWait   time.Duration // how long POST /connections/{id}/test waits (tests shorten it)
}

// New builds a Server.
func New(st *store.Store, log *slog.Logger) *Server {
	return &Server{st: st, log: log, now: func() time.Time { return time.Now().UTC() }, cases: cases.New(st, log), partitions: 8, events: NewBroker()}
}

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
		{"GET", "/me", s.me},
		{"GET", "/stream", s.stream},
		{"GET", "/dashboard", s.dashboard},

		{"GET", "/executions", s.listExecutions},
		{"GET", "/executions/{id}", s.getExecution},
		{"GET", "/executions/{id}/steps", s.listSteps},
		{"POST", "/executions/{id}/cancel", s.cancelExecution},
		{"POST", "/executions/{id}/retry", s.retryExecution},

		{"GET", "/approvals", s.listApprovals},
		{"GET", "/approvals/{id}", s.getApproval},
		{"POST", "/approvals/{id}/decision", s.decideApproval},

		{"GET", "/alerts", s.listAlerts},
		{"POST", "/alerts", s.createAlert},
		{"GET", "/alerts/{id}", s.getAlert},
		{"PATCH", "/alerts/{id}", s.patchAlert},
		{"POST", "/alerts/{id}/case", s.alertToCase},
		{"GET", "/assignees", s.assignees},

		{"POST", "/cases", s.createCase},
		{"GET", "/cases", s.listCases},
		{"GET", "/cases/{id}", s.getCase},
		{"PATCH", "/cases/{id}", s.patchCase},
		{"POST", "/cases/{id}/tasks", s.createTask},
		{"GET", "/cases/{id}/tasks", s.listTasks},
		{"PATCH", "/cases/{id}/tasks/{task_id}", s.patchTask},
		{"POST", "/cases/{id}/alerts", s.linkAlerts},
		{"GET", "/cases/{id}/alerts", s.caseAlerts},
		{"POST", "/cases/{id}/evidence", s.uploadEvidence},
		{"GET", "/cases/{id}/evidence/{eid}", s.evidenceURL},
		{"POST", "/cases/{id}/observables", s.addCaseObservable},
		{"GET", "/cases/{id}/timeline", s.caseTimeline},

		{"GET", "/observables", s.listObservables},
		{"GET", "/observables/{id}", s.getObservable},

		{"POST", "/sources", s.createSource},
		{"GET", "/sources", s.listSources},
		{"GET", "/sources/{id}", s.getSource},
		{"PATCH", "/sources/{id}", s.patchSource},
		{"POST", "/sources/{id}/test", s.testSource},
		{"GET", "/sources/{id}/health", s.sourceHealth},
		{"POST", "/mappings", s.createMapping},
		{"GET", "/mappings", s.listMappings},
		{"POST", "/mappings/test", s.testMapping},
		{"GET", "/mappings/{id}", s.getMapping},
		{"POST", "/filters", s.createFilter},
		{"GET", "/filters", s.listFilters},
		{"POST", "/filters/test", s.testFilter},
		{"GET", "/filters/{id}", s.getFilter},
		{"GET", "/lists", s.listLists},
		{"GET", "/lists/{name}", s.getList},
		{"PUT", "/lists/{name}", s.putList},
		{"GET", "/dlq", s.listDLQ},
		{"POST", "/dlq/{id}/replay", s.replayDLQ},
		{"GET", "/queues/stats", s.queueStats},

		{"POST", "/connections", s.createConnection},
		{"GET", "/connections", s.listConnections},
		{"GET", "/connections/{id}", s.getConnection},
		{"DELETE", "/connections/{id}", s.deleteConnection},
		{"POST", "/connections/{id}/test", s.testConnection},

		{"GET", "/connectors", s.listConnectors},
		{"GET", "/connectors/{id}/actions", s.connectorActions},

		{"PUT", "/secrets/{name}", s.putSecret},
		{"GET", "/secrets", s.listSecrets},
		{"DELETE", "/secrets/{name}", s.deleteSecret},

		{"POST", "/principals", s.createPrincipal},
		{"GET", "/principals", s.listPrincipals},
		{"GET", "/principals/{id}", s.getPrincipal},
		{"PATCH", "/principals/{id}", s.patchPrincipal},
		{"DELETE", "/principals/{id}", s.deletePrincipal},
		{"GET", "/roles", s.listRoles},
		{"GET", "/audit", s.listAudit},
	}
}

// Mount registers every route on mux, plus a catch-all that answers unknown
// API paths with problem+json (404, or 405 with an Allow header).
func (s *Server) Mount(mux *http.ServeMux) {
	routes := s.Routes()
	for _, rt := range routes {
		mux.HandleFunc(rt.Method+" "+Prefix+rt.Path, s.guard(rt, s.wrap(rt.H)))
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
		if !strings.HasSuffix(r.URL.Path, "/evidence") { // evidence uploads have their own, larger cap
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
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
