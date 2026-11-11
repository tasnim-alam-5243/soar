// Package engine is the pure workflow state machine of spec section 7:
//
//	Next(workflow, state, event) -> (commands, newState)
//
// It performs no I/O and never reads the wall clock: the time of an event is part of
// the event. Everything with side effects (dispatching steps, timers, approvals,
// child executions) is returned as a Command that the orchestrator persists in its
// outbox and then carries out. Given the same inputs Next always returns the same
// outputs, so a crash can never make the engine take a different path on replay.
package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/expr"
	"github.com/yourorg/soar/internal/jsonschema"
)

// Node types (spec section 6). "agent" is reserved until AI is enabled (spec 20).
const (
	TypeAction      = "action"
	TypeBranch      = "branch"
	TypeParallel    = "parallel"
	TypeForeach     = "foreach"
	TypeWait        = "wait"
	TypeApproval    = "approval"
	TypeSet         = "set"
	TypeSubworkflow = "subworkflow"
	TypeEnd         = "end"
	TypeAgent       = "agent"
)

// Limits and defaults (spec section 7).
const (
	MaxNodes           = 500
	MaxAttempts        = 10
	DefaultAttempts    = 3
	DefaultStepTimeout = 60 * time.Second
	MaxStepTimeout     = time.Hour
	DefaultDeadline    = 24 * time.Hour
	MaxDeadline        = 30 * 24 * time.Hour
	DefaultInitial     = 2 * time.Second
	DefaultMaxBackoff  = 30 * time.Second
	DefaultApproval    = 24 * time.Hour
	MaxForeachItems    = 1000
	DefaultConcurrency = 4
	MaxConcurrency     = 50
	MaxExprLen         = 4000
	CELCostLimit       = 1_000_000
)

// DefaultApprovers are the roles that may approve unless a node says otherwise (spec 21.1 #13).
var DefaultApprovers = []string{"analyst", "engineer", "admin"}

// Vars are the CEL variables of workflow expressions (spec section 6).
var Vars = []string{"alert", "inputs", "vars", "steps", "secrets", "env", "item", "index"}

// FieldError is one validation problem addressed by a path into the definition.
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ActionInfo is what the validator and engine need to know about an action.
type ActionInfo struct {
	ID             string
	ParamsSchema   map[string]any
	SideEffect     string // read | write | destructive
	Risk           string
	SupportsDryRun bool
	AtMostOnce     bool
}

// Catalog resolves action ids. A nil Catalog skips action checks.
type Catalog interface {
	Lookup(action string) (ActionInfo, bool)
}

// RetryPolicy is a node's business-retry policy (spec 8.5, engine layer).
type RetryPolicy struct {
	MaxAttempts int           `json:"max_attempts"`
	Backoff     string        `json:"backoff"` // exponential | fixed
	Initial     time.Duration `json:"initial"`
	Max         time.Duration `json:"max"`
}

// Case is one arm of a branch node.
type Case struct {
	When *expr.Program
	Next []string
}

// Assign is one variable assignment of a set node (kept in definition order).
type Assign struct {
	Name  string
	Value *Value
}

// Input is a workflow input, computed when an execution starts.
type Input struct {
	Name     string
	From     *expr.Program
	Default  any
	Required bool
}

// Node is a compiled workflow node.
type Node struct {
	ID              string
	Type            string
	Action          string
	Params          *Value
	Retry           RetryPolicy
	Timeout         time.Duration
	IdemKey         *expr.Program
	Priority        string
	When            *expr.Program
	Next            []string // successors from next: and edges
	OnError         []string
	ContinueOnError bool

	Cases   []Case // branch
	Default []string

	Branches []string // parallel

	Items       *expr.Program // foreach
	Concurrency int

	Duration time.Duration // wait
	Until    *expr.Program

	Approvers       []string // approval
	Message         *Value
	ApprovalTimeout time.Duration
	OnTimeout       string // reject | approve
	OnReject        []string

	Assign []Assign // set

	Workflow  string // subworkflow
	SubInputs *Value

	EndStatus string // end: succeeded | failed
	EndOutput *Value

	index int
}

