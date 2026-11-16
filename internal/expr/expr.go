// Package expr compiles and evaluates CEL expressions used by source mappings,
// filter sets and (from Step 6) workflows. It adds the helper functions of spec 9.6.
package expr

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// Lists answers managed-list membership for in_list(value, "list_name").
type Lists interface {
	Contains(list, value string) bool
}

// NoLists is a Lists with no members, for contexts without managed lists.
type NoLists struct{}

func (NoLists) Contains(string, string) bool { return false }

// Engine compiles expressions against a fixed set of variable names.
type Engine struct {
	env       *cel.Env
	costLimit uint64
}

// SetCostLimit bounds the runtime cost of every program compiled afterwards (spec
// section 13: CEL cost limits). Evaluation stops with an error when it is exceeded.
func (e *Engine) SetCostLimit(n uint64) { e.costLimit = n }

// Program is a compiled expression.
type Program struct {
	src string
	prg cel.Program
}

// New builds an engine. Every name in vars becomes a dynamically typed variable.
func New(lists Lists, vars ...string) (*Engine, error) {
	if lists == nil {
		lists = NoLists{}
	}
	opts := []cel.EnvOption{}
	for _, v := range vars {
		opts = append(opts, cel.Variable(v, cel.DynType))
	}
	opts = append(opts, functions(lists)...)
	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, fmt.Errorf("cel environment: %w", err)
	}
	return &Engine{env: env}, nil
}

// Compile parses and type-checks src.
func (e *Engine) Compile(src string) (*Program, error) {
	ast, iss := e.env.Compile(src)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("%s", strings.TrimSpace(iss.Err().Error()))
	}
	var popts []cel.ProgramOption
	if e.costLimit > 0 {
		popts = append(popts, cel.CostLimit(e.costLimit))
	}
	prg, err := e.env.Program(ast, popts...)
	if err != nil {
		return nil, err
	}
	return &Program{src: src, prg: prg}, nil
}

// Source returns the expression text, for error messages.
func (p *Program) Source() string { return p.src }

// Eval runs the program and returns a plain Go value (nil, bool, int64, float64,
// string, []any or map[string]any).
func (p *Program) Eval(vars map[string]any) (any, error) {
	out, _, err := p.prg.Eval(vars)
	if err != nil {
		return nil, err
	}
	if out == types.NullValue {
		return nil, nil
	}
	return unwrap(out.Value()), nil
}

// Bool runs the program and requires a boolean result.
func (p *Program) Bool(vars map[string]any) (bool, error) {
	v, err := p.Eval(vars)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("expression %q must return true or false, got %T", p.src, v)
	}
	return b, nil
}

// unwrap converts CEL values to plain Go values.
func unwrap(v any) any {
	switch t := v.(type) {
	case ref.Val:
		if t == types.NullValue {
			return nil
		}
		return unwrap(t.Value())
	case []ref.Val:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = unwrap(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = unwrap(x)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = x
		}
		return out
	case map[ref.Val]ref.Val:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[fmt.Sprint(unwrap(k))] = unwrap(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = unwrap(x)
		}
		return out
	}
	if v != nil && reflect.TypeOf(v).String() == "structpb.NullValue" {
		return nil
	}
	return v
}

func str(v ref.Val) (string, bool) {
	u := unwrap(v)
	s, ok := u.(string)
	return s, ok
}

// Text renders a scalar the way mappings and lists compare values: 5715.0 -> "5715".
func Text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

func isBlank(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// GetPath walks a dotted path through nested maps; it returns nil when any step is missing.
func GetPath(root any, path string) any {
	cur := root
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

var timeLayouts = []string{
	time.RFC3339Nano, "2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700", "2006-01-02T15:04:05.000Z0700",
	"2006-01-02 15:04:05", "2006-01-02T15:04:05",
}

// ParseTime parses common timestamp layouts (or one explicit layout) and returns UTC RFC 3339.
func ParseTime(s, layout string) (string, error) {
	layouts := timeLayouts
	if layout != "" {
		layouts = []string{layout}
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC().Format(time.RFC3339Nano), nil
		}
	}
	return "", fmt.Errorf("cannot parse time %q", s)
}

var (
	ipRe     = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`)
	urlRe    = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>)\]]+`)
	emailRe  = regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b`)
	domainRe = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,24}\b`)
	hashRe   = regexp.MustCompile(`(?i)\b(?:[a-f0-9]{64}|[a-f0-9]{40}|[a-f0-9]{32})\b`)
)

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func extract(re *regexp.Regexp, s string) []string { return uniq(re.FindAllString(s, -1)) }

// IsEmptyHash reports whether h is blank or the hash of empty input.
func IsEmptyHash(h string) bool {
	switch strings.ToLower(strings.TrimSpace(h)) {
	case "", "d41d8cd98f00b204e9800998ecf8427e", "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855":
		return true
	}
	return false
}

var severityDefault = int64(3)

