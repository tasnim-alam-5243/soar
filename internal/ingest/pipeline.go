package ingest

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/expr"
	"github.com/yourorg/soar/internal/ingest/filters"
	"github.com/yourorg/soar/internal/ingest/mapping"
	"github.com/yourorg/soar/internal/ingest/parsers"
)

// DefaultDedupWindow groups repeats of the same alert into one document (spec 9.9).
const DefaultDedupWindow = 10 * time.Minute

// Pipeline is a source's compiled parse -> filter -> map -> filter chain.
// It holds no I/O, so it is easy to test and safe for concurrent use.
type Pipeline struct {
	Name        string // source name, used in subjects
	System      string // sources sharing a system share external IDs
	Parser      parsers.Parser
	Mapping     *mapping.Compiled
	Filters     []*filters.Set
	DedupWindow time.Duration
	Stats       *Stats
}

// Stats counts what happened to records of one source (spec 9.10).
type Stats struct {
	Accepted, Filtered, Failed atomic.Int64
	LastEventUnix              atomic.Int64
	rules                      sync.Map // "rule|action" -> *atomic.Int64
}

func (s *Stats) rule(rule, action string) {
	v, _ := s.rules.LoadOrStore(rule+"|"+action, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// RuleCounts returns how often each filter rule fired.
func (s *Stats) RuleCounts() map[string]int64 {
	out := map[string]int64{}
	s.rules.Range(func(k, v any) bool { out[k.(string)] = v.(*atomic.Int64).Load(); return true })
	return out
}

// Failure is a record that could not be processed; it is stored and dead-lettered, never dropped.
type Failure struct {
	Record map[string]any
	Stage  string // parse | pre_filter | map | post_filter
	Err    string
}

// Outcome is the result of processing one body.
type Outcome struct {
	Events   []domain.Event
	Filtered int
	Failures []Failure
}

// ProcessBody parses a body and runs every record through the chain.
// A body that cannot be parsed at all is an error; per-record problems are Failures.
func (p *Pipeline) ProcessBody(body []byte) (Outcome, error) {
	recs, err := p.Parser.Parse(body)
	if err != nil {
		return Outcome{}, fmt.Errorf("parse: %w", err)
	}
	return p.ProcessRecords(recs), nil
}

// ProcessRecords runs already-parsed records through the chain.
func (p *Pipeline) ProcessRecords(recs []map[string]any) Outcome {
	var out Outcome
	for _, rec := range recs {
		ev, dropped, fail := p.processOne(rec)
		switch {
		case fail != nil:
			out.Failures = append(out.Failures, *fail)
			p.Stats.Failed.Add(1)
		case dropped:
			out.Filtered++
			p.Stats.Filtered.Add(1)
		default:
			out.Events = append(out.Events, *ev)
			p.Stats.Accepted.Add(1)
			p.Stats.LastEventUnix.Store(time.Now().Unix())
		}
	}
	return out
}

func (p *Pipeline) processOne(raw map[string]any) (ev *domain.Event, dropped bool, fail *Failure) {
	var tags []string
	severity, route := 0, ""
	apply := func(d filters.Decision, set *filters.Set, stage string) bool {
		for _, s := range d.Trace {
			if s.Matched {
				p.Stats.rule(set.Name+"/"+s.Rule, s.Action)
			}
		}
		tags = append(tags, d.Tags...)
		if d.Severity > 0 {
			severity = d.Severity
		}
		if d.Route != "" {
			route = d.Route
		}
		return d.Drop
	}

	vars := map[string]any{"raw": raw, "event": map[string]any{}}
	for _, set := range p.Filters {
		if set.HasStage(filters.PreMap) && apply(set.Eval(filters.PreMap, vars), set, filters.PreMap) {
			return nil, true, nil
		}
	}

	res, err := p.Mapping.Apply(raw)
	if err != nil {
		return nil, false, &Failure{Record: raw, Stage: "map", Err: err.Error()}
	}
	a := res.Alert

	if p.needsPost() {
		em, err := toMap(a)
		if err != nil {
			return nil, false, &Failure{Record: raw, Stage: "post_filter", Err: err.Error()}
		}
		vars["event"] = em
		for _, set := range p.Filters {
			if set.HasStage(filters.PostMap) && apply(set.Eval(filters.PostMap, vars), set, filters.PostMap) {
				return nil, true, nil
			}
		}
	}

	a.Tags = appendUnique(a.Tags, tags...)
	if severity > 0 {
		a.Severity = severity
	}
	now := time.Now().UTC()
	a.SourceID = p.Name
	a.System = p.System
	a.ReceivedAt = now
	a.OccurrenceCount = 1
	a.Status = "new"
	a.Fingerprint = mapping.Fingerprint(p.System, res.DedupKey)
	if a.LastSeen == nil {
		a.LastSeen = &now
	}
	id := domain.NewID(domain.PrefixEvent)
	a.EventID = id
	window := p.DedupWindow
	if window <= 0 {
		window = DefaultDedupWindow
	}
	return &domain.Event{EventID: id, SourceID: p.Name, ReceivedAt: now, DedupWindowSec: int(window.Seconds()), Route: route, Alert: a}, false, nil
}

func (p *Pipeline) needsPost() bool {
	for _, s := range p.Filters {
		if s.HasStage(filters.PostMap) {
			return true
		}
	}
	return false
}

func appendUnique(list []string, add ...string) []string {
	have := map[string]bool{}
	for _, x := range list {
		have[x] = true
	}
	for _, a := range add {
		if !have[a] {
			have[a] = true
			list = append(list, a)
		}
	}
	return list
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

// Build compiles everything a source needs. Lists supplies managed lists for in_list().
type Resolver struct {
	Mappings func(name string, version int) (domain.Mapping, error)
	Filters  func(name string, version int) (domain.FilterSet, error)
}

// ParseRef splits "name@version".
func ParseRef(ref string) (string, int, error) {
	var name string
	var ver int
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '@' {
			name = ref[:i]
			if _, err := fmt.Sscanf(ref[i+1:], "%d", &ver); err != nil || ver < 1 {
				return "", 0, fmt.Errorf("%q: version must be a positive integer", ref)
			}
			return name, ver, nil
		}
	}
	return "", 0, fmt.Errorf("%q: expected name@version", ref)
}

// BuildPipeline assembles a pipeline for a source document.
func BuildPipeline(src domain.Source, lists expr.Lists, r Resolver) (*Pipeline, error) {
	eng, err := expr.New(lists, "raw", "value", "event")
	if err != nil {
		return nil, err
	}
	parser, ok := parsers.Get(src.Parser)
	if !ok {
		return nil, fmt.Errorf("source %s: unknown parser %q (available: %v)", src.Name, src.Parser, parsers.Names())
	}
	mname, mver, err := ParseRef(src.MappingID)
	if err != nil {
		return nil, fmt.Errorf("source %s: mapping: %w", src.Name, err)
	}
	md, err := r.Mappings(mname, mver)
	if err != nil {
		return nil, fmt.Errorf("source %s: mapping %s: %w", src.Name, src.MappingID, err)
	}
	m, err := mapping.Compile(eng, md.Name, md.Version, md.Definition)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", src.Name, err)
	}
	p := &Pipeline{Name: src.Name, System: src.System, Parser: parser, Mapping: m, Stats: &Stats{}, DedupWindow: DefaultDedupWindow}
	if p.System == "" {
		p.System = src.Name
	}
	if w, ok := src.Dedup["window"].(string); ok {
		d, err := time.ParseDuration(w)
		if err != nil || d < time.Second {
			return nil, fmt.Errorf("source %s: dedup.window %q is not a duration like 10m", src.Name, w)
		}
		p.DedupWindow = d
	}
	for _, ref := range src.Filters {
		fname, fver, err := ParseRef(ref)
		if err != nil {
			return nil, fmt.Errorf("source %s: filters: %w", src.Name, err)
		}
		fd, err := r.Filters(fname, fver)
		if err != nil {
			return nil, fmt.Errorf("source %s: filter set %s: %w", src.Name, ref, err)
		}
		set, err := filters.Compile(eng, fd)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", src.Name, err)
		}
		p.Filters = append(p.Filters, set)
	}
	return p, nil
}