// Workflow is a compiled, validated definition. It is immutable and safe to share.
type Workflow struct {
	ID       string
	Name     string
	Version  int
	Inputs   []Input
	Nodes    []*Node
	Outputs  *Value
	Deadline time.Duration

	byID  map[string]*Node
	preds map[string][]string
}

// Node returns a node by id.
func (w *Workflow) Node(id string) *Node { return w.byID[id] }

var nodeIDRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

var knownTypes = map[string]bool{
	TypeAction: true, TypeBranch: true, TypeParallel: true, TypeForeach: true, TypeWait: true,
	TypeApproval: true, TypeSet: true, TypeSubworkflow: true, TypeEnd: true,
}

// fieldsByType lists the node keys each type accepts beyond the common ones, so a
// misspelled key is reported instead of silently ignored.
var commonKeys = []string{"id", "type", "name", "description", "when", "next", "on_error", "continue_on_error", "ui"}

var fieldsByType = map[string][]string{
	TypeAction:      {"action", "params", "retry", "timeout", "idempotency_key", "priority"},
	TypeBranch:      {"cases", "default"},
	TypeParallel:    {"branches"},
	TypeForeach:     {"items", "action", "params", "retry", "timeout", "idempotency_key", "priority", "concurrency"},
	TypeWait:        {"duration", "until"},
	TypeApproval:    {"approvers", "message", "timeout", "on_timeout", "on_reject"},
	TypeSet:         {"vars"},
	TypeSubworkflow: {"workflow", "inputs"},
	TypeEnd:         {"status", "output"},
}

var topKeys = map[string]bool{
	"name": true, "version": true, "description": true, "triggers": true, "inputs": true,
	"nodes": true, "edges": true, "outputs": true, "deadline": true, "ui": true,
}

type compiler struct {
	cel   *expr.Engine
	cat   Catalog
	errs  []FieldError
	nodes map[string]*Node
}

func (c *compiler) add(path, format string, a ...any) {
	c.errs = append(c.errs, FieldError{Path: path, Message: fmt.Sprintf(format, a...)})
}

// NewCEL returns the expression engine used for workflow expressions.
func NewCEL() (*expr.Engine, error) {
	e, err := expr.New(nil, Vars...)
	if err != nil {
		return nil, err
	}
	e.SetCostLimit(CELCostLimit)
	return e, nil
}

// Compile validates a definition (spec section 6, "Validation at save time") and
// returns the compiled workflow, or every problem found. Paths are relative to
// "definition". cat may be nil to skip action checks.
func Compile(def map[string]any, cat Catalog) (*Workflow, []FieldError) {
	celEng, err := NewCEL()
	if err != nil {
		return nil, []FieldError{{Path: "definition", Message: err.Error()}}
	}
	c := &compiler{cel: celEng, cat: cat, nodes: map[string]*Node{}}
	w := c.workflow(def)
	if len(c.errs) > 0 {
		return nil, c.errs
	}
	return w, nil
}

