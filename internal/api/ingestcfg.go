package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/expr"
	"github.com/yourorg/soar/internal/ingest"
	"github.com/yourorg/soar/internal/ingest/adapters"
	"github.com/yourorg/soar/internal/ingest/filters"
	"github.com/yourorg/soar/internal/ingest/mapping"
	"github.com/yourorg/soar/internal/ingest/pack"
	"github.com/yourorg/soar/internal/ingest/parsers"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/store"
)

// QueueOps is the part of the NATS client the API exposes (dead letters, stats).
type QueueOps interface {
	DLQList(ctx context.Context, limit int, beforeSeq uint64) ([]queue.DLQEntry, error)
	DLQReplay(ctx context.Context, seq uint64) error
	Stats(ctx context.Context) ([]queue.StreamStats, error)
}

// WithQueue enables the /dlq and /queues endpoints.
func (s *Server) WithQueue(q QueueOps) *Server { s.q = q; return s }

func (s *Server) needQueue() error {
	if s.q == nil {
		return newProblem(http.StatusServiceUnavailable, "queue-unavailable", "Queue unavailable", "this API instance has no connection to NATS")
	}
	return nil
}

var (
	sourceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	envRefRe     = regexp.MustCompile(`^env:[A-Za-z_][A-Za-z0-9_]{0,99}$`)
)

// --- resolvers backed by the store (strong reads by deterministic id) ---

func (s *Server) mappingDoc(ctx context.Context, name string, v int) (domain.Mapping, error) {
	m, _, err := s.st.Mappings.Get(ctx, domain.DeterministicID("map", name, v))
	return m, err
}

func (s *Server) filterDoc(ctx context.Context, name string, v int) (domain.FilterSet, error) {
	f, _, err := s.st.Filters.Get(ctx, domain.DeterministicID("flt", name, v))
	return f, err
}

func (s *Server) resolver(ctx context.Context) ingest.Resolver {
	return ingest.Resolver{
		Mappings: func(n string, v int) (domain.Mapping, error) { return s.mappingDoc(ctx, n, v) },
		Filters:  func(n string, v int) (domain.FilterSet, error) { return s.filterDoc(ctx, n, v) },
	}
}

// storedLists loads the managed lists so test endpoints evaluate in_list() like production does.
func (s *Server) storedLists(ctx context.Context) (expr.Lists, error) {
	l := &memLists{m: map[string]map[string]bool{}}
	cursor := ""
	for {
		p, err := s.st.Lists.List(ctx, store.ListQuery{Limit: 200, After: cursor})
		if err != nil {
			return nil, err
		}
		for _, d := range p.Items {
			set := map[string]bool{}
			for _, it := range d.Items {
				set[it] = true
			}
			l.m[d.Name] = set
		}
		if p.NextCursor == "" {
			return l, nil
		}
		cursor = p.NextCursor
	}
}

type memLists struct{ m map[string]map[string]bool }

func (l *memLists) Contains(list, v string) bool { return l.m[list][v] }

// --- sources ---

type sourceIn struct {
	Name      string         `json:"name"`
	System    string         `json:"system"`
	Adapter   string         `json:"adapter"`
	Parser    string         `json:"parser"`
	Mapping   string         `json:"mapping"`
	Filters   []string       `json:"filters"`
	Auth      map[string]any `json:"auth"`
	Schedule  string         `json:"schedule"`
	Dedup     map[string]any `json:"dedup"`
	RateLimit map[string]any `json:"rate_limit"`
	Config    map[string]any `json:"config"`
	Enabled   *bool          `json:"enabled"`
}

type sourcePatch struct {
	System    *string         `json:"system"`
	Mapping   *string         `json:"mapping"`
	Filters   *[]string       `json:"filters"`
	Auth      *map[string]any `json:"auth"`
	Schedule  *string         `json:"schedule"`
	Dedup     *map[string]any `json:"dedup"`
	RateLimit *map[string]any `json:"rate_limit"`
	Config    *map[string]any `json:"config"`
	Enabled   *bool           `json:"enabled"`
}

