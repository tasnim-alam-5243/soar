// Package memory is an in-process store.Store for unit tests and local experiments.
// It mimics the OpenSearch behaviour the platform relies on: per-document versions,
// create-conflicts, optimistic Save, bounded-retry Update, filters, sorting and cursors.
package memory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

type metaer interface{ MetaPtr() *domain.Meta }

type entry struct {
	raw []byte
	seq int64
}

// Repo is an in-memory store.Repo.
type Repo[T any] struct {
	mu    sync.Mutex
	docs  map[string]entry
	seq   int64
	index string
	meta  func(*T) *domain.Meta
	// MaxRetries bounds Update retries (default 8).
	MaxRetries int
}

var _ store.Repo[domain.Case] = (*Repo[domain.Case])(nil)

// NewRepo creates an empty repository. index is only used to fill Version.Index.
func NewRepo[T any, P interface {
	*T
	metaer
}](index string) *Repo[T] {
	return &Repo[T]{docs: map[string]entry{}, index: index, MaxRetries: 8, meta: func(t *T) *domain.Meta { return P(t).MetaPtr() }}
}

func (r *Repo[T]) ver(e entry) store.Version {
	return store.Version{Index: r.index, SeqNo: e.seq, PrimaryTerm: 1}
}

func (r *Repo[T]) stamp(doc *T, isNew bool) (string, error) {
	m := r.meta(doc)
	if m.ID == "" {
		return "", fmt.Errorf("%w: document has no id", store.ErrInvalid)
	}
	now := time.Now().UTC()
	if isNew && m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if m.SchemaVersion == 0 {
		m.SchemaVersion = domain.SchemaVersion
	}
	return m.ID, nil
}

func (r *Repo[T]) store(id string, doc T) (store.Version, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return store.Version{}, err
	}
	r.seq++
	e := entry{raw: raw, seq: r.seq}
	r.docs[id] = e
	return r.ver(e), nil
}

func (r *Repo[T]) Get(_ context.Context, id string) (T, store.Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var doc T
	e, ok := r.docs[id]
	if !ok {
		return doc, store.Version{}, store.ErrNotFound
	}
	if err := json.Unmarshal(e.raw, &doc); err != nil {
		return doc, store.Version{}, err
	}
	return doc, r.ver(e), nil
}