func (c *compiler) workflow(def map[string]any) *Workflow {
	w := &Workflow{Deadline: DefaultDeadline, byID: map[string]*Node{}, preds: map[string][]string{}}
	for _, k := range sortedKeys(def) {
		if !topKeys[k] {
			c.add("definition."+k, "unknown field")
		}
	}
	w.Name, _ = def["name"].(string)
	if v, ok := toInt(def["version"]); ok {
		w.Version = v
	}
	if raw, ok := def["deadline"]; ok {
		d, err := parseDurationAny(raw)
		switch {
		case err != nil:
			c.add("definition.deadline", "%v", err)
		case d <= 0 || d > MaxDeadline:
			c.add("definition.deadline", "must be between 1s and %s", fmtDuration(MaxDeadline))
		default:
			w.Deadline = d
		}
	}
	if raw, ok := def["triggers"]; ok {
		CheckTriggers(c, raw)
	}
	c.inputs(w, def["inputs"])

	rawNodes, ok := def["nodes"].([]any)
	if !ok || len(rawNodes) == 0 {
		c.add("definition.nodes", "must be a non-empty array")
		return w
	}
	if len(rawNodes) > MaxNodes {
		c.add("definition.nodes", "at most %d nodes are allowed", MaxNodes)
		return w
	}
	// first pass: ids, so references can be checked in any order
	for i, rn := range rawNodes {
		m, ok := rn.(map[string]any)
		path := fmt.Sprintf("definition.nodes[%d]", i)
		if !ok {
			c.add(path, "must be an object")
			continue
		}
		id, _ := m["id"].(string)
		switch {
		case id == "":
			c.add(path+".id", "is required")
			continue
		case !nodeIDRe.MatchString(id):
			c.add(path+".id", "must start with a letter and use only letters, digits and _ (max 64)")
			continue
		case c.nodes[id] != nil:
			c.add(path+".id", "duplicate node id %q", id)
			continue
		}
		n := &Node{ID: id, index: i}
		c.nodes[id] = n
		w.Nodes = append(w.Nodes, n)
		w.byID[id] = n
	}
	for i, rn := range rawNodes {
		m, ok := rn.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if n := c.nodes[id]; n != nil && n.index == i {
			c.node(n, m, fmt.Sprintf("definition.nodes[%d]", i))
		}
	}
	c.edges(def["edges"])
	if raw, ok := def["outputs"]; ok {
		if _, isMap := raw.(map[string]any); !isMap {
			c.add("definition.outputs", "must be an object of name: expression")
		} else {
			w.Outputs = c.value(raw, "definition.outputs", false)
		}
	}
	if len(c.errs) > 0 {
		return w
	}
	c.graph(w)
	return w
}

// CheckTriggers validates the triggers of a definition or of a workflow document.
func CheckTriggers(c interface {
	add(path, format string, a ...any)
}, raw any,
) {
	list, ok := raw.([]any)
	if !ok {
		c.add("definition.triggers", "must be an array")
		return
	}
	eng, _ := expr.New(nil, "alert", "event")
	for i, t := range list {
		p := fmt.Sprintf("definition.triggers[%d]", i)
		m, ok := t.(map[string]any)
		if !ok {
			c.add(p, "must be an object")
			continue
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "alert", "manual", "event":
		case "schedule":
			cron, _ := m["cron"].(string)
			if cron == "" {
				c.add(p+".cron", "is required for schedule triggers")
			} else if _, err := ParseCron(cron); err != nil {
				c.add(p+".cron", "%v", err)
			}
		default:
			c.add(p+".type", "must be alert, schedule, manual or event")
		}
		if match, _ := m["match"].(string); match != "" {
			if len(match) > MaxExprLen {
				c.add(p+".match", "is longer than %d characters", MaxExprLen)
			} else if _, err := eng.Compile(match); err != nil {
				c.add(p+".match", "does not compile: %v", err)
			}
		}
	}
}

// DefinitionTriggerErrors validates triggers written inside a definition; paths start
// with definition.triggers.
func DefinitionTriggerErrors(raw any) []FieldError {
	c := &compiler{}
	CheckTriggers(c, raw)
	return c.errs
}

// TriggerErrors validates triggers stored outside the definition (the workflow document).
func TriggerErrors(raw []any) []FieldError {
	c := &compiler{}
	CheckTriggers(c, raw)
	for i := range c.errs {
		c.errs[i].Path = strings.Replace(c.errs[i].Path, "definition.triggers", "triggers", 1)
	}
	return c.errs
}

func (c *compiler) inputs(w *Workflow, raw any) {
	if raw == nil {
		return
	}
	list, ok := raw.([]any)
	if !ok {
		c.add("definition.inputs", "must be an array")
		return
	}
	seen := map[string]bool{}
	for i, r := range list {
		p := fmt.Sprintf("definition.inputs[%d]", i)
		m, ok := r.(map[string]any)
		if !ok {
			c.add(p, "must be an object")
			continue
		}
		for _, k := range sortedKeys(m) {
			switch k {
			case "name", "from", "default", "required", "description", "type":
			default:
				c.add(p+"."+k, "unknown field")
			}
		}
		in := Input{Default: m["default"]}
		in.Name, _ = m["name"].(string)
		in.Required, _ = m["required"].(bool)
		if !nodeIDRe.MatchString(in.Name) {
			c.add(p+".name", "is required (letters, digits and _)")
		} else if seen[in.Name] {
			c.add(p+".name", "duplicate input %q", in.Name)
		}
		seen[in.Name] = true
		if from, ok := m["from"].(string); ok && from != "" {
			in.From = c.expr(from, p+".from")
		}
		w.Inputs = append(w.Inputs, in)
	}
}

