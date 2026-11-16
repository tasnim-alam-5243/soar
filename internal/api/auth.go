package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/yourorg/soar/internal/audit"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
)

type ctxKey int

const (
	principalKey ctxKey = iota
	auditKey
)

// devPrincipal acts for every request when authentication is disabled (tests, local dev).
var devPrincipal = security.Principal{ID: "dev", Name: "dev", Type: "user", Roles: []string{security.RoleAdmin}, ActorType: "user"}

// WithAuth turns on authentication. Without it every request acts as an admin, which
// is only acceptable for tests and a localhost-only development stack.
func (s *Server) WithAuth(a *security.Authenticator) *Server { s.auth = a; return s }

func principalFrom(r *http.Request) security.Principal {
	if p, ok := r.Context().Value(principalKey).(security.Principal); ok {
		return p
	}
	return devPrincipal
}

// actor identifies the caller for audit trails, created_by and approvals.
func (s *Server) actor(r *http.Request) string { return principalFrom(r).Name }

// permOverrides map specific operations to permissions; everything else derives
// "<resource>:read" for GET and "<resource>:write" otherwise.
var permOverrides = map[string]string{
	"GET /openapi.yaml":                 "",
	"GET /me":                           "",
	"POST /workflows/validate":          "workflows:read",
	"POST /workflows/{id}/run":          "workflows:run",
	"POST /executions/{id}/cancel":      "executions:cancel",
	"POST /executions/{id}/retry":       "executions:run",
	"POST /approvals/{id}/decision":     "approvals:decide",
	"POST /mappings/test":               "mappings:read",
	"POST /filters/test":                "filters:read",
	"POST /filters/simulate":            "filters:read",
	"POST /sources/{id}/test":           "sources:read",
	"POST /connections/{id}/test":       "connections:read",
	"GET /secrets":                      "secrets:read_metadata",
	"GET /stream":                       "executions:read",
	"GET /roles":                        "principals:read",
	"GET /audit":                        "audit:read",
	"GET /dlq":                          "queue:read",
	"POST /dlq/{id}/replay":             "queue:write",
	"GET /queues/stats":                 "queue:read",
	"GET /connectors":                   "connectors:read",
	"GET /connectors/{id}/actions":      "connectors:read",
	"POST /alerts/{id}/case":            "cases:write",
	"POST /cases/{id}/alerts":           "cases:write",
	"POST /cases/{id}/evidence":         "cases:write",
	"GET /cases/{id}/evidence/{eid}":    "cases:read",
	"POST /cases/{id}/sync":             "cases:write",
	"PATCH /cases/{id}/tasks/{task_id}": "cases:write",
	"GET /cases/{id}/alerts":            "cases:read",
	"GET /dashboard":                    "alerts:read",
	"GET /assignees":                    "alerts:write", // only people who can assign need the list
}

// PermFor returns the permission an operation needs ("" = any authenticated caller).
func PermFor(method, path string) string {
	if p, ok := permOverrides[method+" "+path]; ok {
		return p
	}
	res := strings.Split(strings.Trim(path, "/"), "/")[0]
	switch res {
	case "dlq", "queues":
		res = "queue"
	}
	if method == http.MethodGet {
		return res + ":read"
	}
	return res + ":write"
}

type auditInfo struct {
	action, resource      string
	beforeHash, afterHash string
}

// audit names the mutation of the current request for its audit record; the record
// itself is written by the middleware once the request succeeds.
func (s *Server) audit(r *http.Request, action, resource string, before, after any) {
	ai, ok := r.Context().Value(auditKey).(*auditInfo)
	if !ok {
		return
	}
	ai.action, ai.resource = action, resource
	if before != nil {
		ai.beforeHash = hashOf(before)
	}
	if after != nil {
		ai.afterHash = hashOf(after)
	}
}

