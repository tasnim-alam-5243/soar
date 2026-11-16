// Package store defines how the rest of the platform reads and writes documents.
// Services depend on these interfaces only; internal/store/opensearch is the real
// implementation (the only code that talks to OpenSearch) and internal/store/memory
// is a fast in-process one for unit tests. Both must pass storetest.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yourorg/soar/internal/domain"
)

// Errors returned by every implementation. Use errors.Is.
var (
	ErrNotFound          = errors.New("store: not found")
	ErrAlreadyExists     = errors.New("store: already exists")
	ErrVersionConflict   = errors.New("store: version conflict (document changed since it was read)")
	ErrConflictExhausted = errors.New("store: too many concurrent updates, gave up")
	ErrInvalid           = errors.New("store: invalid argument")
	ErrUnsupported       = errors.New("store: operation not supported for this index kind")
)

// Version identifies one stored revision of a document. Passing it back to Save
// makes the write fail with ErrVersionConflict if anyone else wrote in between
// (OpenSearch if_seq_no / if_primary_term). Index is the concrete index the
// document lives in; it matters for time-series aliases that span several indices.
type Version struct {
	Index       string
	SeqNo       int64
	PrimaryTerm int64
}

// IsZero reports whether v carries no concurrency information.
func (v Version) IsZero() bool { return v.SeqNo == 0 && v.PrimaryTerm == 0 }

// ETag renders the version for HTTP ETag / If-Match headers.
func (v Version) ETag() string { return fmt.Sprintf(`"%d-%d"`, v.SeqNo, v.PrimaryTerm) }

// ParseETag is the inverse of ETag.
func ParseETag(s string) (Version, bool) {
	var v Version
	if n, err := fmt.Sscanf(s, `"%d-%d"`, &v.SeqNo, &v.PrimaryTerm); err != nil || n != 2 || v.PrimaryTerm < 1 || v.SeqNo < 0 {
		return Version{}, false
	}
	return v, true
}

// Range is an inclusive range filter on a date or number field. Nil bounds are open.
type Range struct {
	Field string
	Gte   any
	Lte   any
}

// ListQuery is a small, implementation-neutral query: exact-match filters,
// ranges, one sort key (ties broken by id) and cursor pagination.
type ListQuery struct {
	Terms  map[string][]string // field -> any of these values; all fields must match
	Ranges []Range
	Sort   string // field to sort by; default created_at
	Desc   bool
	Limit  int    // default 50, max 200
	After  string // cursor from a previous Page.NextCursor
	// OrMissing names Terms fields that also match documents without the field, for
	// documents written before the field existed (alerts without a status count as new).
	OrMissing []string
	// Search is free text that must match at least one of SearchFields (all of its words,
	// case-insensitive). Empty: no text filter.
	Search       string
	SearchFields []string
}

// MissingOK reports whether field is listed in OrMissing.
func (q ListQuery) MissingOK(field string) bool {
	for _, f := range q.OrMissing {
		if f == field {
			return true
		}
	}
	return false
}

// Page is one page of results. NextCursor is empty on the last page.
type Page[T any] struct {
	Items      []T
	NextCursor string
}

// Repo stores one kind of document.
type Repo[T any] interface {
	// Get returns the document and its version, or ErrNotFound.
	Get(ctx context.Context, id string) (T, Version, error)
	// Create stores a new document; ErrAlreadyExists if the id is taken.
	Create(ctx context.Context, doc T) (Version, error)
	// Put stores the document, replacing any existing one without a version check.
	// Use it for configuration data that has a single writer.
	Put(ctx context.Context, doc T) (Version, error)
	// Save replaces the document only if it is still at version v (ErrVersionConflict otherwise).
	Save(ctx context.Context, doc T, v Version) (Version, error)
	// Update is read-modify-write with bounded retries on conflict. mutate may run
	// several times, so it must be a pure function of its argument.
	Update(ctx context.Context, id string, mutate func(*T) error) (T, Version, error)
	// Delete removes a document; ErrNotFound if it does not exist.
	Delete(ctx context.Context, id string) error
	// List returns one page of documents.
	List(ctx context.Context, q ListQuery) (Page[T], error)
}

// Names turns entity names into index aliases ("alerts" -> "soar-alerts").
type Names struct{ Prefix string }

