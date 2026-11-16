// Package filters decides which events are kept, tagged, re-prioritised or routed
// (spec 9.5.1). Filter sets are versioned objects that sources reference by name@version.
package filters

import (
	"fmt"
	"strings"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/expr"
)

// Stages: pre_map rules see the raw record as `raw` (cheapest, drops early);
// post_map rules see the normalized alert as `event`.
const (
	PreMap  = "pre_map"
	PostMap = "post_map"
)

// Vars are the variable names filter expressions may use.
var Vars = []string{"raw", "event"}

// Rule is one compiled rule.
type Rule struct {
	Name     string
	Stage    string
	Action   string // keep | drop | tag | set_severity | route
	When     *expr.Program
	Tag      string
	Severity int
	Workflow string
}

// Set is a compiled filter set. Immutable and safe for concurrent use.
type Set struct {
	Name    string
	Version int
	Mode    string // first_match | all
	Default string // keep | drop
	Rules   []Rule
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

// Compile validates and compiles a stored filter set.
func Compile(e *expr.Engine, fs domain.FilterSet) (*Set, error) {
	s := &Set{Name: fs.Name, Version: fs.Version, Mode: fs.Mode, Default: fs.Default}
	if s.Mode == "" {
		s.Mode = "first_match"
	}
	if s.Default == "" {
		s.Default = "keep"
	}
	var problems []string
	if s.Mode != "first_match" && s.Mode != "all" {
		problems = append(problems, fmt.Sprintf("mode must be first_match or all, got %q", s.Mode))
	}
	if s.Default != "keep" && s.Default != "drop" {
		problems = append(problems, fmt.Sprintf("default must be keep or drop, got %q", s.Default))
	}
	names := map[string]bool{}
	for i, raw := range fs.Rules {
		path := fmt.Sprintf("rules[%d]", i)
		r := Rule{Name: str(raw, "name"), Stage: str(raw, "stage"), Action: str(raw, "action")}
		if r.Name == "" {
			problems = append(problems, path+".name: is required")
		} else if names[r.Name] {
			problems = append(problems, fmt.Sprintf("%s.name: duplicate rule name %q", path, r.Name))
		}
		names[r.Name] = true
		if r.Stage != PreMap && r.Stage != PostMap {
			problems = append(problems, fmt.Sprintf("%s.stage: must be pre_map or post_map, got %q", path, r.Stage))
		}
		switch r.Action {
		case "keep", "drop", "tag", "set_severity", "route":
		default:
			problems = append(problems, fmt.Sprintf("%s.action: must be keep, drop, tag, set_severity or route, got %q", path, r.Action))
		}
		if _, ok := raw["script"]; ok {
			problems = append(problems, path+".script: script filters need the Python runner, which arrives in Step 7")
			continue
		}
		when := str(raw, "when")
		if when == "" {
			problems = append(problems, path+".when: is required")
		} else if p, err := e.Compile(when); err != nil {
			problems = append(problems, fmt.Sprintf("%s.when: %v", path, err))
		} else {
			r.When = p
		}
		r.Tag = str(raw, "tag")
		if r.Tag == "" {
			r.Tag = r.Name
		}
		if f, ok := raw["severity"].(float64); ok {
			r.Severity = int(f)
		}
		if n, ok := raw["severity"].(int); ok {
			r.Severity = n
		}
		r.Workflow = str(raw, "workflow")
		if r.Action == "set_severity" && (r.Severity < 1 || r.Severity > 5) {
			problems = append(problems, path+".severity: set_severity needs severity 1-5")
		}
		if r.Action == "route" && r.Workflow == "" {
			problems = append(problems, path+".workflow: route needs a workflow name")
		}
		s.Rules = append(s.Rules, r)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid filter set %s: %s", fs.Name, strings.Join(problems, "; "))
	}
	return s, nil
}

// Step is one rule evaluation in a trace.
type Step struct {
	Rule    string `json:"rule"`
	Matched bool   `json:"matched"`
	Action  string `json:"action,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Decision is the outcome of one stage.
type Decision struct {
	Drop     bool
	Rule     string // rule that dropped the event, or "default"
	Tags     []string
	Severity int    // 0 = unchanged
	Route    string // workflow name
	Trace    []Step
}

// HasStage reports whether the set has rules for stage.
func (s *Set) HasStage(stage string) bool {
	for _, r := range s.Rules {
		if r.Stage == stage {
			return true
		}
	}
	return false
}

// Eval runs the rules of one stage. A rule whose expression fails does not match
// (fail open: the event is kept) and the failure is recorded in the trace.
func (s *Set) Eval(stage string, vars map[string]any) Decision {
	var d Decision
	matchedAny := false
	for _, r := range s.Rules {
		if r.Stage != stage {
			continue
		}
		ok, err := r.When.Bool(vars)
		step := Step{Rule: r.Name, Matched: ok && err == nil}
		if err != nil {
			step.Error = err.Error()
		}
		if step.Matched {
			step.Action = r.Action
		}
		d.Trace = append(d.Trace, step)
		if !step.Matched {
			continue
		}
		matchedAny = true
		switch r.Action {
		case "drop":
			d.Drop, d.Rule = true, r.Name
			return d
		case "keep":
			return d
		case "tag":
			d.Tags = append(d.Tags, r.Tag)
		case "set_severity":
			d.Severity = r.Severity
		case "route":
			d.Route = r.Workflow
		}
		if s.Mode == "first_match" {
			return d
		}
	}
	if !matchedAny && s.Default == "drop" && s.HasStage(stage) {
		d.Drop, d.Rule = true, "default"
	}
	return d
}