func (c *compiler) node(n *Node, m map[string]any, path string) {
	typ, _ := m["type"].(string)
	action, _ := m["action"].(string)
	if typ == "" && action != "" {
		typ = TypeAction
	}
	switch {
	case typ == TypeAgent:
		c.add(path+".type", "node type %q is reserved and not enabled yet", TypeAgent)
		return
	case typ == "":
		c.add(path+".type", "is required (or give an action)")
		return
	case !knownTypes[typ]:
		c.add(path+".type", "unknown node type %q", typ)
		return
	}
	n.Type = typ
	allowed := map[string]bool{}
	for _, k := range commonKeys {
		allowed[k] = true
	}
	for _, k := range fieldsByType[typ] {
		allowed[k] = true
	}
	for _, k := range sortedKeys(m) {
		if !allowed[k] {
			c.add(path+"."+k, "is not a field of %s nodes", typ)
		}
	}

	if s, ok := m["when"].(string); ok && s != "" {
		n.When = c.expr(s, path+".when")
	}
	n.Next = c.targets(m["next"], path+".next", n.ID)
	n.OnError = c.targets(m["on_error"], path+".on_error", n.ID)
	n.ContinueOnError, _ = m["continue_on_error"].(bool)

	switch typ {
	case TypeAction, TypeForeach:
		c.actionFields(n, m, path, action)
	case TypeBranch:
		cases, _ := m["cases"].([]any)
		if len(cases) == 0 {
			c.add(path+".cases", "must be a non-empty array of {when, next}")
		}
		for j, rc := range cases {
			cp := fmt.Sprintf("%s.cases[%d]", path, j)
			cm, ok := rc.(map[string]any)
			if !ok {
				c.add(cp, "must be an object")
				continue
			}
			when, _ := cm["when"].(string)
			if when == "" {
				c.add(cp+".when", "is required")
				continue
			}
			n.Cases = append(n.Cases, Case{When: c.expr(when, cp+".when"), Next: c.targets(cm["next"], cp+".next", n.ID)})
		}
		n.Default = c.targets(m["default"], path+".default", n.ID)
		if len(n.Next) > 0 {
			c.add(path+".next", "branch nodes route through cases and default, not next")
		}
	case TypeParallel:
		n.Branches = c.targets(m["branches"], path+".branches", n.ID)
		if len(n.Branches) == 0 && len(n.Next) == 0 {
			c.add(path+".branches", "must list at least one node")
		}
	case TypeWait:
		if raw, ok := m["duration"]; ok {
			d, err := parseDurationAny(raw)
			if err != nil || d <= 0 || d > MaxDeadline {
				c.add(path+".duration", "must be a positive duration up to %s (for example 30s, 5m, 2h, 1d)", fmtDuration(MaxDeadline))
			}
			n.Duration = d
		}
		if s, ok := m["until"].(string); ok && s != "" {
			n.Until = c.expr(s, path+".until")
		}
		if (n.Duration == 0) == (n.Until == nil) {
			c.add(path, "give exactly one of duration or until")
		}
	case TypeApproval:
		n.Approvers = DefaultApprovers
		if raw, ok := m["approvers"]; ok {
			list := toStrings(raw)
			if len(list) == 0 {
				c.add(path+".approvers", "must be a non-empty list of roles, user names (user:<name>) or principal ids (oidc:<sub>, prn_...)")
			}
			n.Approvers = list
		}
		if raw, ok := m["message"]; ok {
			n.Message = c.value(raw, path+".message", false)
		}
		n.ApprovalTimeout = DefaultApproval
		if raw, ok := m["timeout"]; ok {
			d, err := parseDurationAny(raw)
			if err != nil || d <= 0 || d > MaxDeadline {
				c.add(path+".timeout", "must be a positive duration up to %s", fmtDuration(MaxDeadline))
			}
			n.ApprovalTimeout = d
		}
		n.OnTimeout = "reject"
		if s, ok := m["on_timeout"].(string); ok {
			if s != "reject" && s != "approve" {
				c.add(path+".on_timeout", "must be reject or approve")
			}
			n.OnTimeout = s
		}
		n.OnReject = c.targets(m["on_reject"], path+".on_reject", n.ID)
	case TypeSet:
		vars, ok := m["vars"].(map[string]any)
		if !ok || len(vars) == 0 {
			c.add(path+".vars", "must be a non-empty object of name: expression")
		}
		for _, k := range sortedKeys(vars) {
			if !nodeIDRe.MatchString(k) {
				c.add(path+".vars."+k, "variable names use letters, digits and _")
				continue
			}
			n.Assign = append(n.Assign, Assign{Name: k, Value: c.value(vars[k], path+".vars."+k, false)})
		}
	case TypeSubworkflow:
		n.Workflow, _ = m["workflow"].(string)
		if n.Workflow == "" {
			c.add(path+".workflow", "is required (the name of a published workflow)")
		}
		if raw, ok := m["inputs"]; ok {
			if _, isMap := raw.(map[string]any); !isMap {
				c.add(path+".inputs", "must be an object")
			} else {
				n.SubInputs = c.value(raw, path+".inputs", false)
			}
		}
	case TypeEnd:
		n.EndStatus = "succeeded"
		if s, ok := m["status"].(string); ok {
			if s != "succeeded" && s != "failed" {
				c.add(path+".status", "must be succeeded or failed")
			}
			n.EndStatus = s
		}
		if raw, ok := m["output"]; ok {
			n.EndOutput = c.value(raw, path+".output", false)
		}
		if len(n.Next) > 0 {
			c.add(path+".next", "end nodes have no successors")
		}
	}
}

