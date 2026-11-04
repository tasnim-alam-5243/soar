package opensearch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/opensearch/schema"
)

// Options tune every repository created by NewStore.
type Options struct {
	// Refresh is passed to every write ("", "true" or "wait_for"). Empty is right
	// for production; tests set "true" so a List sees the document it just wrote.
	Refresh string
	// MaxRetries bounds Update's retries after a version conflict. Default 8.
	MaxRetries int
	// OnConflict is called once per version conflict (metrics hook, Step 14).
	OnConflict func(index string)
}

type metaer interface{ MetaPtr() *domain.Meta }

// Repo is the OpenSearch implementation of store.Repo for one index alias.
//
// State indices are read by ID with the real-time GET API. Time-series aliases
// can span several rolled-over indices, so they are read through an ids search
// (visible after the next refresh) and cannot be deleted by ID.
type Repo[T any] struct {
	c     *Client
	alias string
	kind  schema.Kind
	opts  Options
	meta  func(*T) *domain.Meta
}

var _ store.Repo[domain.Case] = (*Repo[domain.Case])(nil)

// NewRepo builds a repository. T must embed domain.Meta.
func NewRepo[T any, P interface {
	*T
	metaer
}](c *Client, alias string, kind schema.Kind, opts Options) *Repo[T] {
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 8
	}
	return &Repo[T]{c: c, alias: alias, kind: kind, opts: opts, meta: func(t *T) *domain.Meta { return P(t).MetaPtr() }}
}

type writeResp struct {
	Index       string `json:"_index"`
	SeqNo       int64  `json:"_seq_no"`
	PrimaryTerm int64  `json:"_primary_term"`
}

func (w writeResp) version() store.Version {
	return store.Version{Index: w.Index, SeqNo: w.SeqNo, PrimaryTerm: w.PrimaryTerm}
}

func (r *Repo[T]) query(extra url.Values) string {
	if extra == nil {
		extra = url.Values{}
	}
	if r.opts.Refresh != "" {
		extra.Set("refresh", r.opts.Refresh)
	}
	if len(extra) == 0 {
		return ""
	}
	return "?" + extra.Encode()
}