// validateSource checks a source document the way the runtime would load it.
// Secrets are never accepted inline: auth and connections reference them by name.
func (s *Server) validateSource(ctx context.Context, d domain.Source) error {
	var v verr
	if !sourceNameRe.MatchString(d.Name) {
		v.add("name", "must be 1-64 characters: lowercase letters, digits, - and _, starting with a letter or digit")
	}
	isPush := d.Adapter == "webhook"
	if !isPush {
		if _, ok := adapters.Get(d.Adapter); !ok {
			v.add("adapter", "must be webhook or one of %v", adapters.Names())
		}
	}
	if _, ok := parsers.Get(d.Parser); !ok {
		v.add("parser", "must be one of %v", parsers.Names())
	}
	if isPush {
		typ, _ := d.Auth["type"].(string)
		switch typ {
		case "token", "hmac", "none":
		default:
			v.add("auth.type", "webhook sources need auth.type token, hmac or none")
		}
		for _, k := range []string{"token_ref", "secret_ref"} {
			if ref, ok := d.Auth[k].(string); ok && !envRefRe.MatchString(ref) {
				v.add("auth."+k, "must reference a secret as env:NAME (named secrets arrive with Step 9); never paste the secret")
			}
		}
	} else {
		if _, err := adapters.ParseSchedule(d.Schedule); err != nil {
			v.add("schedule", "%s", err.Error())
		}
		if name, _ := d.Config["connection"].(string); name == "" {
			v.add("config.connection", "is required for pull sources")
		}
	}
	if d.RateLimit != nil {
		if _, err := ingest.NewLimiter(d.RateLimit); err != nil {
			v.add("rate_limit", "%s", err.Error())
		}
	}
	if len(v.errs) > 0 {
		return v.err()
	}
	// Compile the whole chain. This also proves the mapping and filter sets exist.
	if _, err := ingest.BuildPipeline(d, expr.NoLists{}, s.resolver(ctx)); err != nil {
		var ce *mapping.CompileError
		if errors.As(err, &ce) {
			for _, p := range ce.Problems {
				v.add("mapping", "%s", p)
			}
		} else {
			v.add("mapping", "%s", err.Error())
		}
	}
	return v.err()
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) error {
	return createResource(s, w, r, s.st.Sources, "/sources", func() (domain.Source, string, error) {
		var in sourceIn
		if err := decode(r, &in); err != nil {
			return domain.Source{}, "", err
		}
		if in.System == "" {
			in.System = in.Name
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		doc := domain.Source{
			Meta: domain.NewMeta(domain.PrefixSource), Name: in.Name, System: in.System, Adapter: in.Adapter, Parser: in.Parser,
			MappingID: in.Mapping, Filters: in.Filters, Auth: in.Auth, Schedule: in.Schedule, Dedup: in.Dedup, RateLimit: in.RateLimit,
			Config: in.Config, Enabled: enabled,
		}
		doc.ID = domain.DeterministicID(domain.PrefixSource, in.Name) // one source per name, atomically
		if err := s.validateSource(r.Context(), doc); err != nil {
			return domain.Source{}, "", err
		}
		return doc, doc.ID, nil
	})
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{fields: map[string]string{"created_at": "created_at", "name": "name"}, defaultName: "name"}, "adapter", "enabled", "system")
	if err != nil {
		return err
	}
	term(&q, v, "adapter", "adapter")
	term(&q, v, "enabled", "enabled")
	term(&q, v, "system", "system")
	page, err := s.st.Sources.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) getSource(w http.ResponseWriter, r *http.Request) error {
	return getOne(w, r, s.st.Sources)
}

func (s *Server) patchSource(w http.ResponseWriter, r *http.Request) error {
	var p sourcePatch
	if err := decode(r, &p); err != nil {
		return err
	}
	doc, ver, err := mutate(r, s.st.Sources, r.PathValue("id"), func(d *domain.Source) error {
		if p.System != nil {
			d.System = *p.System
		}
		if p.Mapping != nil {
			d.MappingID = *p.Mapping
		}
		if p.Filters != nil {
			d.Filters = *p.Filters
		}
		if p.Auth != nil {
			d.Auth = *p.Auth
		}
		if p.Schedule != nil {
			d.Schedule = *p.Schedule
		}
		if p.Dedup != nil {
			d.Dedup = *p.Dedup
		}
		if p.RateLimit != nil {
			d.RateLimit = *p.RateLimit
		}
		if p.Config != nil {
			d.Config = *p.Config
		}
		if p.Enabled != nil {
			d.Enabled = *p.Enabled
		}
		return s.validateSource(r.Context(), *d)
	})
	if err != nil {
		return err
	}
	return writeResource(w, http.StatusOK, doc, ver)
}