func hashOf(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) {
	if w.status == 0 {
		w.status = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush lets server-sent events through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection (per-request write deadlines).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// guard authenticates, authorises and audits one route.
func (s *Server) guard(rt Route, h http.HandlerFunc) http.HandlerFunc {
	perm := PermFor(rt.Method, rt.Path)
	return func(w http.ResponseWriter, r *http.Request) {
		p := devPrincipal
		if s.auth != nil && !(rt.Method == http.MethodGet && rt.Path == "/openapi.yaml") {
			tok := bearer(r)
			if tok == "" && rt.Path == "/stream" {
				tok = r.URL.Query().Get("access_token") // EventSource cannot send headers
			}
			var err error
			p, err = s.auth.Authenticate(r.Context(), tok)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="soar"`)
				writeProblem(r.Context(), w, newProblem(http.StatusUnauthorized, "unauthenticated", "Unauthenticated", "a valid bearer token (OIDC access token or API key) is required"))
				return
			}
			if perm != "" && !p.Can(perm) {
				writeProblem(r.Context(), w, newProblem(http.StatusForbidden, "forbidden", "Forbidden", "your roles do not allow "+perm))
				return
			}
		}
		ctx := context.WithValue(r.Context(), principalKey, p)
		ai := &auditInfo{}
		ctx = context.WithValue(ctx, auditKey, ai)
		ctx = obs.WithAttrs(ctx, slogActor(p))
		sw := &statusWriter{ResponseWriter: w}
		h(sw, r.WithContext(ctx))
		if r.Method != http.MethodGet && r.Method != http.MethodHead && sw.status > 0 && sw.status < 400 && !strings.HasSuffix(rt.Path, "/test") &&
			rt.Path != "/workflows/validate" && rt.Path != "/filters/simulate" {
			s.writeAudit(r, p, rt, ai)
		}
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

func (s *Server) writeAudit(r *http.Request, p security.Principal, rt Route, ai *auditInfo) {
	action, resource := ai.action, ai.resource
	if action == "" {
		action = rt.Method + " " + rt.Path
	}
	if resource == "" {
		resource = strings.TrimPrefix(r.URL.Path, Prefix)
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	rec := domain.AuditRecord{
		Meta: domain.NewMeta("aud"), Actor: p.Name, ActorType: p.ActorType, Action: action, Resource: resource,
		BeforeHash: ai.beforeHash, AfterHash: ai.afterHash, IP: ip, RequestID: obs.RequestID(r.Context()),
	}
	if net.ParseIP(rec.IP) == nil {
		rec.IP = ""
	}
	(&audit.Auditor{Sink: s.st.Audit, Log: s.log}).Record(r.Context(), rec)
}

// canApprove reports whether the caller may decide an approval: the approvals:decide
// permission plus one of the approval's roles or names (spec 21.1 #13).
func (s *Server) canApprove(r *http.Request, a domain.Approval) bool {
	p := principalFrom(r)
	return p.Can("approvals:decide") && p.HasAny(a.Approvers)
}

// ---- principals, roles, audit, me ----

type principalIn struct {
	Type  string   `json:"type"`
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

type principalOut struct {
	domain.Principal
	APIKey string `json:"api_key,omitempty"` // shown once, on creation
}

func validateRoles(v *verr, roles []string) {
	if len(roles) == 0 {
		v.add("roles", "at least one role is required")
	}
	for i, r := range roles {
		if !security.ValidRole(r) {
			v.add("roles["+itoa(i)+"]", "unknown role %q", r)
		}
	}
}

func (s *Server) createPrincipal(w http.ResponseWriter, r *http.Request) error {
	var in principalIn
	if err := decode(r, &in); err != nil {
		return err
	}
	var v verr
	v.oneOf("type", in.Type, "user", "api_key", "service")
	v.required("name", in.Name)
	v.maxLen("name", in.Name, 200)
	validateRoles(&v, in.Roles)
	if err := v.err(); err != nil {
		return err
	}
	doc := domain.Principal{Meta: domain.NewMeta("prn"), Type: in.Type, Name: in.Name, Roles: in.Roles}
	doc.ID = domain.DeterministicID("prn", in.Type, in.Name)
	out := principalOut{}
	if in.Type != "user" {
		key, hash, err := security.NewAPIKey()
		if err != nil {
			return err
		}
		doc.KeyHash, out.APIKey = hash, key
	}
	if _, err := s.st.Principals.Create(r.Context(), doc); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return conflict("principal-exists", "a "+in.Type+" named "+in.Name+" already exists")
		}
		return err
	}
	s.audit(r, "principal.create", "principal:"+doc.ID, nil, map[string]any{"roles": doc.Roles})
	doc.KeyHash = ""
	out.Principal = doc
	w.Header().Set("Location", Prefix+"/principals/"+doc.ID)
	return writeJSON(w, http.StatusCreated, out)
}

func scrubPrincipal(p domain.Principal) domain.Principal { p.KeyHash = ""; return p }

func (s *Server) listPrincipals(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{fields: map[string]string{"name": "name", "created_at": "created_at"}, defaultName: "name"}, "type", "role")
	if err != nil {
		return err
	}
	term(&q, v, "type", "type")
	term(&q, v, "role", "roles")
	page, err := s.st.Principals.List(r.Context(), q)
	if err != nil {
		return err
	}
	for i := range page.Items {
		page.Items[i] = scrubPrincipal(page.Items[i])
	}
	return writePage(w, page)
}

func (s *Server) getPrincipal(w http.ResponseWriter, r *http.Request) error {
	doc, ver, err := s.st.Principals.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeResource(w, http.StatusOK, scrubPrincipal(doc), ver)
}

type principalPatch struct {
	Roles    *[]string `json:"roles"`
	Disabled *bool     `json:"disabled"`
}

func (s *Server) patchPrincipal(w http.ResponseWriter, r *http.Request) error {
	var in principalPatch
	if err := decode(r, &in); err != nil {
		return err
	}
	var v verr
	if in.Roles != nil {
		validateRoles(&v, *in.Roles)
	}
	if err := v.err(); err != nil {
		return err
	}
	doc, ver, err := mutate(r, s.st.Principals, r.PathValue("id"), func(p *domain.Principal) error {
		if in.Roles != nil {
			p.Roles = *in.Roles
		}
		if in.Disabled != nil {
			p.Disabled = *in.Disabled
		}
		return nil
	})
	if err != nil {
		return err
	}
	if s.auth != nil {
		s.auth.Forget()
	}
	s.audit(r, "principal.update", "principal:"+doc.ID, nil, map[string]any{"roles": doc.Roles, "disabled": doc.Disabled})
	return writeResource(w, http.StatusOK, scrubPrincipal(doc), ver)
}

func (s *Server) deletePrincipal(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if principalFrom(r).ID == id {
		return conflict("self-delete", "you cannot delete the principal you are authenticated as")
	}
	if err := s.st.Principals.Delete(r.Context(), id); err != nil {
		return err
	}
	if s.auth != nil {
		s.auth.Forget()
	}
	s.audit(r, "principal.delete", "principal:"+id, nil, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listRoles(w http.ResponseWriter, _ *http.Request) error {
	perms := security.RolePermissions()
	type role struct {
		Name        string   `json:"name"`
		Permissions []string `json:"permissions"`
	}
	items := make([]role, 0, len(security.AllRoles))
	for _, r := range security.AllRoles {
		items = append(items, role{Name: r, Permissions: perms[r]})
	}
	return writePage(w, store.Page[role]{Items: items})
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{fields: map[string]string{"created_at": "created_at"}, defaultName: "created_at", defaultDesc: true},
		"actor", "action", "resource", "from", "to")
	if err != nil {
		return err
	}
	for _, f := range []string{"actor", "action", "resource"} {
		term(&q, v, f, f)
	}
	if err := timeRange(&q, v, "created_at"); err != nil {
		return err
	}
	page, err := s.st.Audit.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) error {
	p := principalFrom(r)
	perms := map[string]bool{}
	for _, res := range []string{
		"workflows", "executions", "approvals", "alerts", "cases", "observables", "sources", "mappings",
		"filters", "lists", "connections", "connectors", "secrets", "queue", "principals", "audit",
	} {
		for _, act := range []string{"read", "write", "run", "cancel", "decide"} {
			if p.Can(res + ":" + act) {
				perms[res+":"+act] = true
			}
		}
	}
	list := make([]string, 0, len(perms))
	for k := range perms {
		list = append(list, k)
	}
	sortStrings(list)
	return writeJSON(w, http.StatusOK, map[string]any{
		"id": p.ID, "name": p.Name, "type": p.Type, "roles": p.Roles, "permissions": list,
		"auth_enabled": s.auth != nil,
	})
}

// BootstrapPrincipalID is the id of the principal SOAR_BOOTSTRAP_API_KEY installs.
var BootstrapPrincipalID = domain.DeterministicID("prn", "api_key", "bootstrap-admin")

// EnsureBootstrapKey creates an admin API-key principal from SOAR_BOOTSTRAP_API_KEY
// once, so a fresh installation can be administered before OIDC users exist. Later
// starts only follow a changed key (rotation); what an admin did to the principal
// stays: a disabled bootstrap key is not re-enabled and changed roles are kept. It
// reports whether the principal is disabled.
func EnsureBootstrapKey(ctx context.Context, st *store.Store, key string) (disabled bool, err error) {
	if key == "" {
		return false, nil
	}
	hash := security.HashAPIKey(key)
	cur, _, err := st.Principals.Get(ctx, BootstrapPrincipalID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		doc := domain.Principal{Meta: domain.NewMeta("prn"), Type: "api_key", Name: "bootstrap-admin", Roles: []string{security.RoleAdmin}, KeyHash: hash}
		doc.ID = BootstrapPrincipalID
		if _, err := st.Principals.Create(ctx, doc); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return false, err
		}
		return false, nil
	case err != nil:
		return false, err
	case cur.KeyHash == hash:
		return cur.Disabled, nil
	}
	p, _, err := st.Principals.Update(ctx, BootstrapPrincipalID, func(p *domain.Principal) error {
		p.KeyHash = hash // a rotated key replaces the old one; Disabled and Roles are left alone
		return nil
	})
	return p.Disabled, err
}