func (c *compiler) actionFields(n *Node, m map[string]any, path, action string) {
	n.Action = action
	if action == "" {
		c.add(path+".action", "is required for %s nodes", n.Type)
	}
	if n.Type == TypeForeach {
		s, _ := m["items"].(string)
		if s == "" {
			c.add(path+".items", "is required (an expression returning a list)")
		} else {
			n.Items = c.expr(s, path+".items")
		}
		n.Concurrency = DefaultConcurrency
		if raw, ok := m["concurrency"]; ok {
			v, ok := toInt(raw)
			if !ok || v < 1 || v > MaxConcurrency {
				c.add(path+".concurrency", "must be an integer from 1 to %d", MaxConcurrency)
			}
			n.Concurrency = v
		}
	}
	params := map[string]any{}
	if raw, ok := m["params"]; ok {
		pm, ok := raw.(map[string]any)
		if !ok {
			c.add(path+".params", "must be an object")
		} else {
			params = pm
		}
	}
	n.Params = c.value(params, path+".params", n.Type == TypeForeach)
	if c.cat != nil && action != "" {
		info, ok := c.cat.Lookup(action)
		if !ok {
			c.add(path+".action", "unknown action %q", action)
		} else if info.ParamsSchema != nil {
			for _, e := range jsonschema.CheckKeys(info.ParamsSchema, params, func(k string) bool { return n.Params.Map[k].Literal() }, path+".params") {
				c.add(e.Path, "%s", e.Message)
			}
		}
	}

	n.Retry = RetryPolicy{MaxAttempts: DefaultAttempts, Backoff: "exponential", Initial: DefaultInitial, Max: DefaultMaxBackoff}
	if raw, ok := m["retry"]; ok {
		rm, ok := raw.(map[string]any)
		if !ok {
			c.add(path+".retry", "must be an object")
		}
		for _, k := range sortedKeys(rm) {
			rp := path + ".retry." + k
			switch k {
			case "max_attempts":
				v, ok := toInt(rm[k])
				if !ok || v < 1 || v > MaxAttempts {
					c.add(rp, "must be an integer from 1 to %d", MaxAttempts)
				}
				n.Retry.MaxAttempts = v
			case "backoff":
				s, _ := rm[k].(string)
				if s != "exponential" && s != "fixed" {
					c.add(rp, "must be exponential or fixed")
				}
				n.Retry.Backoff = s
			case "initial", "max":
				d, err := parseDurationAny(rm[k])
				if err != nil || d <= 0 || d > MaxStepTimeout {
					c.add(rp, "must be a positive duration up to 1h")
				}
				if k == "initial" {
					n.Retry.Initial = d
				} else {
					n.Retry.Max = d
				}
			default:
				c.add(rp, "unknown field")
			}
		}
		if n.Retry.Max < n.Retry.Initial {
			n.Retry.Max = n.Retry.Initial
		}
	}
	n.Timeout = DefaultStepTimeout
	if raw, ok := m["timeout"]; ok {
		d, err := parseDurationAny(raw)
		if err != nil || d <= 0 || d > MaxStepTimeout {
			c.add(path+".timeout", "must be a positive duration up to 1h")
		}
		n.Timeout = d
	}
	if s, ok := m["idempotency_key"].(string); ok && s != "" {
		n.IdemKey = c.expr(s, path+".idempotency_key")
	}
	n.Priority = "normal"
	if s, ok := m["priority"].(string); ok {
		switch s {
		case "critical", "normal", "bulk":
			n.Priority = s
		default:
			c.add(path+".priority", "must be critical, normal or bulk")
		}
	}
}

