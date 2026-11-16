// Package policy decides whether an action may run (spec 20.1):
//
//	Decide(actor, action, args, risk) -> allow | deny | require_approval
//
// The same guardrails apply to humans, workflows and (later) agents. Rules are CEL
// expressions evaluated in order; the first match wins. Built-in defaults follow the
// spec: destructive actions need a human approval unless an approval node directly
// precedes them, sends above a recipient threshold need approval (Outlook, spec
// 10.1), agents need approval for any write, and dry runs are always allowed.
package policy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/yourorg/soar/internal/expr"
)

// Effects.
const (
	Allow           = "allow"
	Deny            = "deny"
	RequireApproval = "require_approval"
)

// Request describes one intended action.
type Request struct {
	ActorType      string // user | workflow | agent | system
	Actor          string
	Action         string
	SideEffect     string // read | write | destructive
	Risk           string // low | medium | high
	Args           map[string]any
	DryRun         bool
	PreApproved    bool           // an approval node directly before this step was approved
	ApprovalPolicy map[string]any // from the action manifest, e.g. {recipients_over: 50}
}

// Decision is the outcome.
type Decision struct {
	Effect string `json:"effect"`
	Reason string `json:"reason"`
	Rule   string `json:"rule,omitempty"`
}

// Rule is one configured rule.
type Rule struct {
	Name   string `yaml:"name" json:"name"`
	When   string `yaml:"when" json:"when"`
	Effect string `yaml:"effect" json:"effect"`
	Reason string `yaml:"reason" json:"reason"`
}

type compiled struct {
	Rule
	prg *expr.Program
}

// Engine evaluates rules.
type Engine struct{ rules []compiled }

// New compiles rules. Variables: actor, actor_type, action, side_effect, risk, args,
// dry_run, pre_approved.
func New(rules []Rule) (*Engine, error) {
	eng, err := expr.New(nil, "actor", "actor_type", "action", "side_effect", "risk", "args", "dry_run", "pre_approved")
	if err != nil {
		return nil, err
	}
	eng.SetCostLimit(100_000)
	e := &Engine{}
	for i, r := range rules {
		switch r.Effect {
		case Allow, Deny, RequireApproval:
		default:
			return nil, fmt.Errorf("policy rule %d (%s): effect must be allow, deny or require_approval", i, r.Name)
		}
		p, err := eng.Compile(r.When)
		if err != nil {
			return nil, fmt.Errorf("policy rule %d (%s): %w", i, r.Name, err)
		}
		e.rules = append(e.rules, compiled{Rule: r, prg: p})
	}
	return e, nil
}

// Load reads rules from a YAML file ({rules: [...]}); a missing file means defaults only.
func Load(path string) (*Engine, error) {
	if path == "" {
		return New(nil)
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return New(nil)
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Rules []Rule `yaml:"rules"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("policy file %s: %w", path, err)
	}
	return New(f.Rules)
}

// Decide applies the configured rules, then the defaults.
func (e *Engine) Decide(r Request) Decision {
	if r.DryRun {
		return Decision{Effect: Allow, Reason: "dry run: nothing changes"}
	}
	if e != nil && len(e.rules) > 0 {
		vars := map[string]any{
			"actor": r.Actor, "actor_type": r.ActorType, "action": r.Action, "side_effect": r.SideEffect,
			"risk": r.Risk, "args": orEmpty(r.Args), "dry_run": r.DryRun, "pre_approved": r.PreApproved,
		}
		for _, rule := range e.rules {
			ok, err := rule.prg.Bool(vars)
			if err != nil {
				return Decision{Effect: Deny, Reason: fmt.Sprintf("policy rule %s failed: %v", rule.Name, err), Rule: rule.Name}
			}
			if ok {
				reason := rule.Reason
				if reason == "" {
					reason = "matched policy rule " + rule.Name
				}
				return Decision{Effect: rule.Effect, Reason: reason, Rule: rule.Name}
			}
		}
	}
	if n, ok := num(r.ApprovalPolicy["recipients_over"]); ok && !r.PreApproved {
		if c := recipients(r.Args); float64(c) > n {
			return Decision{Effect: RequireApproval, Reason: fmt.Sprintf("%d recipients is above the approval threshold of %v", c, n), Rule: "default:recipients"}
		}
	}
	if n, ok := num(r.ApprovalPolicy["items_over"]); ok && !r.PreApproved {
		if c := count(r.Args["mailboxes"]) + count(r.Args["message_ids"]); float64(c) > n {
			return Decision{Effect: RequireApproval, Reason: fmt.Sprintf("%d items is above the approval threshold of %v", c, n), Rule: "default:items"}
		}
	}
	switch {
	case r.SideEffect == "destructive" && !r.PreApproved:
		return Decision{Effect: RequireApproval, Reason: "destructive actions need a human approval", Rule: "default:destructive"}
	case r.ActorType == "agent" && r.SideEffect != "read" && !r.PreApproved:
		return Decision{Effect: RequireApproval, Reason: "agents need approval for actions with side effects", Rule: "default:agent"}
	}
	return Decision{Effect: Allow, Reason: "allowed by default"}
}

func recipients(args map[string]any) int {
	return count(args["to"]) + count(args["cc"]) + count(args["bcc"])
}

func count(v any) int {
	switch t := v.(type) {
	case []any:
		return len(t)
	case []string:
		return len(t)
	case string:
		if t != "" {
			return 1
		}
	}
	return 0
}

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
