// Package actions holds the action framework of spec section 10: manifests, the
// registry ("extend by registry", rule 3.1.10), the built-in Go actions and the
// loader for integration packages whose actions run in the Python runner.
package actions

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/jsonschema"
)

// Classes decide which worker pool and NATS subject runs an action.
const (
	ClassBuiltin = "builtin"
	ClassPython  = "python"
)

// Limits are resource limits for one execution of an action.
type Limits struct {
	CPU      float64  `json:"cpu,omitempty" yaml:"cpu"`
	MemoryMB int      `json:"memory_mb,omitempty" yaml:"memory_mb"`
	TimeoutS int      `json:"timeout_s,omitempty" yaml:"timeout_s"`
	Egress   []string `json:"egress,omitempty" yaml:"egress"`
}

// Manifest describes an action (spec section 10). Description, SideEffect, Risk and
// SupportsDryRun are required: they drive approvals and later AI tool use (spec 20).
type Manifest struct {
	ID              string         `json:"id" yaml:"id"`
	Version         int            `json:"version" yaml:"version"`
	Title           string         `json:"title,omitempty" yaml:"title"`
	Description     string         `json:"description" yaml:"description"`
	SideEffect      string         `json:"side_effect" yaml:"side_effect"` // read | write | destructive
	Risk            string         `json:"risk" yaml:"risk"`               // low | medium | high
	SupportsDryRun  bool           `json:"supports_dry_run" yaml:"supports_dry_run"`
	Class           string         `json:"class" yaml:"class"` // builtin | python
	ParamsSchema    map[string]any `json:"params_schema,omitempty" yaml:"params_schema"`
	OutputSchema    map[string]any `json:"output_schema,omitempty" yaml:"output_schema"`
	Secrets         []string       `json:"secrets,omitempty" yaml:"secrets"`
	RetryableErrors []string       `json:"retryable_errors,omitempty" yaml:"retryable_errors"`
	AtMostOnce      bool           `json:"at_most_once" yaml:"at_most_once"`
	Limits          Limits         `json:"limits" yaml:"limits"`
	Connector       string         `json:"connector,omitempty" yaml:"-"`           // integration package, for python actions
	Entrypoint      string         `json:"entrypoint,omitempty" yaml:"entrypoint"` // python: "actions/case.py:create"
	NeedsConnection bool           `json:"needs_connection,omitempty" yaml:"needs_connection"`
	ApprovalPolicy  map[string]any `json:"approval_policy,omitempty" yaml:"approval_policy"` // e.g. {recipients_over: 50}
}

// Validate checks the required manifest fields.
func (m Manifest) Validate() error {
	var problems []string
	if m.ID == "" || strings.ContainsAny(m.ID, " \t") {
		problems = append(problems, "id is required and has no spaces")
	}
	if strings.TrimSpace(m.Description) == "" {
		problems = append(problems, "description is required")
	}
	switch m.SideEffect {
	case "read", "write", "destructive":
	default:
		problems = append(problems, "side_effect must be read, write or destructive")
	}
	switch m.Risk {
	case "low", "medium", "high":
	default:
		problems = append(problems, "risk must be low, medium or high")
	}
	switch m.Class {
	case ClassBuiltin, ClassPython:
	default:
		problems = append(problems, "class must be builtin or python")
	}
	if m.Class == ClassPython && m.Entrypoint == "" {
		problems = append(problems, "python actions need an entrypoint")
	}
	if len(problems) > 0 {
		return fmt.Errorf("action %q: %s", m.ID, strings.Join(problems, "; "))
	}
	return nil
}

// Retryable reports whether an error code is declared retryable by the manifest.
func (m Manifest) Retryable(code string) bool {
	for _, c := range m.RetryableErrors {
		if c == code || (c == "http_5xx" && strings.HasPrefix(code, "http_5")) {
			return true
		}
	}
	return false
}

// Connection is the resolved connection a connector action talks through.
type Connection struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Config  map[string]any    `json:"config,omitempty"`
	Secrets map[string]string `json:"-"` // decrypted inside the worker only
}

// Input is everything an action receives.
type Input struct {
	ExecutionID    string
	WorkflowID     string
	NodeID         string
	AlertID        string
	Attempt        int
	Item           int
	IdempotencyKey string
	Params         map[string]any // secrets already resolved
	Connection     *Connection
	DryRun         bool
	Traceparent    string
	Log            func(msg string)
}

// Error is an action failure (spec section 10).
type Error struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errf builds an *Error.
func Errf(code string, retryable bool, format string, a ...any) *Error {
	return &Error{Code: code, Retryable: retryable, Message: fmt.Sprintf(format, a...)}
}

// AsError converts any error into an *Error (unknown errors are not retryable).
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "timeout", Retryable: true, Message: "the action did not finish within its timeout"}
	}
	return &Error{Code: "internal", Message: err.Error()}
}

// Action is a runnable action.
type Action interface {
	Manifest() Manifest
	Execute(ctx context.Context, in Input) (any, error)
}

// Executor runs actions of one class that are not Go code (the Python runner).
type Executor interface {
	Execute(ctx context.Context, m Manifest, in Input) (any, error)
}

// Func adapts a function to Action.
type Func struct {
	M  Manifest
	Fn func(ctx context.Context, in Input) (any, error)
}