func (c *compiler) expr(src, path string) *expr.Program {
	if len(src) > MaxExprLen {
		c.add(path, "is longer than %d characters", MaxExprLen)
		return nil
	}
	p, err := c.cel.Compile(src)
	if err != nil {
		c.add(path, "does not compile: %v", err)
		return nil
	}
	return p
}

// value compiles a parameter tree. Strings that compile as CEL against the workflow
// variables are expressions; any other string is a literal (spec 6: method: GET is a
// literal, url: 'inputs.url' an expression). To force a literal string that happens
// to be valid CEL, quote it inside: '"1 + 2"'.
func (c *compiler) value(raw any, path string, foreach bool) *Value {
	switch t := raw.(type) {
	case string:
		if len(t) > MaxExprLen {
			return &Value{Lit: t, lit: true}
		}
		if p, err := c.cel.Compile(t); err == nil {
			return &Value{Prg: p, Secrets: secretRefs(t)}
		}
		return &Value{Lit: t, lit: true}
	case map[string]any:
		v := &Value{Map: map[string]*Value{}}
		for _, k := range sortedKeys(t) {
			v.Map[k] = c.value(t[k], path+"."+k, foreach)
		}
		return v
	case []any:
		v := &Value{List: make([]*Value, len(t)), isList: true}
		for i, x := range t {
			v.List[i] = c.value(x, fmt.Sprintf("%s[%d]", path, i), foreach)
		}
		return v
	default:
		return &Value{Lit: t, lit: true}
	}
}

func (c *compiler) targets(raw any, path, self string) []string {
	if raw == nil {
		return nil
	}
	var list []any
	switch t := raw.(type) {
	case string:
		list = []any{t}
	case []any:
		list = t
	default:
		c.add(path, "must be a node id or a list of node ids")
		return nil
	}
	var out []string
	for j, x := range list {
		s, ok := x.(string)
		p := fmt.Sprintf("%s[%d]", path, j)
		switch {
		case !ok:
			c.add(p, "must be a node id")
		case c.nodes[s] == nil:
			c.add(p, "refers to unknown node %q", s)
		case s == self:
			c.add(p, "a node cannot lead to itself; loops are only allowed through foreach")
		default:
			out = appendUnique(out, s)
		}
	}
	return out
}

