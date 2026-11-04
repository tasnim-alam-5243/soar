package store

import "context"

// BulkAction is the operation of one bulk item.
type BulkAction string

const (
	BulkCreate BulkAction = "create" // fails with 409 if the id exists
	BulkIndex  BulkAction = "index"  // create or replace (honours Version)
	BulkUpdate BulkAction = "update" // partial update: Body holds doc/script/upsert
	BulkDelete BulkAction = "delete"
)

// BulkOp is one item of a bulk write.
type BulkOp struct {
	Action  BulkAction
	Index   string         // alias or concrete index
	ID      string         // required for update and delete; create/index generate one if empty
	Doc     any            // create, index: the document
	Body    map[string]any // update: {"doc":...,"doc_as_upsert":true} or {"script":...,"upsert":...}
	Version *Version       // optional optimistic-concurrency check (index, update, delete)
	Retries int            // update only: retry_on_conflict
}

// BulkResult is the outcome of one BulkOp, in the same order as the request.
type BulkResult struct {
	Status  int // HTTP status of the item, for example 201, 200, 409, 429
	ID      string
	Version Version
	Err     *BulkError
}

// BulkError is an item-level failure.
type BulkError struct {
	Type   string
	Reason string
}

func (e *BulkError) Error() string { return e.Type + ": " + e.Reason }

// OK reports whether the item was written.
func (r BulkResult) OK() bool { return r.Err == nil && r.Status < 300 }

// Conflict reports a version or already-exists conflict (HTTP 409).
func (r BulkResult) Conflict() bool { return r.Status == 409 }

// Retryable reports a transient failure worth retrying later (429, 5xx).
func (r BulkResult) Retryable() bool { return r.Status == 429 || r.Status >= 500 }

// Permanent reports a failure that retrying cannot fix (bad document, strict mapping).
func (r BulkResult) Permanent() bool { return r.Err != nil && !r.Conflict() && !r.Retryable() }

// BulkWriter writes many documents per request. Results line up with ops.
// An error return means the request as a whole failed and nothing can be assumed
// about individual items, so callers retry the whole batch (writes are idempotent).
type BulkWriter interface {
	Write(ctx context.Context, ops []BulkOp) ([]BulkResult, error)
}