// Index returns the alias for an entity.
func (n Names) Index(entity string) string { return n.Prefix + "-" + entity }

// Store holds one repository per index in spec section 5.
type Store struct {
	Names Names

	Workflows     Repo[domain.Workflow]
	Executions    Repo[domain.Execution]
	StepRuns      Repo[domain.StepRun]
	Alerts        Repo[domain.Alert]
	Cases         Repo[domain.Case]
	CaseTasks     Repo[domain.CaseTask]
	CaseAlerts    Repo[domain.CaseAlert]
	Observables   Repo[domain.ObservableDoc]
	Timeline      Repo[domain.TimelineEvent]
	Audit         Repo[domain.AuditRecord]
	Secrets       Repo[domain.Secret]
	Principals    Repo[domain.Principal]
	Idempotency   Repo[domain.Idempotency]
	Timers        Repo[domain.Timer]
	Approvals     Repo[domain.Approval]
	Subscriptions Repo[domain.Subscription]
	Sources       Repo[domain.Source]
	Mappings      Repo[domain.Mapping]
	Filters       Repo[domain.FilterSet]
	Lists         Repo[domain.List]
	Cursors       Repo[domain.Cursor]
	Raw           Repo[domain.RawRecord]
	Seen          Repo[domain.Seen]
	Connections   Repo[domain.Connection]
}

// Counter is implemented by repositories that can count matching documents without
// fetching them (dashboards). Both the OpenSearch and the memory store implement it.
type Counter interface {
	Count(ctx context.Context, q ListQuery) (int64, error)
}

// Count counts documents of any repository that implements Counter.
func Count[T any](ctx context.Context, r Repo[T], q ListQuery) (int64, error) {
	c, ok := r.(Counter)
	if !ok {
		return 0, ErrUnsupported
	}
	return c.Count(ctx, q)
}

// TimeBucket is one interval of a date histogram. Counts holds the count per value of
// the split field, or under the key "" when no split field was asked for.
type TimeBucket struct {
	Start  time.Time
	Counts map[string]int64
}

// Term is one value of a field and the number of matching documents that carry it.
type Term struct {
	Value string
	Count int64
}

// Aggregator is implemented by repositories that can summarise matching documents
// (dashboards). Both the OpenSearch and the memory store implement it.
type Aggregator interface {
	// Histogram counts the documents matching q per fixed interval of the date field
	// field, from the interval holding from to the one holding to (empty intervals
	// included), split by the values of split ("" = no split). Documents without a split
	// value are not counted when a split is asked for.
	Histogram(ctx context.Context, q ListQuery, field string, from, to time.Time, interval time.Duration, split string) ([]TimeBucket, error)
	// TopTerms returns the size most frequent values of field among the documents
	// matching q, most frequent first (ties: by value).
	TopTerms(ctx context.Context, q ListQuery, field string, size int) ([]Term, error)
}

// Histogram runs Aggregator.Histogram on any repository that implements it.
func Histogram[T any](ctx context.Context, r Repo[T], q ListQuery, field string, from, to time.Time, interval time.Duration, split string) ([]TimeBucket, error) {
	a, ok := r.(Aggregator)
	if !ok {
		return nil, ErrUnsupported
	}
	if interval <= 0 || !to.After(from) {
		return nil, fmt.Errorf("%w: histogram needs a positive interval and from before to", ErrInvalid)
	}
	return a.Histogram(ctx, q, field, from, to, interval, split)
}

// TopTerms runs Aggregator.TopTerms on any repository that implements it.
func TopTerms[T any](ctx context.Context, r Repo[T], q ListQuery, field string, size int) ([]Term, error) {
	a, ok := r.(Aggregator)
	if !ok {
		return nil, ErrUnsupported
	}
	if size <= 0 || size > 100 {
		size = 10
	}
	return a.TopTerms(ctx, q, field, size)
}

// BucketStart returns the start of the fixed interval holding t. Intervals are aligned
// to the Unix epoch, as OpenSearch's fixed_interval date histogram aligns them.
func BucketStart(t time.Time, interval time.Duration) time.Time {
	ms := interval.Milliseconds()
	if ms <= 0 {
		return t.UTC()
	}
	return time.UnixMilli(t.UnixMilli() - ((t.UnixMilli()%ms)+ms)%ms).UTC()
}
