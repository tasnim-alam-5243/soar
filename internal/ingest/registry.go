package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

// Runtime is a source that is ready to serve: configuration, compiled pipeline,
// authenticator and rate limiter.
type Runtime struct {
	Source   domain.Source
	Pipeline *Pipeline
	Auth     Authenticator
	Limiter  *Limiter
	Err      error // set when the source could not be compiled; it is skipped until fixed
	sig      string
}

// Lists is the in-memory copy of managed lists used by in_list() (spec 9.5.1).
type Lists struct {
	mu sync.RWMutex
	m  map[string]map[string]bool
}

// Contains implements expr.Lists.
func (l *Lists) Contains(list, value string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.m[list][value]
}

func (l *Lists) set(m map[string]map[string]bool) {
	l.mu.Lock()
	l.m = m
	l.mu.Unlock()
}

// PushAdapter is the transport served by `soar ingest`; every other adapter is pulled by `soar collector`.
const PushAdapter = "webhook"

// IsPush reports whether a source is received by the ingest role (and therefore needs inbound auth).
func IsPush(s domain.Source) bool { return s.Adapter == PushAdapter }

// IsPull reports whether a source is polled by the collector role.
func IsPull(s domain.Source) bool { return !IsPush(s) }

// Registry keeps compiled sources in memory and reloads them from the store, so
// changes to sources, mappings, filters and lists apply without a restart.
type Registry struct {
	// Serves selects the sources this process loads (IsPush in ingest, IsPull in the
	// collector). Nil loads every source. Sources of the other role are never compiled,
	// so their secrets need not be available here.
	Serves func(domain.Source) bool

	st      *store.Store
	log     *slog.Logger
	secrets func(ref string) (string, error)

	lists  *Lists
	mu     sync.RWMutex
	byName map[string]*Runtime

	failing map[string]string // source -> last logged load error, so a broken source is logged once, not every refresh
	last    map[string]healthSample
}

// NewRegistry builds an empty registry; call Refresh or Run to fill it.
func NewRegistry(st *store.Store, log *slog.Logger, secrets func(string) (string, error)) *Registry {
	return &Registry{
		st: st, log: log, secrets: secrets, lists: &Lists{m: map[string]map[string]bool{}}, byName: map[string]*Runtime{},
		failing: map[string]string{}, last: map[string]healthSample{},
	}
}

func listAll[T any](ctx context.Context, repo store.Repo[T]) ([]T, error) {
	var out []T
	cursor := ""
	for {
		p, err := repo.List(ctx, store.ListQuery{Limit: 200, After: cursor, Sort: "created_at"})
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.NextCursor == "" {
			return out, nil
		}
		cursor = p.NextCursor
	}
}

// Refresh reloads lists and sources. A source that fails to compile keeps its previous
// working runtime (so a bad edit cannot stop ingestion) and the error is logged.
func (r *Registry) Refresh(ctx context.Context) error {
	lists, err := listAll(ctx, r.st.Lists)
	if err != nil {
		return fmt.Errorf("load lists: %w", err)
	}
	lm := map[string]map[string]bool{}
	for _, l := range lists {
		set := map[string]bool{}
		for _, it := range l.Items {
			set[it] = true
		}
		lm[l.Name] = set
	}
	r.lists.set(lm)

	sources, err := listAll(ctx, r.st.Sources)
	if err != nil {
		return fmt.Errorf("load sources: %w", err)
	}
	mappings, err := listAll(ctx, r.st.Mappings)
	if err != nil {
		return fmt.Errorf("load mappings: %w", err)
	}
	filterSets, err := listAll(ctx, r.st.Filters)
	if err != nil {
		return fmt.Errorf("load filters: %w", err)
	}
	res := Resolver{
		Mappings: func(name string, v int) (domain.Mapping, error) {
			for _, m := range mappings {
				if m.Name == name && m.Version == v {
					return m, nil
				}
			}
			return domain.Mapping{}, fmt.Errorf("not found")
		},
		Filters: func(name string, v int) (domain.FilterSet, error) {
			for _, f := range filterSets {
				if f.Name == name && f.Version == v {
					return f, nil
				}
			}
			return domain.FilterSet{}, fmt.Errorf("not found")
		},
	}

	next := map[string]*Runtime{}
	r.mu.RLock()
	old := r.byName
	r.mu.RUnlock()
	stillFailing := map[string]string{}
	for _, s := range sources {
		if r.Serves != nil && !r.Serves(s) {
			continue // the other role's source: its auth and connection secrets are not ours to resolve
		}
		sig := fmt.Sprintf("%s|%s|%s", ConfigSignature(s), signature(mappings, s.MappingID), signature(filterSets, s.Filters...))
		if prev, ok := old[s.Name]; ok && prev.sig == sig && prev.Err == nil {
			prev.Source = s
			next[s.Name] = prev
			continue
		}
		rt := &Runtime{Source: s, sig: sig}
		p, err := BuildPipeline(s, r.lists, res)
		if err == nil {
			rt.Pipeline = p
			if prev, ok := old[s.Name]; ok && prev.Pipeline != nil {
				rt.Pipeline.Stats = prev.Pipeline.Stats // counters survive a config reload
			}
			if IsPush(s) {
				// Only pushed data is authenticated on arrival; pull sources authenticate
				// outbound through their connection (spec 9.5).
				rt.Auth, err = NewAuthenticator(s.Auth, r.secrets)
			}
		}
		if err == nil && s.RateLimit != nil {
			rt.Limiter, err = NewLimiter(s.RateLimit)
		}
		if err != nil {
			rt.Err = err
			// A broken source is retried every refresh (a secret may appear later), but the
			// error is logged only when it first appears or changes.
			key := sig + "|" + err.Error()
			stillFailing[s.Name] = key
			if r.failing[s.Name] != key {
				r.log.Error("source cannot be loaded", "source", s.Name, "error", err.Error())
			}
			if prev, ok := old[s.Name]; ok && prev.Err == nil {
				next[s.Name] = prev // keep serving with the last good configuration
				continue
			}
		} else if _, was := r.failing[s.Name]; was {
			r.log.Info("source loaded", "source", s.Name)
		}
		next[s.Name] = rt
	}
	r.mu.Lock()
	r.byName = next
	r.failing = stillFailing
	r.mu.Unlock()
	return nil
}

