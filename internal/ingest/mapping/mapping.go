// Package mapping turns a vendor record into a normalized alert using a declarative
// definition of CEL expressions (spec 9.6). Adding a source is a mapping plus a
// source document, not code.
package mapping

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/expr"
)

// Vars are the variable names mapping expressions may use.
var Vars = []string{"raw", "value"}

type obsRule struct {
	typ, role string
	value     *expr.Program // yields one value
	from      *expr.Program // yields a list of values
	skipIf    *expr.Program
	path      string
}

// Compiled is a mapping ready to run. It is immutable and safe for concurrent use.
type Compiled struct {
	Name    string
	Version int
	Kind    string

	fields map[string]*expr.Program
	obs    []obsRule
	mitre  map[string]*expr.Program
	dedup  []*expr.Program
	ext    *expr.Program
}

// Error says which expression failed and why, so a mapping can be fixed quickly.
type Error struct {
	Field string
	Expr  string
	Err   error
}

func (e *Error) Error() string {
	return fmt.Sprintf("mapping %s: expression %q: %v", e.Field, e.Expr, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }

// CompileError collects every problem found while compiling a definition.
type CompileError struct{ Problems []string }

func (e *CompileError) Error() string { return "invalid mapping: " + strings.Join(e.Problems, "; ") }

var fieldNames = []string{"external_id", "title", "description", "summary", "severity", "original_severity", "occurred_at", "tags", "category", "external_url"}

// Compile builds a mapping from its definition (the parsed YAML or JSON).
func Compile(e *expr.Engine, name string, version int, def map[string]any) (*Compiled, error) {
	c := &Compiled{Name: name, Version: version, fields: map[string]*expr.Program{}, mitre: map[string]*expr.Program{}}
	var problems []string
	comp := func(path string, v any) *expr.Program {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			problems = append(problems, path+": must be a CEL expression string")
			return nil
		}
		p, err := e.Compile(s)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			return nil
		}
		return p
	}

	c.Kind, _ = def["kind"].(string)
	if c.Kind == "" {
		c.Kind = "alert"
	}
	known := map[string]bool{"kind": true, "fields": true, "observables": true, "mitre": true, "dedup_key": true, "ext": true}
	for k := range def {
		if !known[k] {
			problems = append(problems, fmt.Sprintf("unknown key %q", k))
		}
	}

	fields, _ := def["fields"].(map[string]any)
	if fields == nil {
		problems = append(problems, "fields: is required")
	}
	for _, n := range fieldNames {
		if v, ok := fields[n]; ok {
			if p := comp("fields."+n, v); p != nil {
				c.fields[n] = p
			}
		}
	}
	for k := range fields {
		found := false
		for _, n := range fieldNames {
			found = found || n == k
		}
		if !found {
			problems = append(problems, fmt.Sprintf("fields.%s: unknown field (allowed: %s)", k, strings.Join(fieldNames, ", ")))
		}
	}
	if _, ok := c.fields["title"]; !ok && fields != nil {
		problems = append(problems, "fields.title: is required")
	}

	if list, ok := def["observables"].([]any); ok {
		for i, raw := range list {
			path := fmt.Sprintf("observables[%d]", i)
			m, ok := raw.(map[string]any)
			if !ok {
				problems = append(problems, path+": must be an object")
				continue
			}
			r := obsRule{path: path}
			r.typ, _ = m["type"].(string)
			r.role, _ = m["role"].(string)
			if r.typ == "" {
				problems = append(problems, path+".type: is required")
			}
			if v, ok := m["value"]; ok {
				r.value = comp(path+".value", v)
			}
			if v, ok := m["from"]; ok {
				r.from = comp(path+".from", v)
			}
			if (r.value == nil) == (r.from == nil) {
				problems = append(problems, path+": give exactly one of value or from")
			}
			if v, ok := m["skip_if"]; ok {
				r.skipIf = comp(path+".skip_if", v)
			}
			c.obs = append(c.obs, r)
		}
	}
	if m, ok := def["mitre"].(map[string]any); ok {
		for _, k := range []string{"technique_ids", "techniques", "tactics"} {
			if v, ok := m[k]; ok {
				if p := comp("mitre."+k, v); p != nil {
					c.mitre[k] = p
				}
			}
		}
	}
	if list, ok := def["dedup_key"].([]any); ok {
		for i, v := range list {
			if p := comp(fmt.Sprintf("dedup_key[%d]", i), v); p != nil {
				c.dedup = append(c.dedup, p)
			}
		}
	}
	if v, ok := def["ext"]; ok {
		c.ext = comp("ext", v)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, &CompileError{Problems: problems}
	}
	return c, nil
}

// Result is one mapped record.
type Result struct {
	Alert    domain.Alert
	DedupKey []string
}