func functions(lists Lists) []cel.EnvOption {
	d, s := cel.DynType, cel.StringType
	listS := cel.ListType(cel.StringType)
	bind := func(f func(args ...ref.Val) ref.Val) cel.OverloadOpt { return cel.FunctionBinding(f) }
	val := func(v any) ref.Val { return types.DefaultTypeAdapter.NativeToValue(v) }
	errv := func(format string, a ...any) ref.Val { return types.NewErr(format, a...) }

	coalesce := func(args ...ref.Val) ref.Val {
		for _, a := range args {
			if !isBlank(unwrap(a)) {
				return a
			}
		}
		return args[len(args)-1] // all blank: the last argument is the default, even when it is ""
	}
	extractFn := func(name string, re *regexp.Regexp) cel.EnvOption {
		return cel.Function(name, cel.Overload(name+"_string", []*cel.Type{s}, listS, cel.UnaryBinding(func(a ref.Val) ref.Val {
			text, _ := str(a)
			return val(extract(re, text))
		})))
	}

	return []cel.EnvOption{
		cel.Function("coalesce",
			cel.Overload("coalesce_2", []*cel.Type{d, d}, d, bind(coalesce)),
			cel.Overload("coalesce_3", []*cel.Type{d, d, d}, d, bind(coalesce)),
			cel.Overload("coalesce_4", []*cel.Type{d, d, d, d}, d, bind(coalesce)),
		),
		cel.Function("get_path", cel.Overload("get_path_dyn_string", []*cel.Type{d, s}, d, cel.BinaryBinding(func(m, p ref.Val) ref.Val {
			path, ok := str(p)
			if !ok {
				return errv("get_path: path must be a string")
			}
			out := GetPath(unwrap(m), path)
			if out == nil {
				return types.NullValue
			}
			return val(out)
		}))),
		cel.Function("in_list",
			cel.Overload("in_list_dyn_string", []*cel.Type{d, s}, cel.BoolType, cel.BinaryBinding(func(v, l ref.Val) ref.Val {
				name, ok := str(l)
				if !ok {
					return errv("in_list: list name must be a string")
				}
				return types.Bool(lists.Contains(name, Text(unwrap(v))))
			})),
			// CEL's own list-membership overload (the `in` operator) has the id "in_list",
			// this function's name, so the runtime sends `x in ["a", "b"]` here: give it
			// the standard meaning.
			cel.Overload("in_list_dyn_list", []*cel.Type{d, cel.ListType(d)}, cel.BoolType, cel.BinaryBinding(func(v, l ref.Val) ref.Val {
				c, ok := l.(traits.Container)
				if !ok {
					return types.NoSuchOverloadErr()
				}
				return c.Contains(v)
			})),
		),
		cel.Function("severity_map", cel.Overload("severity_map_dyn_dyn", []*cel.Type{d, d}, cel.IntType, cel.BinaryBinding(func(v, m ref.Val) ref.Val {
			table, ok := unwrap(m).(map[string]any)
			if !ok {
				return errv("severity_map: second argument must be a map")
			}
			key := strings.ToLower(strings.TrimSpace(Text(unwrap(v))))
			if n, found := table[key]; found {
				if f, ok := toFloat(n); ok {
					return types.Int(int64(f))
				}
			}
			return types.Int(severityDefault)
		}))),
		cel.Function("level_map", cel.Overload("level_map_dyn_dyn", []*cel.Type{d, d}, cel.IntType, cel.BinaryBinding(func(v, m ref.Val) ref.Val {
			level, ok := toFloat(unwrap(v))
			if !ok {
				if f, err := strconv.ParseFloat(Text(unwrap(v)), 64); err == nil {
					level, ok = f, true
				}
			}
			if !ok {
				return errv("level_map: %v is not a number", unwrap(v))
			}
			rows, ok := unwrap(m).([]any)
			if !ok {
				return errv("level_map: second argument must be a list of [low, high, severity]")
			}
			for _, r := range rows {
				row, ok := r.([]any)
				if !ok || len(row) != 3 {
					return errv("level_map: every row must be [low, high, severity]")
				}
				lo, _ := toFloat(row[0])
				hi, _ := toFloat(row[1])
				sev, _ := toFloat(row[2])
				if level >= lo && level <= hi {
					return types.Int(int64(sev))
				}
			}
			return types.Int(severityDefault)
		}))),
		cel.Function("parse_time",
			cel.Overload("parse_time_string", []*cel.Type{s}, s, cel.UnaryBinding(func(a ref.Val) ref.Val {
				text, _ := str(a)
				out, err := ParseTime(text, "")
				if err != nil {
					return errv("%v", err)
				}
				return types.String(out)
			})),
			cel.Overload("parse_time_string_string", []*cel.Type{s, s}, s, cel.BinaryBinding(func(a, l ref.Val) ref.Val {
				text, _ := str(a)
				layout, _ := str(l)
				out, err := ParseTime(text, layout)
				if err != nil {
					return errv("%v", err)
				}
				return types.String(out)
			})),
		),
		cel.Function("lower", cel.Overload("lower_string", []*cel.Type{s}, s, cel.UnaryBinding(func(a ref.Val) ref.Val {
			text, _ := str(a)
			return types.String(strings.ToLower(text))
		}))),
		cel.Function("regex_extract", cel.Overload("regex_extract_string_string", []*cel.Type{s, s}, s, cel.BinaryBinding(func(a, p ref.Val) ref.Val {
			text, _ := str(a)
			pat, _ := str(p)
			re, err := regexp.Compile(pat)
			if err != nil {
				return errv("regex_extract: %v", err)
			}
			m := re.FindStringSubmatch(text)
			switch {
			case m == nil:
				return types.String("")
			case len(m) > 1:
				return types.String(m[1])
			}
			return types.String(m[0])
		}))),
		extractFn("extract_ips", ipRe), extractFn("extract_urls", urlRe), extractFn("extract_emails", emailRe),
		extractFn("extract_domains", domainRe), extractFn("extract_hashes", hashRe),
		cel.Function("is_empty_hash", cel.Overload("is_empty_hash_dyn", []*cel.Type{d}, cel.BoolType, cel.UnaryBinding(func(a ref.Val) ref.Val {
			return types.Bool(IsEmptyHash(Text(unwrap(a))))
		}))),
	}
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}

// SortedKeys is a small helper for deterministic output in tests and traces.
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