// prepare validates the document and stamps the common fields.
func (r *Repo[T]) prepare(doc *T, isNew bool) (string, error) {
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

func (r *Repo[T]) conflict() {
	if r.opts.OnConflict != nil {
		r.opts.OnConflict(r.alias)
	}
}

func (r *Repo[T]) Get(ctx context.Context, id string) (T, store.Version, error) {
	var zero T
	if id == "" {
		return zero, store.Version{}, fmt.Errorf("%w: empty id", store.ErrInvalid)
	}
	if r.kind == schema.TimeSeries {
		body := map[string]any{"size": 1, "seq_no_primary_term": true, "query": map[string]any{"ids": map[string]any{"values": []string{id}}}}
		_, out, err := r.c.Do(ctx, http.MethodPost, "/"+r.alias+"/_search", body)
		if err != nil {
			return zero, store.Version{}, err
		}
		var sr struct {
			Hits struct {
				Hits []struct {
					Index       string          `json:"_index"`
					SeqNo       int64           `json:"_seq_no"`
					PrimaryTerm int64           `json:"_primary_term"`
					Source      json.RawMessage `json:"_source"`
				} `json:"hits"`
			} `json:"hits"`
		}
		if err := json.Unmarshal(out, &sr); err != nil {
			return zero, store.Version{}, fmt.Errorf("opensearch: decode search: %w", err)
		}
		if len(sr.Hits.Hits) == 0 {
			return zero, store.Version{}, store.ErrNotFound
		}
		h := sr.Hits.Hits[0]
		var doc T
		if err := json.Unmarshal(h.Source, &doc); err != nil {
			return zero, store.Version{}, fmt.Errorf("opensearch: decode %s/%s: %w", r.alias, id, err)
		}
		return doc, store.Version{Index: h.Index, SeqNo: h.SeqNo, PrimaryTerm: h.PrimaryTerm}, nil
	}

	status, out, err := r.c.Do(ctx, http.MethodGet, "/"+r.alias+"/_doc/"+url.PathEscape(id), nil, http.StatusNotFound)
	if err != nil {
		return zero, store.Version{}, err
	}
	var gr struct {
		writeResp
		Found  bool            `json:"found"`
		Source json.RawMessage `json:"_source"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(out, &gr); err != nil {
		return zero, store.Version{}, fmt.Errorf("opensearch: decode get: %w", err)
	}
	if status == http.StatusNotFound {
		if len(gr.Error) > 0 { // 404 because the index itself is missing: a real problem, not "no such document"
			return zero, store.Version{}, &APIError{Status: status, Method: http.MethodGet, Path: "/" + r.alias + "/_doc/" + id, Body: string(out)}
		}
		return zero, store.Version{}, store.ErrNotFound
	}
	var doc T
	if err := json.Unmarshal(gr.Source, &doc); err != nil {
		return zero, store.Version{}, fmt.Errorf("opensearch: decode %s/%s: %w", r.alias, id, err)
	}
	return doc, gr.version(), nil
}

func (r *Repo[T]) Create(ctx context.Context, doc T) (store.Version, error) {
	id, err := r.prepare(&doc, true)
	if err != nil {
		return store.Version{}, err
	}
	status, out, err := r.c.Do(ctx, http.MethodPut, "/"+r.alias+"/_create/"+url.PathEscape(id)+r.query(nil), doc, http.StatusConflict)
	if err != nil {
		return store.Version{}, err
	}
	if status == http.StatusConflict {
		return store.Version{}, store.ErrAlreadyExists
	}
	return decodeWrite(out)
}

func (r *Repo[T]) Put(ctx context.Context, doc T) (store.Version, error) {
	id, err := r.prepare(&doc, true)
	if err != nil {
		return store.Version{}, err
	}
	_, out, err := r.c.Do(ctx, http.MethodPut, "/"+r.alias+"/_doc/"+url.PathEscape(id)+r.query(nil), doc)
	if err != nil {
		return store.Version{}, err
	}
	return decodeWrite(out)
}

func (r *Repo[T]) Save(ctx context.Context, doc T, v store.Version) (store.Version, error) {
	if v.IsZero() {
		return store.Version{}, fmt.Errorf("%w: Save needs the version returned by Get", store.ErrInvalid)
	}
	id, err := r.prepare(&doc, false)
	if err != nil {
		return store.Version{}, err
	}
	target := r.alias
	if v.Index != "" {
		target = v.Index
	}
	q := url.Values{"if_seq_no": {strconv.FormatInt(v.SeqNo, 10)}, "if_primary_term": {strconv.FormatInt(v.PrimaryTerm, 10)}}
	status, out, err := r.c.Do(ctx, http.MethodPut, "/"+target+"/_doc/"+url.PathEscape(id)+r.query(q), doc, http.StatusConflict)
	if err != nil {
		return store.Version{}, err
	}
	if status == http.StatusConflict {
		r.conflict()
		return store.Version{}, store.ErrVersionConflict
	}
	return decodeWrite(out)
}

func (r *Repo[T]) Update(ctx context.Context, id string, mutate func(*T) error) (T, store.Version, error) {
	var zero T
	for attempt := 0; attempt <= r.opts.MaxRetries; attempt++ {
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
		// Someone else won the race. Back off with jitter so the losers spread out.
		wait := time.Duration(rand.IntN(5<<min(attempt, 4))+1) * time.Millisecond
		select {
		case <-ctx.Done():
			return zero, store.Version{}, ctx.Err()
		case <-time.After(wait):
		}
	}
	return zero, store.Version{}, store.ErrConflictExhausted
}

func (r *Repo[T]) Delete(ctx context.Context, id string) error {
	if r.kind == schema.TimeSeries {
		return store.ErrUnsupported // time-series data leaves through ISM retention
	}
	status, _, err := r.c.Do(ctx, http.MethodDelete, "/"+r.alias+"/_doc/"+url.PathEscape(id)+r.query(nil), nil, http.StatusNotFound)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return store.ErrNotFound
	}
	return nil
}

const (
	defaultLimit = 50
	maxLimit     = 200
)

func (r *Repo[T]) List(ctx context.Context, q store.ListQuery) (store.Page[T], error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	sortField := q.Sort
	if sortField == "" {
		sortField = "created_at"
	}
	order := "asc"
	if q.Desc {
		order = "desc"
	}

	var filter []any
	for field, vals := range q.Terms {
		filter = append(filter, map[string]any{"terms": map[string]any{field: vals}})
	}
	for _, rg := range q.Ranges {
		b := map[string]any{}
		if rg.Gte != nil {
			b["gte"] = rg.Gte
		}
		if rg.Lte != nil {
			b["lte"] = rg.Lte
		}
		filter = append(filter, map[string]any{"range": map[string]any{rg.Field: b}})
	}
	query := map[string]any{"match_all": map[string]any{}}
	if len(filter) > 0 {
		query = map[string]any{"bool": map[string]any{"filter": filter}}
	}
	body := map[string]any{
		"size":             limit + 1, // one extra row tells us whether another page exists
		"track_total_hits": false,
		"query":            query,
		"sort": []any{
			map[string]any{sortField: map[string]any{"order": order, "missing": "_last"}},
			map[string]any{"id": map[string]any{"order": order}},
		},
	}
	if q.After != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.After)
		var after []json.RawMessage // raw, so huge "missing value" sentinels keep full precision
		if err != nil || json.Unmarshal(raw, &after) != nil || len(after) != 2 {
			return store.Page[T]{}, fmt.Errorf("%w: bad cursor", store.ErrInvalid)
		}
		body["search_after"] = after
	}

	_, out, err := r.c.Do(ctx, http.MethodPost, "/"+r.alias+"/_search", body)
	if err != nil {
		return store.Page[T]{}, err
	}
	var sr struct {
		Hits struct {
			Hits []struct {
				Source json.RawMessage   `json:"_source"`
				Sort   []json.RawMessage `json:"sort"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(out, &sr); err != nil {
		return store.Page[T]{}, fmt.Errorf("opensearch: decode search: %w", err)
	}
	hits := sr.Hits.Hits
	page := store.Page[T]{}
	if len(hits) > limit {
		hits = hits[:limit]
		raw, _ := json.Marshal(hits[limit-1].Sort)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	page.Items = make([]T, 0, len(hits))
	for _, h := range hits {
		var d T
		if err := json.Unmarshal(h.Source, &d); err != nil {
			return store.Page[T]{}, fmt.Errorf("opensearch: decode hit: %w", err)
		}
		page.Items = append(page.Items, d)
	}
	return page, nil
}

func decodeWrite(out []byte) (store.Version, error) {
	var w writeResp
	if err := json.Unmarshal(out, &w); err != nil {
		return store.Version{}, fmt.Errorf("opensearch: decode write response: %w", err)
	}
	return w.version(), nil
}