func strList(v any) []string {
	var out []string
	switch t := v.(type) {
	case nil:
	case []any:
		for _, x := range t {
			if s := strings.TrimSpace(expr.Text(x)); s != "" {
				out = append(out, s)
			}
		}
	default:
		if s := strings.TrimSpace(expr.Text(t)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Apply maps one record. Observables whose value is null or empty are skipped.
func (c *Compiled) Apply(raw map[string]any) (*Result, error) {
	vars := map[string]any{"raw": raw, "value": nil}
	eval := func(field string, p *expr.Program) (any, error) {
		v, err := p.Eval(vars)
		if err != nil {
			return nil, &Error{Field: field, Expr: p.Source(), Err: err}
		}
		return v, nil
	}
	a := domain.Alert{Kind: c.Kind, Severity: 3}

	for _, n := range fieldNames {
		p, ok := c.fields[n]
		if !ok {
			continue
		}
		v, err := eval("fields."+n, p)
		if err != nil {
			return nil, err
		}
		switch n {
		case "external_id":
			a.ExternalID = expr.Text(v)
		case "title":
			a.Title = strings.TrimSpace(expr.Text(v))
		case "description":
			a.Description = expr.Text(v)
		case "summary":
			a.Summary = expr.Text(v)
		case "category":
			a.Category = expr.Text(v)
		case "external_url":
			a.ExternalURL = expr.Text(v)
		case "original_severity":
			a.OriginalSeverity = expr.Text(v)
		case "severity":
			f, ok := v.(int64)
			if !ok {
				return nil, &Error{Field: "fields.severity", Expr: p.Source(), Err: fmt.Errorf("must be a number, got %v", v)}
			}
			a.Severity = int(min(max(f, 1), 5))
		case "occurred_at":
			if s := expr.Text(v); s != "" {
				t, err := time.Parse(time.RFC3339Nano, s)
				if err != nil {
					return nil, &Error{Field: "fields.occurred_at", Expr: p.Source(), Err: fmt.Errorf("wrap the value in parse_time(): %w", err)}
				}
				a.OccurredAt = &t
			}
		case "tags":
			a.Tags = strList(v)
		}
	}
	if a.Title == "" {
		return nil, &Error{Field: "fields.title", Expr: c.fields["title"].Source(), Err: fmt.Errorf("evaluated to an empty title")}
	}

	seen := map[string]bool{}
	for _, r := range c.obs {
		var vals []string
		if r.from != nil {
			v, err := eval(r.path+".from", r.from)
			if err != nil {
				return nil, err
			}
			vals = strList(v)
		} else {
			v, err := eval(r.path+".value", r.value)
			if err != nil {
				return nil, err
			}
			vals = strList(v)
		}
		for _, val := range vals {
			if r.skipIf != nil {
				vars["value"] = val
				skip, err := r.skipIf.Bool(vars)
				if err != nil {
					return nil, &Error{Field: r.path + ".skip_if", Expr: r.skipIf.Source(), Err: err}
				}
				if skip {
					continue
				}
			}
			key := r.typ + "\x00" + val + "\x00" + r.role
			if seen[key] {
				continue
			}
			seen[key] = true
			a.Observables = append(a.Observables, domain.Observable{Type: r.typ, Value: val, Role: r.role})
			switch r.typ {
			case "host":
				ensureEntities(&a).Hosts = appendUniq(a.Entities.Hosts, val)
			case "user":
				ensureEntities(&a).Users = appendUniq(a.Entities.Users, val)
			}
		}
	}
	vars["value"] = nil

	if len(c.mitre) > 0 {
		m := &domain.Mitre{}
		for k, p := range c.mitre {
			v, err := eval("mitre."+k, p)
			if err != nil {
				return nil, err
			}
			switch k {
			case "technique_ids":
				m.TechniqueIDs = strList(v)
			case "techniques":
				m.Techniques = strList(v)
			case "tactics":
				m.Tactics = strList(v)
			}
		}
		if len(m.TechniqueIDs)+len(m.Techniques)+len(m.Tactics) > 0 {
			a.Mitre = m
		}
	}
	if c.ext != nil {
		v, err := eval("ext", c.ext)
		if err != nil {
			return nil, err
		}
		if m, ok := v.(map[string]any); ok {
			a.Ext = m
		} else if v != nil {
			a.Ext = map[string]any{"value": v}
		}
	}

	res := &Result{Alert: a}
	for i, p := range c.dedup {
		v, err := eval(fmt.Sprintf("dedup_key[%d]", i), p)
		if err != nil {
			return nil, err
		}
		res.DedupKey = append(res.DedupKey, expr.Text(v))
	}
	return res, nil
}

func ensureEntities(a *domain.Alert) *domain.Entities {
	if a.Entities == nil {
		a.Entities = &domain.Entities{}
	}
	return a.Entities
}

func appendUniq(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// Fingerprint identifies "the same alert" for deduplication. System keeps different
// SIEMs apart; parts are the mapping's dedup_key values.
func Fingerprint(system string, parts []string) string {
	h := sha256.New()
	h.Write([]byte(system))
	for _, p := range parts {
		h.Write([]byte{0x1f})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
