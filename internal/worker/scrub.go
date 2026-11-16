package worker

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
)

// minScrubLen is the shortest secret value that is scrubbed: shorter values would
// mangle ordinary text. The Python runner uses the same floor.
const minScrubLen = 4

// redacted replaces a secret value (same marker as the Python runner).
const redacted = "***"

// secretSet holds the secret values one step used, so the worker can keep them out of
// everything it records or returns (spec rule 3.1.7): outputs, error messages, log lines.
// The base64 form of each value is scrubbed too (an echoed Basic authorization header).
type secretSet struct {
	values []string // longest first, so a value inside another is replaced after it
}

func (s *secretSet) add(v string) {
	if len(v) < minScrubLen {
		return
	}
	for _, x := range []string{v, base64.StdEncoding.EncodeToString([]byte(v))} {
		dup := false
		for _, have := range s.values {
			if have == x {
				dup = true
				break
			}
		}
		if !dup {
			s.values = append(s.values, x)
		}
	}
	sort.Slice(s.values, func(i, j int) bool { return len(s.values[i]) > len(s.values[j]) })
}

func (s *secretSet) empty() bool { return s == nil || len(s.values) == 0 }

// text returns t with every secret value replaced, and whether anything was replaced.
func (s *secretSet) text(t string) (string, bool) {
	if s.empty() {
		return t, false
	}
	hit := false
	for _, v := range s.values {
		if strings.Contains(t, v) {
			t, hit = strings.ReplaceAll(t, v, redacted), true
		}
	}
	return t, hit
}

// value returns v with every string in it (keys included) scrubbed, and whether
// anything was replaced. Typed values are converted through JSON first, which is
// the form the output is stored in anyway.
func (s *secretSet) value(v any) (any, bool) {
	if s.empty() || v == nil {
		return v, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v, false
	}
	if _, hit := s.text(string(b)); !hit {
		// cheap check on the encoded form; a value with characters JSON escapes is
		// still found by the walk below
		if !s.escapedIn(string(b)) {
			return v, false
		}
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return v, false
	}
	hit := false
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			r, h := s.text(t)
			hit = hit || h
			return r
		case map[string]any:
			out := make(map[string]any, len(t))
			for k, val := range t {
				nk, h := s.text(k)
				hit = hit || h
				out[nk] = walk(val)
			}
			return out
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		default:
			return x
		}
	}
	out := walk(generic)
	if !hit {
		return v, false
	}
	return out, true
}

// escapedIn reports whether a secret appears in JSON text only in escaped form.
func (s *secretSet) escapedIn(encoded string) bool {
	for _, v := range s.values {
		e, _ := json.Marshal(v)
		if q := string(e[1 : len(e)-1]); q != v && strings.Contains(encoded, q) {
			return true
		}
	}
	return false
}