type sourceHealthOut struct {
	Source  string               `json:"source"`
	Enabled bool                 `json:"enabled"`
	Health  *domain.SourceHealth `json:"health,omitempty"`
}

func (s *Server) sourceHealth(w http.ResponseWriter, r *http.Request) error {
	d, _, err := s.st.Sources.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, sourceHealthOut{Source: d.Name, Enabled: d.Enabled, Health: d.Health})
}

type testRequest struct {
	Payload any `json:"payload"`
}

type failureOut struct {
	Stage string `json:"stage"`
	Error string `json:"error"`
}

type sourceTestOut struct {
	Events   []domain.Event `json:"events"`
	Filtered int            `json:"filtered"`
	Failures []failureOut   `json:"failures"`
	Rules    map[string]int `json:"rules_fired"`
}

// testSource runs a sample payload through a source's whole chain without publishing anything.
func (s *Server) testSource(w http.ResponseWriter, r *http.Request) error {
	var in testRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Payload == nil {
		return unprocessable([]FieldError{{Path: "payload", Message: "is required"}})
	}
	d, _, err := s.st.Sources.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	lists, err := s.storedLists(r.Context())
	if err != nil {
		return err
	}
	p, err := ingest.BuildPipeline(d, lists, s.resolver(r.Context()))
	if err != nil {
		return unprocessable([]FieldError{{Path: "source", Message: err.Error()}})
	}
	var body []byte
	if str, ok := in.Payload.(string); ok {
		body = []byte(str)
	} else {
		body, _ = json.Marshal(in.Payload)
	}
	out, err := p.ProcessBody(body)
	if err != nil {
		return unprocessable([]FieldError{{Path: "payload", Message: err.Error()}})
	}
	res := sourceTestOut{Events: out.Events, Filtered: out.Filtered, Failures: []failureOut{}, Rules: map[string]int{}}
	if res.Events == nil {
		res.Events = []domain.Event{}
	}
	for _, f := range out.Failures {
		res.Failures = append(res.Failures, failureOut{Stage: f.Stage, Error: f.Err})
	}
	for k, n := range p.Stats.RuleCounts() {
		res.Rules[k] = int(n)
	}
	return writeJSON(w, http.StatusOK, res)
}

// --- mappings and filter sets: immutable, versioned ---

func nextVersion(ctx context.Context, exists func(v int) (bool, error), floor int) (int, error) {
	for v := max(floor, 1); v < floor+1000; v++ {
		taken, err := exists(v)
		if err != nil {
			return 0, err
		}
		if !taken {
			return v, nil
		}
	}
	return 0, fmt.Errorf("no free version number")
}

type mappingIn struct {
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Definition map[string]any `json:"definition"`
}

func (s *Server) compileMapping(def map[string]any) error {
	eng, err := expr.New(expr.NoLists{}, mapping.Vars...)
	if err != nil {
		return err
	}
	if _, err := mapping.Compile(eng, "check", 1, def); err != nil {
		var ce *mapping.CompileError
		if errors.As(err, &ce) {
			errs := make([]FieldError, len(ce.Problems))
			for i, p := range ce.Problems {
				errs[i] = FieldError{Path: "definition", Message: p}
			}
			return unprocessable(errs)
		}
		return err
	}
	return nil
}

func (s *Server) createMapping(w http.ResponseWriter, r *http.Request) error {
	return createResource(s, w, r, s.st.Mappings, "/mappings", func() (domain.Mapping, string, error) {
		var in mappingIn
		if err := decode(r, &in); err != nil {
			return domain.Mapping{}, "", err
		}
		var v verr
		if !sourceNameRe.MatchString(strings.ReplaceAll(in.Name, "_", "-")) {
			v.add("name", "must be 1-64 characters: letters, digits, - and _")
		}
		if in.Definition == nil {
			v.add("definition", "is required")
		}
		if err := v.err(); err != nil {
			return domain.Mapping{}, "", err
		}
		if err := s.compileMapping(in.Definition); err != nil {
			return domain.Mapping{}, "", err
		}
		ver, err := nextVersion(r.Context(), func(v int) (bool, error) {
			_, _, err := s.st.Mappings.Get(r.Context(), domain.DeterministicID("map", strings.ToLower(in.Name), v))
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			return err == nil, err
		}, 1)
		if err != nil {
			return domain.Mapping{}, "", err
		}
		name := strings.ToLower(in.Name)
		kind := in.Kind
		if kind == "" {
			kind = "alert"
		}
		doc := domain.Mapping{Meta: domain.NewMeta("map"), Name: name, Version: ver, Kind: kind, Definition: in.Definition, Checksum: pack.Checksum(in.Definition)}
		doc.ID = domain.DeterministicID("map", name, ver)
		return doc, doc.ID, nil
	})
}

