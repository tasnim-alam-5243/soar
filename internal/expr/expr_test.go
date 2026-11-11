package expr

import (
	"encoding/json"
	"reflect"
	"testing"
)

type fakeLists map[string][]string

func (f fakeLists) Contains(list, v string) bool {
	for _, x := range f[list] {
		if x == v {
			return true
		}
	}
	return false
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHelpers(t *testing.T) {
	raw := decode(t, `{"id":"1793256511.10907","timestamp":"2026-10-29T12:48:31.597+0600",
	  "rule":{"id":"5715","level":3,"description":"sshd: authentication success.","groups":["syslog","sshd"]},
	  "agent":{"name":"web01"},"data":{"srcip":"10.10.192.1","dstuser":"root"},
	  "syscheck":{"sha1_after":"da39a3ee5e6b4b0d3255bfef95601890afd80709","md5_after":"0123456789abcdef0123456789abcdef"},
	  "full_log":"fetched http://evil.example/x.sh from 203.0.113.9 by a@b.io sha 0123456789abcdef0123456789abcdef"}`)
	e, err := New(fakeLists{"noisy": {"5715"}, "empty": nil}, "raw", "item")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		expr string
		want any
	}{
		{`raw.rule.id`, "5715"},
		{`int(raw.rule.level) < 4`, true},
		{`raw.rule.level >= 3`, true},
		{`get_path(raw, "data.srcip")`, "10.10.192.1"},
		{`get_path(raw, "data.nothing")`, nil},
		{`get_path(raw, "mitre.id")`, nil},
		{`coalesce(get_path(raw, "mitre.id"), "fallback")`, "fallback"},
		{`coalesce(get_path(raw, "data.dstuser"), "x")`, "root"},
		{`coalesce(get_path(raw, "a"), get_path(raw, "b"), "")`, ""},
		{`coalesce(get_path(raw, "a"), get_path(raw, "b"), get_path(raw, "data.srcip"), "z")`, "10.10.192.1"},
		{`in_list(raw.rule.id, "noisy")`, true},
		{`in_list(raw.agent.name, "noisy")`, false},
		{`in_list(raw.rule.level, "noisy")`, false},
		{`raw.rule.description.matches("(?i)authentication")`, true},
		{`level_map(raw.rule.level, [[0,3,1],[4,6,2],[7,9,3]])`, int64(1)},
		{`level_map(7, [[0,3,1],[4,6,2],[7,9,3]])`, int64(3)},
		{`level_map(99, [[0,3,1]])`, int64(3)},
		{`severity_map("HIGH", {"critical":5,"high":4,"low":2})`, int64(4)},
		{`severity_map("weird", {"high":4})`, int64(3)},
		{`parse_time(raw.timestamp)`, "2026-10-29T06:48:31.597Z"},
		{`parse_time("29/10/2026", "02/01/2006")`, "2026-10-29T00:00:00Z"},
		{`lower("AbC")`, "abc"},
		{`regex_extract(raw.full_log, "by (\\S+@\\S+)")`, "a@b.io"},
		{`regex_extract("nothing", "(\\d+)")`, ""},
		{`extract_ips(raw.full_log)`, []any{"203.0.113.9"}},
		{`extract_urls(raw.full_log)`, []any{"http://evil.example/x.sh"}},
		{`extract_emails(raw.full_log)`, []any{"a@b.io"}},
		{`extract_hashes(raw.full_log)`, []any{"0123456789abcdef0123456789abcdef"}},
		{`is_empty_hash(raw.syscheck.sha1_after)`, true},
		{`is_empty_hash(raw.syscheck.md5_after)`, false},
		{`is_empty_hash(get_path(raw, "syscheck.sha256_after"))`, true},
		{`raw.rule.groups.exists(g, g == "sshd")`, true},
		{`has(raw.mitre)`, false},
		{`["siem"]`, []any{"siem"}},
		{`string(raw.rule.level)`, "3"},
	}
	for _, c := range cases {
		p, err := e.Compile(c.expr)
		if err != nil {
			t.Errorf("%s: compile: %v", c.expr, err)
			continue
		}
		got, err := p.Eval(map[string]any{"raw": raw})
		if err != nil {
			t.Errorf("%s: eval: %v", c.expr, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.expr, got, c.want)
		}
	}
}

func TestErrorsAreReported(t *testing.T) {
	e, _ := New(nil, "raw")
	if _, err := e.Compile(`raw.rule.level >=`); err == nil {
		t.Error("syntax error must fail at compile time")
	}
	if _, err := e.Compile(`no_such_function(raw)`); err == nil {
		t.Error("unknown function must fail at compile time")
	}
	p, _ := e.Compile(`raw.missing.key`)
	if _, err := p.Eval(map[string]any{"raw": map[string]any{}}); err == nil {
		t.Error("a missing key without get_path must be a runtime error naming the problem")
	}
	b, _ := e.Compile(`"text"`)
	if _, err := b.Bool(nil); err == nil {
		t.Error("Bool must reject non-boolean results")
	}
	p, _ = e.Compile(`parse_time("garbage")`)
	if _, err := p.Eval(nil); err == nil {
		t.Error("parse_time of garbage must be an error")
	}
}

func TestText(t *testing.T) {
	for in, want := range map[any]string{5715.0: "5715", "x": "x", nil: "", 1.5: "1.5", true: "true"} {
		if got := Text(in); got != want {
			t.Errorf("Text(%v)=%q want %q", in, got, want)
		}
	}
}