func (r *Repo[T]) Create(_ context.Context, doc T) (store.Version, error) {
	id, err := r.stamp(&doc, true)
	if err != nil {
		return store.Version{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.docs[id]; ok {
		return store.Version{}, store.ErrAlreadyExists
	}
	return r.store(id, doc)
}

func (r *Repo[T]) Put(_ context.Context, doc T) (store.Version, error) {
	id, err := r.stamp(&doc, true)
	if err != nil {
		return store.Version{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.store(id, doc)
}

func (r *Repo[T]) Save(_ context.Context, doc T, v store.Version) (store.Version, error) {
	if v.IsZero() {
		return store.Version{}, fmt.Errorf("%w: Save needs the version returned by Get", store.ErrInvalid)
	}
	id, err := r.stamp(&doc, false)
	if err != nil {
		return store.Version{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.docs[id]
	if !ok || cur.seq != v.SeqNo || v.PrimaryTerm != 1 {
		return store.Version{}, store.ErrVersionConflict
	}
	return r.store(id, doc)
}

func (r *Repo[T]) Update(ctx context.Context, id string, mutate func(*T) error) (T, store.Version, error) {
	var zero T
	for attempt := 0; attempt <= r.MaxRetries; attempt++ {
		doc, v, err := r.Get(ctx, id)
		if err != nil {
			return zero, store.Version{}, err
		}
		if err := mutate(&doc); err != nil {
			return zero, store.Version{}, err
		}
		if r.meta(&doc).ID != id {
			return zero, store.Version{}, fmt.Errorf("%w: update must not change the id", store.ErrInvalid)
		}
		nv, err := r.Save(ctx, doc, v)
		if err == nil {
			return doc, nv, nil
		}
		if !errors.Is(err, store.ErrVersionConflict) {
			return zero, store.Version{}, err
		}
		if ctx.Err() != nil {
			return zero, store.Version{}, ctx.Err()
		}
	}
	return zero, store.Version{}, store.ErrConflictExhausted
}

func (r *Repo[T]) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.docs[id]; !ok {
		return store.ErrNotFound
	}
	delete(r.docs, id)
	return nil
}

type row struct {
	id   string
	raw  []byte
	data map[string]any
}

func (r *Repo[T]) List(_ context.Context, q store.ListQuery) (store.Page[T], error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	field := q.Sort
	if field == "" {
		field = "created_at"
	}

	r.mu.Lock()
	rows := make([]row, 0, len(r.docs))
	for id, e := range r.docs {
		var m map[string]any
		if err := json.Unmarshal(e.raw, &m); err != nil {
			r.mu.Unlock()
			return store.Page[T]{}, err
		}
		if matches(m, q) {
			rows = append(rows, row{id: id, raw: e.raw, data: m})
		}
	}
	r.mu.Unlock()

	less := func(a, b row) bool {
		c := compareValues(lookup(a.data, field), lookup(b.data, field))
		if c == 0 {
			c = strings.Compare(a.id, b.id)
		}
		if q.Desc {
			return c > 0
		}
		return c < 0
	}
	sort.Slice(rows, func(i, j int) bool { return less(rows[i], rows[j]) })

	if q.After != "" {
		rawc, err := base64.RawURLEncoding.DecodeString(q.After)
		var cur [2]any
		if err != nil || json.Unmarshal(rawc, &cur) != nil {
			return store.Page[T]{}, fmt.Errorf("%w: bad cursor", store.ErrInvalid)
		}
		curRow := row{id: fmt.Sprint(cur[1]), data: map[string]any{field: cur[0]}}
		i := sort.Search(len(rows), func(i int) bool { return less(curRow, rows[i]) })
		rows = rows[i:]
	}

	page := store.Page[T]{}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		rawc, _ := json.Marshal([2]any{lookup(last.data, field), last.id})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(rawc)
	}
	page.Items = make([]T, 0, len(rows))
	for _, rw := range rows {
		var d T
		if err := json.Unmarshal(rw.raw, &d); err != nil {
			return store.Page[T]{}, err
		}
		page.Items = append(page.Items, d)
	}
	return page, nil
}

func matches(m map[string]any, q store.ListQuery) bool {
	for field, want := range q.Terms {
		got := valuesOf(lookup(m, field))
		ok := false
		for _, g := range got {
			for _, w := range want {
				if g == w {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	for _, rg := range q.Ranges {
		v := lookup(m, rg.Field)
		if v == nil {
			return false
		}
		if rg.Gte != nil && compareValues(v, rg.Gte) < 0 {
			return false
		}
		if rg.Lte != nil && compareValues(v, rg.Lte) > 0 {
			return false
		}
	}
	return true
}

// lookup resolves a dotted path ("trigger.alert_id") in decoded JSON.
func lookup(m map[string]any, path string) any {
	var cur any = m
	for _, part := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			if part == "raw" && cur != nil { // emulate the keyword sub-field of text+keyword mappings
				return cur
			}
			return nil
		}
		cur = mm[part]
	}
	return cur
}

// valuesOf flattens a scalar or array into comparable strings.
func valuesOf(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		var out []string
		for _, x := range t {
			out = append(out, valuesOf(x)...)
		}
		return out
	case float64:
		return []string{strconv.FormatFloat(t, 'f', -1, 64)}
	default:
		return []string{fmt.Sprint(t)}
	}
}

// compareValues orders nil last-ish (as smallest), then times, numbers, strings.
func compareValues(a, b any) int {
	if a == nil || b == nil {
		switch {
		case a == nil && b == nil:
			return 0
		case a == nil:
			return -1
		default:
			return 1
		}
	}
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			ta, ea := time.Parse(time.RFC3339Nano, sa)
			tb, eb := time.Parse(time.RFC3339Nano, sb)
			if ea == nil && eb == nil {
				return ta.Compare(tb)
			}
			return strings.Compare(sa, sb)
		}
	}
	fa, oka := toFloat(a)
	fb, okb := toFloat(b)
	if oka && okb {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

// NewStore returns a Store with an empty in-memory repository for every index.
func NewStore(prefix string) *store.Store {
	n := store.Names{Prefix: prefix}
	return &store.Store{
		Names:       n,
		Workflows:   NewRepo[domain.Workflow](n.Index("workflows")),
		Executions:  NewRepo[domain.Execution](n.Index("executions")),
		StepRuns:    NewRepo[domain.StepRun](n.Index("steprun")),
		Alerts:      NewRepo[domain.Alert](n.Index("alerts")),
		Cases:       NewRepo[domain.Case](n.Index("cases")),
		CaseTasks:   NewRepo[domain.CaseTask](n.Index("case-tasks")),
		CaseAlerts:  NewRepo[domain.CaseAlert](n.Index("case-alerts")),
		Observables: NewRepo[domain.ObservableDoc](n.Index("observables")),
		Timeline:    NewRepo[domain.TimelineEvent](n.Index("timeline")),
		Audit:       NewRepo[domain.AuditRecord](n.Index("audit")),
		Secrets:     NewRepo[domain.Secret](n.Index("secrets")),
		Principals:  NewRepo[domain.Principal](n.Index("principals")),
		Idempotency: NewRepo[domain.Idempotency](n.Index("idempotency")),
		Timers:      NewRepo[domain.Timer](n.Index("timers")),
		Approvals:   NewRepo[domain.Approval](n.Index("approvals")),
		Sources:     NewRepo[domain.Source](n.Index("sources")),
		Mappings:    NewRepo[domain.Mapping](n.Index("mappings")),
		Filters:     NewRepo[domain.FilterSet](n.Index("filters")),
		Lists:       NewRepo[domain.List](n.Index("lists")),
		Cursors:     NewRepo[domain.Cursor](n.Index("cursors")),
		Raw:         NewRepo[domain.RawRecord](n.Index("raw")),
		Seen:        NewRepo[domain.Seen](n.Index("seen")),
		Connections: NewRepo[domain.Connection](n.Index("connections")),
	}
}

// Count implements store.Counter.
func (r *Repo[T]) Count(_ context.Context, q store.ListQuery) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, e := range r.docs {
		var m map[string]any
		if err := json.Unmarshal(e.raw, &m); err != nil {
			return 0, err
		}
		if matches(m, q) {
			n++
		}
	}
	return n, nil
}
