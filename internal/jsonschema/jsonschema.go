// Package jsonschema validates decoded JSON against the subset of JSON Schema used by
// action manifests (spec section 10): type, properties, required,
// additionalProperties, items, enum, const, minimum/maximum, minLength/maxLength,
// minItems/maxItems, pattern, format (uri, email, ipv4), oneOf/anyOf.
// It imports nothing internal so the engine, the worker and the API can all use it.
package jsonschema

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Error is one violation, addressed by a JSON path such as "params.headers.X".
type Error struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e Error) String() string { return e.Path + ": " + e.Message }

// Validate checks v against schema and returns every violation (nil when valid).
// root names the value in paths ("params", "output").
func Validate(schema map[string]any, v any, root string) []Error {
	if schema == nil {
		return nil
	}
	var errs []Error
	check(schema, v, root, &errs)
	return errs
}

// CheckKeys is the save-time check of a partially known object: it reports missing
// required properties and unknown keys (when additionalProperties is false), and
// validates only the values known reports as literal. Expressions are checked at
// run time when their value exists.
func CheckKeys(schema map[string]any, obj map[string]any, known func(key string) bool, root string) []Error {
	if schema == nil {
		return nil
	}
	var errs []Error
	props, _ := schema["properties"].(map[string]any)
	for _, r := range strList(schema["required"]) {
		if _, ok := obj[r]; !ok {
			errs = append(errs, Error{root + "." + r, "is required"})
		}
	}
	keys := sortedKeys(obj)
	for _, k := range keys {
		ps, declared := props[k].(map[string]any)
		if !declared {
			if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
				errs = append(errs, Error{root + "." + k, "is not a known parameter (allowed: " + strings.Join(sortedKeys(props), ", ") + ")"})
			}
			continue
		}
		if known(k) {
			check(ps, obj[k], root+"."+k, &errs)
		}
	}
	return errs
}

func check(s map[string]any, v any, path string, errs *[]Error) {
	add := func(format string, a ...any) { *errs = append(*errs, Error{path, fmt.Sprintf(format, a...)}) }

	if c, ok := s["const"]; ok && !equal(c, v) {
		add("must be %v", c)
	}
	if enum, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if equal(e, v) {
				found = true
				break
			}
		}
		if !found {
			add("must be one of %v", enum)
		}
	}
	if alts, ok := s["anyOf"].([]any); ok && !anyMatches(alts, v, path) {
		add("does not match any allowed form")
	}
	if alts, ok := s["oneOf"].([]any); ok {
		n := 0
		for _, a := range alts {
			if am, ok := a.(map[string]any); ok && len(Validate(am, v, path)) == 0 {
				n++
			}
		}
		if n != 1 {
			add("must match exactly one allowed form (matched %d)", n)
		}
	}

	if t, present := s["type"]; present {
		types := strList(t)
		if len(types) == 0 {
			if ts, ok := t.(string); ok {
				types = []string{ts}
			}
		}
		if !typeMatches(types, v) {
			add("must be %s, got %s", strings.Join(types, " or "), typeName(v))
			return
		}
	}

	switch val := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		for _, r := range strList(s["required"]) {
			if _, ok := val[r]; !ok {
				*errs = append(*errs, Error{path + "." + r, "is required"})
			}
		}
		for _, k := range sortedKeys(val) {
			if ps, ok := props[k].(map[string]any); ok {
				check(ps, val[k], path+"."+k, errs)
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					*errs = append(*errs, Error{path + "." + k, "is not allowed"})
				}
			case map[string]any:
				check(ap, val[k], path+"."+k, errs)
			}
		}
	case []any:
		if items, ok := s["items"].(map[string]any); ok {
			for i, x := range val {
				check(items, x, fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
		if n, ok := num(s["minItems"]); ok && float64(len(val)) < n {
			add("must have at least %v items", n)
		}
		if n, ok := num(s["maxItems"]); ok && float64(len(val)) > n {
			add("must have at most %v items", n)
		}
	case string:
		if n, ok := num(s["minLength"]); ok && float64(len([]rune(val))) < n {
			add("must be at least %v characters", n)
		}
		if n, ok := num(s["maxLength"]); ok && float64(len([]rune(val))) > n {
			add("must be at most %v characters", n)
		}
		if p, ok := s["pattern"].(string); ok {
			if re, err := compile(p); err != nil {
				add("schema pattern %q is invalid", p)
			} else if !re.MatchString(val) {
				add("must match %s", p)
			}
		}
		if f, ok := s["format"].(string); ok {
			if msg := checkFormat(f, val); msg != "" {
				add("%s", msg)
			}
		}
	default:
		if f, ok := num(v); ok {
			if n, ok := num(s["minimum"]); ok && f < n {
				add("must be >= %v", n)
			}
			if n, ok := num(s["maximum"]); ok && f > n {
				add("must be <= %v", n)
			}
		}
	}
}

func anyMatches(alts []any, v any, path string) bool {
	for _, a := range alts {
		if am, ok := a.(map[string]any); ok && len(Validate(am, v, path)) == 0 {
			return true
		}
	}
	return false
}

func checkFormat(f, s string) string {
	switch f {
	case "uri", "url":
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "must be an absolute URL"
		}
	case "email":
		if !strings.Contains(s, "@") || strings.HasPrefix(s, "@") || strings.HasSuffix(s, "@") {
			return "must be an email address"
		}
	case "ipv4":
		if ip := net.ParseIP(s); ip == nil || ip.To4() == nil {
			return "must be an IPv4 address"
		}
	case "ip":
		if net.ParseIP(s) == nil {
			return "must be an IP address"
		}
	}
	return ""
}

var (
	reMu    sync.Mutex
	reCache = map[string]*regexp.Regexp{}
)

func compile(p string) (*regexp.Regexp, error) {
	reMu.Lock()
	defer reMu.Unlock()
	if re, ok := reCache[p]; ok {
		return re, nil
	}
	re, err := regexp.Compile(p)
	if err == nil {
		reCache[p] = re
	}
	return re, err
}

func typeMatches(types []string, v any) bool {
	for _, t := range types {
		switch t {
		case "object":
			if _, ok := v.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := v.([]any); ok {
				return true
			}
		case "string":
			if _, ok := v.(string); ok {
				return true
			}
		case "boolean":
			if _, ok := v.(bool); ok {
				return true
			}
		case "null":
			if v == nil {
				return true
			}
		case "number":
			if _, ok := num(v); ok {
				return true
			}
		case "integer":
			if f, ok := num(v); ok && f == math.Trunc(f) {
				return true
			}
		}
	}
	return false
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	if _, ok := num(v); ok {
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint64:
		return float64(t), true
	case int32:
		return float64(t), true
	}
	return 0, false
}

func equal(a, b any) bool {
	fa, oka := num(a)
	fb, okb := num(b)
	if oka && okb {
		return fa == fb
	}
	return fmt.Sprint(a) == fmt.Sprint(b) && typeName(a) == typeName(b)
}

func strList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