func (s *Server) listMappings(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{fields: map[string]string{"created_at": "created_at", "name": "name", "version": "version"}, defaultName: "name"}, "name")
	if err != nil {
		return err
	}
	term(&q, v, "name", "name")
	page, err := s.st.Mappings.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) getMapping(w http.ResponseWriter, r *http.Request) error {
	return getOne(w, r, s.st.Mappings)
}

type mappingTestIn struct {
	Mapping    string         `json:"mapping"`
	Definition map[string]any `json:"definition"`
	Sample     map[string]any `json:"sample"`
}

type mappingTestOut struct {
	Valid    bool           `json:"valid"`
	Alert    *domain.Alert  `json:"alert,omitempty"`
	DedupKey []string       `json:"dedup_key,omitempty"`
	Error    *mappingErrOut `json:"error,omitempty"`
}

type mappingErrOut struct {
	Field   string `json:"field,omitempty"`
	Expr    string `json:"expr,omitempty"`
	Message string `json:"message"`
}

// testMapping returns the normalized alert, or the exact expression that failed.
func (s *Server) testMapping(w http.ResponseWriter, r *http.Request) error {
	var in mappingTestIn
	if err := decode(r, &in); err != nil {
		return err
	}
	var v verr
	if in.Sample == nil {
		v.add("sample", "is required")
	}
	if (in.Mapping == "") == (in.Definition == nil) {
		v.add("mapping", "give exactly one of mapping (name@version) or definition")
	}
	if err := v.err(); err != nil {
		return err
	}
	def := in.Definition
	if in.Mapping != "" {
		name, ver, err := ingest.ParseRef(in.Mapping)
		if err != nil {
			return unprocessable([]FieldError{{Path: "mapping", Message: err.Error()}})
		}
		m, err := s.mappingDoc(r.Context(), name, ver)
		if err != nil {
			return err
		}
		def = m.Definition
	}
	lists, err := s.storedLists(r.Context())
	if err != nil {
		return err
	}
	eng, err := expr.New(lists, mapping.Vars...)
	if err != nil {
		return err
	}
	c, err := mapping.Compile(eng, "test", 1, def)
	if err != nil {
		return writeJSON(w, http.StatusOK, mappingTestOut{Error: &mappingErrOut{Message: err.Error()}})
	}
	res, err := c.Apply(in.Sample)
	if err != nil {
		out := mappingTestOut{Error: &mappingErrOut{Message: err.Error()}}
		var me *mapping.Error
		if errors.As(err, &me) {
			out.Error = &mappingErrOut{Field: me.Field, Expr: me.Expr, Message: me.Err.Error()}
		}
		return writeJSON(w, http.StatusOK, out)
	}
	return writeJSON(w, http.StatusOK, mappingTestOut{Valid: true, Alert: &res.Alert, DedupKey: res.DedupKey})
}

type filterIn struct {
	Name    string           `json:"name"`
	Mode    string           `json:"mode"`
	Default string           `json:"default"`
	Rules   []map[string]any `json:"rules"`
}

func (s *Server) compileFilter(fs domain.FilterSet, lists expr.Lists) (*filters.Set, error) {
	eng, err := expr.New(lists, filters.Vars...)
	if err != nil {
		return nil, err
	}
	return filters.Compile(eng, fs)
}

func toRules(in []map[string]any) []domain.JSON {
	out := make([]domain.JSON, len(in))
	copy(out, in)
	return out
}

