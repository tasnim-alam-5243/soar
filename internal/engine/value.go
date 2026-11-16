package engine

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/yourorg/soar/internal/expr"
)

// Value is a compiled parameter tree: a literal, an expression, a map or a list.
type Value struct {
	Lit     any
	Prg     *expr.Program
	Map     map[string]*Value
	List    []*Value
	Secrets []string // secret names an expression references

	lit    bool
	isList bool
}

// Literal reports whether the value contains no expression at all.
func (v *Value) Literal() bool {
	if v == nil {
		return true
	}
	switch {
	case v.Prg != nil:
		return false
	case v.Map != nil:
		for _, x := range v.Map {
			if !x.Literal() {
				return false
			}
		}
	case v.isList:
		for _, x := range v.List {
			if !x.Literal() {
				return false
			}
		}
	}
	return true
}

// Eval resolves the tree against vars.
func (v *Value) Eval(vars map[string]any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch {
	case v.Prg != nil:
		out, err := v.Prg.Eval(withSecretMarkers(vars, v.Secrets))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", v.Prg.Source(), err)
		}
		return out, nil
	case v.Map != nil:
		out := make(map[string]any, len(v.Map))
		for _, k := range sortedKeys(v.Map) {
			x, err := v.Map[k].Eval(vars)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = x
		}
		return out, nil
	case v.isList:
		out := make([]any, len(v.List))
		for i, x := range v.List {
			r, err := x.Eval(vars)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = r
		}
		return out, nil
	}
	return v.Lit, nil
}

// ---- secrets ----
//
// Spec rule 7: secrets are decrypted only inside the worker and never appear in the
// execution context, logs or messages. Workflow expressions may still use
// secrets.<name>; the engine evaluates them with placeholder strings and the worker
// replaces each placeholder with the decrypted value right before running the
// action. A secret can therefore be used as a whole value or concatenated into a
// string ("Bearer " + secrets.token), but not transformed by other functions.

const (
	markerOpen  = "\x00secret:"
	markerClose = "\x00"
)

var secretRefRe = regexp.MustCompile(`\bsecrets\.([A-Za-z_][A-Za-z0-9_]*)`)

func secretRefs(src string) []string {
	var out []string
	for _, m := range secretRefRe.FindAllStringSubmatch(src, -1) {
		out = appendUnique(out, m[1])
	}
	return out
}

// SecretMarker is the placeholder the worker replaces with the named secret.
func SecretMarker(name string) string { return markerOpen + name + markerClose }

var markerRe = regexp.MustCompile("\x00secret:([A-Za-z_][A-Za-z0-9_]*)\x00")

func withSecretMarkers(vars map[string]any, names []string) map[string]any {
	if len(names) == 0 {
		return vars
	}
	m := make(map[string]any, len(names))
	for _, n := range names {
		m[n] = SecretMarker(n)
	}
	out := make(map[string]any, len(vars)+1)
	for k, v := range vars {
		out[k] = v
	}
	out["secrets"] = m
	return out
}

// SecretNames lists the secrets a resolved parameter tree needs.
func SecretNames(v any) []string {
	var out []string
	walkStrings(v, func(s string) string {
		for _, m := range markerRe.FindAllStringSubmatch(s, -1) {
			out = appendUnique(out, m[1])
		}
		return s
	})
	return out
}

// ResolveSecrets replaces every placeholder in v using lookup. It returns a new tree.
func ResolveSecrets(v any, lookup func(name string) (string, error)) (any, error) {
	var firstErr error
	out := walkStrings(v, func(s string) string {
		if !strings.Contains(s, markerOpen) {
			return s
		}
		return markerRe.ReplaceAllStringFunc(s, func(m string) string {
			name := markerRe.FindStringSubmatch(m)[1]
			val, err := lookup(name)
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("secret %q: %w", name, err)
			}
			return val
		})
	})
	return out, firstErr
}

// RedactSecrets replaces placeholders with "[secret:name]" for display and storage.
func RedactSecrets(v any) any {
	return walkStrings(v, func(s string) string {
		return markerRe.ReplaceAllString(s, "[secret:$1]")
	})
}

func walkStrings(v any, f func(string) string) any {
	switch t := v.(type) {
	case string:
		return f(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = walkStrings(x, f)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = walkStrings(x, f)
		}
		return out
	}
	return v
}