func (c *compiler) edges(raw any) {
	if raw == nil {
		return
	}
	edges, ok := raw.([]any)
	if !ok {
		c.add("definition.edges", "must be an array of \"from -> to\" strings")
		return
	}
	for i, re := range edges {
		path := fmt.Sprintf("definition.edges[%d]", i)
		s, ok := re.(string)
		parts := strings.Split(s, "->")
		if !ok || len(parts) != 2 {
			c.add(path, "must look like \"from -> to\"")
			continue
		}
		from, to := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		fn, tn := c.nodes[from], c.nodes[to]
		switch {
		case fn == nil:
			c.add(path, "refers to unknown node %q", from)
		case tn == nil:
			c.add(path, "refers to unknown node %q", to)
		case from == to:
			c.add(path, "a node cannot lead to itself")
		case fn.Type == TypeBranch:
			c.add(path, "branch node %q routes through cases and default, not edges", from)
		case fn.Type == TypeEnd:
			c.add(path, "end node %q has no successors", from)
		default:
			fn.Next = appendUnique(fn.Next, to)
		}
	}
}

// Targets returns every node n can lead to, in a stable order.
func (n *Node) Targets() []string {
	var out []string
	add := func(l []string) {
		for _, s := range l {
			out = appendUnique(out, s)
		}
	}
	add(n.Next)
	for _, cs := range n.Cases {
		add(cs.Next)
	}
	add(n.Default)
	add(n.Branches)
	add(n.OnReject)
	add(n.OnError)
	return out
}

// graph computes predecessors and checks acyclicity and reachability.
func (c *compiler) graph(w *Workflow) {
	for _, n := range w.Nodes {
		for _, t := range n.Targets() {
			w.preds[t] = append(w.preds[t], n.ID)
		}
	}
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var cycle []string
	var visit func(id string, stack []string) bool
	visit = func(id string, stack []string) bool {
		color[id] = grey
		stack = append(stack, id)
		for _, t := range w.byID[id].Targets() {
			switch color[t] {
			case grey:
				cycle = append(stack, t)
				return true
			case white:
				if visit(t, stack) {
					return true
				}
			}
		}
		color[id] = black
		return false
	}
	for _, n := range w.Nodes {
		if color[n.ID] == white && visit(n.ID, nil) {
			c.add("definition", "the workflow contains a cycle (%s); loops are only allowed through foreach", strings.Join(cycle, " -> "))
			return
		}
	}
	var entries []string
	for _, n := range w.Nodes {
		if len(w.preds[n.ID]) == 0 {
			entries = append(entries, n.ID)
		}
	}
	// with no cycles at least one entry exists; every node is reachable from an entry
	// by construction (follow predecessors back to a node without any), so the only
	// unreachable nodes are those whose only predecessors are themselves unreachable,
	// which cannot happen in a DAG. We still check, defensively.
	reach := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if reach[id] {
			return
		}
		reach[id] = true
		for _, t := range w.byID[id].Targets() {
			walk(t)
		}
	}
	for _, e := range entries {
		walk(e)
	}
	for _, n := range w.Nodes {
		if !reach[n.ID] {
			c.add(fmt.Sprintf("definition.nodes[%d]", n.index), "node %q is not reachable", n.ID)
		}
	}
}

// Entries returns the nodes without predecessors, in definition order.
func (w *Workflow) Entries() []string {
	var out []string
	for _, n := range w.Nodes {
		if len(w.preds[n.ID]) == 0 {
			out = append(out, n.ID)
		}
	}
	return out
}

// ---- small helpers ----

var durRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)(ms|s|m|h|d)$`)

// ParseDuration accepts Go durations plus a "d" (day) unit: 500ms, 30s, 5m, 2h, 1d, 1h30m.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if m := durRe.FindStringSubmatch(s); m != nil && m[2] == "d" {
		f, _ := strconv.ParseFloat(m[1], 64)
		return time.Duration(f * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use for example 30s, 5m, 2h, 1d)", s)
	}
	return d, nil
}

func parseDurationAny(v any) (time.Duration, error) {
	switch t := v.(type) {
	case string:
		return ParseDuration(t)
	case float64:
		return time.Duration(t * float64(time.Second)), nil // bare numbers are seconds
	case int:
		return time.Duration(t) * time.Second, nil
	}
	return 0, fmt.Errorf("invalid duration %v", v)
}

func fmtDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	return d.String()
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if t == float64(int(t)) {
			return int(t), true
		}
	case int:
		return t, true
	case int64:
		return int(t), true
	}
	return 0, false
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Preds returns the nodes that lead to id.
func (w *Workflow) Preds(id string) []string { return w.preds[id] }