func (s *Server) createFilter(w http.ResponseWriter, r *http.Request) error {
	return createResource(s, w, r, s.st.Filters, "/filters", func() (domain.FilterSet, string, error) {
		var in filterIn
		if err := decode(r, &in); err != nil {
			return domain.FilterSet{}, "", err
		}
		var v verr
		if !sourceNameRe.MatchString(strings.ReplaceAll(strings.ToLower(in.Name), "_", "-")) {
			v.add("name", "must be 1-64 characters: letters, digits, - and _")
		}
		if len(in.Rules) == 0 {
			v.add("rules", "at least one rule is required")
		}
		if err := v.err(); err != nil {
			return domain.FilterSet{}, "", err
		}
		name := strings.ToLower(in.Name)
		fs := domain.FilterSet{Name: name, Mode: in.Mode, Default: in.Default, Rules: toRules(in.Rules)}
		if _, err := s.compileFilter(fs, expr.NoLists{}); err != nil {
			return domain.FilterSet{}, "", unprocessable([]FieldError{{Path: "rules", Message: err.Error()}})
		}
		ver, err := nextVersion(r.Context(), func(v int) (bool, error) {
			_, _, err := s.st.Filters.Get(r.Context(), domain.DeterministicID("flt", name, v))
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			return err == nil, err
		}, 1)
		if err != nil {
			return domain.FilterSet{}, "", err
		}
		fs.Meta, fs.Version, fs.Checksum = domain.NewMeta("flt"), ver, pack.Checksum(fs.Rules)
		fs.ID = domain.DeterministicID("flt", name, ver)
		if fs.Mode == "" {
			fs.Mode = "first_match"
		}
		if fs.Default == "" {
			fs.Default = "keep"
		}
		return fs, fs.ID, nil
	})
}

func (s *Server) listFilters(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{fields: map[string]string{"created_at": "created_at", "name": "name", "version": "version"}, defaultName: "name"}, "name")
	if err != nil {
		return err
	}
	term(&q, v, "name", "name")
	page, err := s.st.Filters.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) getFilter(w http.ResponseWriter, r *http.Request) error {
	return getOne(w, r, s.st.Filters)
}

type filterTestIn struct {
	FilterSet string         `json:"filter_set"`
	Inline    *filterIn      `json:"inline"`
	Stage     string         `json:"stage"`
	Sample    map[string]any `json:"sample"`
}

type filterTestOut struct {
	Drop     bool           `json:"drop"`
	Rule     string         `json:"rule,omitempty"`
	Tags     []string       `json:"tags"`
	Severity int            `json:"severity,omitempty"`
	Route    string         `json:"route,omitempty"`
	Trace    []filters.Step `json:"trace"`
}

// testFilter evaluates a filter set against one sample and returns a rule-by-rule trace.
func (s *Server) testFilter(w http.ResponseWriter, r *http.Request) error {
	var in filterTestIn
	if err := decode(r, &in); err != nil {
		return err
	}
	var v verr
	if in.Sample == nil {
		v.add("sample", "is required")
	}
	if (in.FilterSet == "") == (in.Inline == nil) {
		v.add("filter_set", "give exactly one of filter_set (name@version) or inline")
	}
	if in.Stage == "" {
		in.Stage = filters.PreMap
	}
	v.oneOf("stage", in.Stage, filters.PreMap, filters.PostMap)
	if err := v.err(); err != nil {
		return err
	}
	var fs domain.FilterSet
	if in.FilterSet != "" {
		name, ver, err := ingest.ParseRef(in.FilterSet)
		if err != nil {
			return unprocessable([]FieldError{{Path: "filter_set", Message: err.Error()}})
		}
		if fs, err = s.filterDoc(r.Context(), name, ver); err != nil {
			return err
		}
	} else {
		fs = domain.FilterSet{Name: "inline", Mode: in.Inline.Mode, Default: in.Inline.Default, Rules: toRules(in.Inline.Rules)}
	}
	lists, err := s.storedLists(r.Context())
	if err != nil {
		return err
	}
	set, err := s.compileFilter(fs, lists)
	if err != nil {
		return unprocessable([]FieldError{{Path: "filter_set", Message: err.Error()}})
	}
	vars := map[string]any{"raw": map[string]any{}, "event": map[string]any{}}
	if in.Stage == filters.PreMap {
		vars["raw"] = in.Sample
	} else {
		vars["event"] = in.Sample
	}
	d := set.Eval(in.Stage, vars)
	out := filterTestOut{Drop: d.Drop, Rule: d.Rule, Tags: d.Tags, Severity: d.Severity, Route: d.Route, Trace: d.Trace}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if out.Trace == nil {
		out.Trace = []filters.Step{}
	}
	return writeJSON(w, http.StatusOK, out)
}

