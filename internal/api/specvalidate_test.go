package api

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	apispec "github.com/yourorg/soar/api"
)

// spec is just enough of OpenAPI 3.0 to check requests and responses: $ref, allOf,
// type, enum, required, properties, additionalProperties:false, items, min/max,
// minLength/maxLength, pattern and format date-time.
type spec struct{ root map[string]any }

func loadSpec() (*spec, error) {
	var root map[string]any
	if err := yaml.Unmarshal(apispec.Spec, &root); err != nil {
		return nil, err
	}
	return &spec{root: root}, nil
}

func (s *spec) resolve(n any) any {
	for {
		m, ok := n.(map[string]any)
		if !ok {
			return n
		}
		ref, ok := m["$ref"].(string)
		if !ok {
			return n
		}
		cur := any(s.root)
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			cm, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = cm[part]
		}
		n = cur
	}
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}

// check returns a list of violations of schema by the decoded JSON value v.
func (s *spec) check(schema, v any, path string) []string {
	sc := asMap(s.resolve(schema))
	if sc == nil {
		return []string{path + ": schema not found"}
	}
	var errs []string
	if all, ok := sc["allOf"].([]any); ok {
		for _, sub := range all {
			errs = append(errs, s.check(sub, v, path)...)
		}
	}
	typ, _ := sc["type"].(string)
	if v == nil {
		if nullable, _ := sc["nullable"].(bool); !nullable && typ != "" {
			return append(errs, path+": null is not allowed")
		}
		return errs
	}
	switch typ {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return append(errs, fmt.Sprintf("%s: want object, got %T", path, v))
		}
		props := asMap(sc["properties"])
		if req, ok := sc["required"].([]any); ok {
			for _, r := range req {
				if _, present := obj[r.(string)]; !present {
					errs = append(errs, fmt.Sprintf("%s: missing required field %q", path, r))
				}
			}
		}
		if ap, ok := sc["additionalProperties"].(bool); ok && !ap {
			for k := range obj {
				if _, known := props[k]; !known {
					errs = append(errs, fmt.Sprintf("%s: unexpected field %q", path, k))
				}
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k]; ok {
				errs = append(errs, s.check(ps, obj[k], path+"."+k)...)
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return append(errs, fmt.Sprintf("%s: want array, got %T", path, v))
		}
		if items, ok := sc["items"]; ok {
			for i, it := range arr {
				errs = append(errs, s.check(items, it, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return append(errs, fmt.Sprintf("%s: want string, got %T", path, v))
		}
		if n, ok := num(sc["minLength"]); ok && float64(utf8.RuneCountInString(str)) < n {
			errs = append(errs, fmt.Sprintf("%s: shorter than %v", path, n))
		}
		if n, ok := num(sc["maxLength"]); ok && float64(utf8.RuneCountInString(str)) > n {
			errs = append(errs, fmt.Sprintf("%s: longer than %v", path, n))
		}
		if p, ok := sc["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(str) {
			errs = append(errs, fmt.Sprintf("%s: does not match %s", path, p))
		}
		if sc["format"] == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, str); err != nil {
				errs = append(errs, fmt.Sprintf("%s: not an RFC 3339 timestamp: %q", path, str))
			}
		}
	case "integer", "number":
		f, ok := v.(float64)
		if !ok {
			return append(errs, fmt.Sprintf("%s: want number, got %T", path, v))
		}
		if typ == "integer" && f != math.Trunc(f) {
			errs = append(errs, fmt.Sprintf("%s: want integer, got %v", path, f))
		}
		if n, ok := num(sc["minimum"]); ok && f < n {
			errs = append(errs, fmt.Sprintf("%s: %v is below %v", path, f, n))
		}
		if n, ok := num(sc["maximum"]); ok && f > n {
			errs = append(errs, fmt.Sprintf("%s: %v is above %v", path, f, n))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			errs = append(errs, fmt.Sprintf("%s: want boolean, got %T", path, v))
		}
	}
	if enum, ok := sc["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				found = true
			}
		}
		if !found {
			errs = append(errs, fmt.Sprintf("%s: %v is not one of %v", path, v, enum))
		}
	}
	return errs
}

type operation struct {
	id, method, template string
	def                  map[string]any
	params               []map[string]any
}

// operations lists every operation of the document.
func (s *spec) operations() []operation {
	var ops []operation
	for tmpl, item := range asMap(s.root["paths"]) {
		im := asMap(item)
		var shared []map[string]any
		if ps, ok := im["parameters"].([]any); ok {
			for _, p := range ps {
				shared = append(shared, asMap(s.resolve(p)))
			}
		}
		for method, def := range im {
			switch method {
			case "get", "post", "put", "patch", "delete":
			default:
				continue
			}
			d := asMap(def)
			op := operation{id: d["operationId"].(string), method: strings.ToUpper(method), template: tmpl, def: d, params: append([]map[string]any{}, shared...)}
			if ps, ok := d["parameters"].([]any); ok {
				for _, p := range ps {
					op.params = append(op.params, asMap(s.resolve(p)))
				}
			}
			ops = append(ops, op)
		}
	}
	return ops
}

// find returns the operation for a concrete request.
func (s *spec) find(method, path string) (operation, bool) {
	for _, op := range s.operations() {
		if op.method == method && pathMatches(op.template, path) {
			return op, true
		}
	}
	return operation{}, false
}
