// Package parsers turns a raw body into records (spec 9.4). Step 5 ships json and
// ndjson; the others (xml, csv, cef, leef, syslog, keyvalue, regex) register the same way.
package parsers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Parser converts one raw body into zero or more records.
type Parser interface {
	Parse(body []byte) ([]map[string]any, error)
}

var registry = map[string]Parser{}

// Register adds a parser under a name. It panics on duplicates (a programming error).
func Register(name string, p Parser) {
	if _, dup := registry[name]; dup {
		panic("parsers: duplicate parser " + name)
	}
	registry[name] = p
}

// Get returns the parser registered under name.
func Get(name string) (Parser, bool) { p, ok := registry[name]; return p, ok }

// Names lists registered parsers.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func init() {
	Register("json", jsonParser{})
	Register("ndjson", ndjsonParser{})
}

type jsonParser struct{}

// Parse accepts one object or an array of objects.
func (jsonParser) Parse(body []byte) ([]map[string]any, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("invalid JSON: data after the first value (use the ndjson parser for several records)")
	}
	switch t := v.(type) {
	case map[string]any:
		return []map[string]any{t}, nil
	case []any:
		out := make([]map[string]any, 0, len(t))
		for i, x := range t {
			m, ok := x.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("record %d is not a JSON object", i)
			}
			out = append(out, m)
		}
		return out, nil
	}
	return nil, fmt.Errorf("body must be a JSON object or an array of objects")
}

type ndjsonParser struct{}

// Parse reads one JSON object per line; blank lines are skipped.
func (ndjsonParser) Parse(body []byte) ([]map[string]any, error) {
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no records")
	}
	return out, nil
}