// --- managed lists ---

type listIn struct {
	Items []string `json:"items"`
}

func listID(name string) string { return domain.DeterministicID("lst", name) }

func (s *Server) listLists(w http.ResponseWriter, r *http.Request) error {
	q, _, err := listQuery(r, sortSpec{fields: map[string]string{"name": "name"}, defaultName: "name"})
	if err != nil {
		return err
	}
	page, err := s.st.Lists.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) getList(w http.ResponseWriter, r *http.Request) error {
	d, ver, err := s.st.Lists.Get(r.Context(), listID(r.PathValue("name")))
	if err != nil {
		return err
	}
	return writeResource(w, http.StatusOK, d, ver)
}

// putList replaces a managed list. Filters see the change on their next reload (seconds).
func (s *Server) putList(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	var in listIn
	if err := decode(r, &in); err != nil {
		return err
	}
	var v verr
	if !sourceNameRe.MatchString(strings.ReplaceAll(strings.ToLower(name), "_", "-")) {
		v.add("name", "must be 1-64 characters: letters, digits, - and _")
	}
	if len(in.Items) > 100000 {
		v.add("items", "at most 100000 items")
	}
	for i, it := range in.Items {
		if it == "" || len(it) > 512 {
			v.add("items["+strconv.Itoa(i)+"]", "must be 1-512 characters")
			break
		}
	}
	if err := v.err(); err != nil {
		return err
	}
	if in.Items == nil {
		in.Items = []string{}
	}
	doc := domain.List{Meta: domain.Meta{ID: listID(name)}, Name: name, Items: in.Items}
	ver, err := s.st.Lists.Put(r.Context(), doc)
	if err != nil {
		return err
	}
	stored, sv, err := s.st.Lists.Get(r.Context(), doc.ID)
	if err != nil {
		return writeResource(w, http.StatusOK, doc, ver)
	}
	return writeResource(w, http.StatusOK, stored, sv)
}

// --- dead letters and queue statistics ---

type dlqOut struct {
	Items      []queue.DLQEntry `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

func (s *Server) listDLQ(w http.ResponseWriter, r *http.Request) error {
	if err := s.needQueue(); err != nil {
		return err
	}
	v := r.URL.Query()
	for k := range v {
		if k != "limit" && k != "cursor" {
			return badRequest("unknown query parameter %q", k)
		}
	}
	limit := defaultLimit
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxLimit {
			return badRequest("limit must be an integer from 1 to %d", maxLimit)
		}
		limit = n
	}
	var before uint64
	if c := v.Get("cursor"); c != "" {
		n, err := strconv.ParseUint(c, 10, 64)
		if err != nil {
			return badRequest("cursor is not valid")
		}
		before = n
	}
	items, err := s.q.DLQList(r.Context(), limit, before)
	if err != nil {
		return err
	}
	out := dlqOut{Items: items}
	if out.Items == nil {
		out.Items = []queue.DLQEntry{}
	}
	if len(items) == limit {
		out.NextCursor = strconv.FormatUint(items[len(items)-1].Seq, 10)
	}
	return writeJSON(w, http.StatusOK, out)
}

type replayOut struct {
	Replayed uint64 `json:"replayed"`
}

func (s *Server) replayDLQ(w http.ResponseWriter, r *http.Request) error {
	if err := s.needQueue(); err != nil {
		return err
	}
	seq, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		return badRequest("the dead-letter id is a sequence number")
	}
	if err := s.q.DLQReplay(r.Context(), seq); err != nil {
		if errors.Is(err, queue.ErrNotInDLQ) {
			return notFound("no such dead-letter message (already replayed?)")
		}
		return err
	}
	return writeJSON(w, http.StatusOK, replayOut{Replayed: seq})
}

type queueStatsOut struct {
	Streams []queue.StreamStats `json:"streams"`
	At      time.Time           `json:"at"`
}

func (s *Server) queueStats(w http.ResponseWriter, r *http.Request) error {
	if err := s.needQueue(); err != nil {
		return err
	}
	st, err := s.q.Stats(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, queueStatsOut{Streams: st, At: s.now()})
}