// ConfigSignature identifies a source's configuration: everything except its metadata
// and health. Health is written every 15 s while data flows and bumps updated_at, so
// updated_at cannot tell a configuration change from a health report.
func ConfigSignature(s domain.Source) string {
	s.Meta, s.Health = domain.Meta{}, nil
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// Signature identifies the configuration a runtime was built from (source, mapping and
// filter sets). It changes only when one of them is edited.
func (rt *Runtime) Signature() string { return rt.sig }

func signature[T any](docs []T, refs ...string) string {
	// UpdatedAt of the referenced mappings/filter sets is enough to notice edits.
	var out string
	for _, ref := range refs {
		name, v, err := ParseRef(ref)
		if err != nil {
			continue
		}
		switch ds := any(docs).(type) {
		case []domain.Mapping:
			for _, m := range ds {
				if m.Name == name && m.Version == v {
					out += m.Checksum
				}
			}
		case []domain.FilterSet:
			for _, f := range ds {
				if f.Name == name && f.Version == v {
					out += f.Checksum
				}
			}
		}
	}
	return out
}

// Get returns a loaded, enabled source.
func (r *Registry) Get(name string) (*Runtime, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.byName[name]
	if !ok || !rt.Source.Enabled || rt.Pipeline == nil {
		return nil, false
	}
	return rt, true
}

// All returns every loaded runtime (including disabled ones).
func (r *Registry) All() []*Runtime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Runtime, 0, len(r.byName))
	for _, rt := range r.byName {
		out = append(out, rt)
	}
	return out
}

// Run refreshes every interval until ctx ends. The first refresh is synchronous.
func (r *Registry) Run(ctx context.Context, every time.Duration) error {
	if err := r.Refresh(ctx); err != nil {
		r.log.Error("initial source load failed; retrying", "error", err.Error())
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := r.Refresh(ctx); err != nil {
				r.log.Warn("source reload failed; keeping the previous configuration", "error", err.Error())
			}
		}
	}
}

type healthSample struct {
	at                         time.Time
	accepted, filtered, failed int64
}

// ReportHealth writes per-source health (spec 9.10) into the source documents.
// It only touches sources whose counters changed, so an idle source costs nothing.
func (r *Registry) ReportHealth(ctx context.Context, roles func(domain.Source) bool) {
	now := time.Now().UTC()
	for _, rt := range r.All() {
		if rt.Pipeline == nil || (roles != nil && !roles(rt.Source)) {
			continue
		}
		st := rt.Pipeline.Stats
		cur := healthSample{at: now, accepted: st.Accepted.Load(), filtered: st.Filtered.Load(), failed: st.Failed.Load()}
		prev, seen := r.last[rt.Source.Name]
		r.last[rt.Source.Name] = cur
		if seen && prev.accepted == cur.accepted && prev.failed == cur.failed && prev.filtered == cur.filtered {
			continue
		}
		var rate, errRate float64
		if seen {
			if dt := now.Sub(prev.at).Seconds(); dt > 0 {
				rate = float64(cur.accepted-prev.accepted) / dt
				if total := cur.accepted - prev.accepted + cur.failed - prev.failed; total > 0 {
					errRate = float64(cur.failed-prev.failed) / float64(total)
				}
			}
		}
		h := domain.SourceHealth{Status: "ok", RatePerSec: rate, ErrorRate: errRate}
		if ts := st.LastEventUnix.Load(); ts > 0 {
			t := time.Unix(ts, 0).UTC()
			h.LastEventAt = &t
		}
		if errRate > 0.5 {
			h.Status = "degraded"
		}
		_, _, err := r.st.Sources.Update(ctx, rt.Source.ID, func(s *domain.Source) error { s.Health = &h; return nil })
		if err != nil {
			r.log.Warn("could not record source health", "source", rt.Source.Name, "error", err.Error())
		}
	}
}