func (f Func) Manifest() Manifest                                 { return f.M }
func (f Func) Execute(ctx context.Context, in Input) (any, error) { return f.Fn(ctx, in) }

type remote struct {
	m    Manifest
	exec Executor
}

func (r remote) Manifest() Manifest { return r.m }
func (r remote) Execute(ctx context.Context, in Input) (any, error) {
	if r.exec == nil {
		return nil, Errf("runner_unavailable", true, "no runner is configured for %s actions", r.m.Class)
	}
	return r.exec.Execute(ctx, r.m, in)
}

// Registry maps action ids to actions. It is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	actions map[string]Action
	conns   map[string]ConnectorInfo
}

// ConnectorInfo describes an integration package (GET /connectors).
type ConnectorInfo struct {
	ID               string         `json:"id"`
	Title            string         `json:"title,omitempty"`
	Description      string         `json:"description,omitempty"`
	Version          string         `json:"version,omitempty"`
	AuthType         string         `json:"auth_type,omitempty"`
	ConnectionSchema map[string]any `json:"connection_schema,omitempty"`
	Secrets          []string       `json:"secrets,omitempty"`
	Actions          []string       `json:"actions"`
	Builtin          bool           `json:"builtin"`
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{actions: map[string]Action{}, conns: map[string]ConnectorInfo{}}
}

// Register adds an action; registering an id twice is an error.
func (r *Registry) Register(a Action) error {
	m := a.Manifest()
	if err := m.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.actions[m.ID]; dup {
		return fmt.Errorf("action %q is registered twice", m.ID)
	}
	r.actions[m.ID] = a
	return nil
}

// RegisterRemote adds a manifest whose code runs in exec.
func (r *Registry) RegisterRemote(m Manifest, exec Executor) error {
	return r.Register(remote{m: m, exec: exec})
}

// SetExecutor attaches the runner to every python action registered without one.
func (r *Registry) SetExecutor(class string, exec Executor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, a := range r.actions {
		if rm, ok := a.(remote); ok && rm.m.Class == class {
			rm.exec = exec
			r.actions[id] = rm
		}
	}
}

// AddConnector records an integration package.
func (r *Registry) AddConnector(c ConnectorInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[c.ID] = c
}

// Get returns an action.
func (r *Registry) Get(id string) (Action, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.actions[id]
	return a, ok
}

// Manifests returns every manifest sorted by id.
func (r *Registry) Manifests() []Manifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Manifest, 0, len(r.actions))
	for _, a := range r.actions {
		out = append(out, a.Manifest())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Connectors returns every connector sorted by id, including a "builtin" pseudo-connector.
func (r *Registry) Connectors() []ConnectorInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	builtin := ConnectorInfo{ID: "builtin", Title: "Built-in actions", Builtin: true}
	for id, a := range r.actions {
		if a.Manifest().Class == ClassBuiltin {
			builtin.Actions = append(builtin.Actions, id)
		}
	}
	sort.Strings(builtin.Actions)
	out := []ConnectorInfo{builtin}
	for _, c := range r.conns {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Connector returns one connector.
func (r *Registry) Connector(id string) (ConnectorInfo, bool) {
	for _, c := range r.Connectors() {
		if c.ID == id {
			return c, true
		}
	}
	return ConnectorInfo{}, false
}

// Lookup implements engine.Catalog for save-time validation.
func (r *Registry) Lookup(id string) (engine.ActionInfo, bool) {
	a, ok := r.Get(id)
	if !ok {
		return engine.ActionInfo{}, false
	}
	m := a.Manifest()
	return engine.ActionInfo{
		ID: m.ID, ParamsSchema: paramsSchemaForValidation(m), SideEffect: m.SideEffect, Risk: m.Risk,
		SupportsDryRun: m.SupportsDryRun, AtMostOnce: m.AtMostOnce,
	}, true
}

// paramsSchemaForValidation allows the reserved "connection" parameter on connector actions.
func paramsSchemaForValidation(m Manifest) map[string]any {
	if m.ParamsSchema == nil || !m.NeedsConnection {
		return m.ParamsSchema
	}
	s := make(map[string]any, len(m.ParamsSchema))
	for k, v := range m.ParamsSchema {
		s[k] = v
	}
	props := map[string]any{}
	if p, ok := m.ParamsSchema["properties"].(map[string]any); ok {
		for k, v := range p {
			props[k] = v
		}
	}
	props["connection"] = map[string]any{"type": "string"}
	s["properties"] = props
	return s
}

// CheckParams validates resolved params against the manifest (run time).
func CheckParams(m Manifest, params map[string]any) *Error {
	errs := jsonschema.Validate(paramsSchemaForValidation(m), params, "params")
	if len(errs) == 0 {
		return nil
	}
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.String()
	}
	return &Error{Code: "invalid_params", Message: strings.Join(parts, "; ")}
}

// CheckOutput validates an action's output against its declared schema (spec 20.1:
// structured outputs). A violation is a non-retryable failure.
func CheckOutput(m Manifest, out any) *Error {
	errs := jsonschema.Validate(m.OutputSchema, out, "output")
	if len(errs) == 0 {
		return nil
	}
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.String()
	}
	return &Error{Code: "invalid_output", Message: strings.Join(parts, "; ")}
}
