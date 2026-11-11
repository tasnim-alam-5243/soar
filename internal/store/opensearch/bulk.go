package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/store"
)

// BulkConfig bounds one _bulk request.
type BulkConfig struct {
	MaxDocs  int // default 1000 (spec: 500-1,000 docs per bulk)
	MaxBytes int // default 5 MiB
	Refresh  string
}

// Bulk implements store.BulkWriter on top of POST /_bulk.
type Bulk struct {
	c   *Client
	cfg BulkConfig
}

var _ store.BulkWriter = (*Bulk)(nil)

// NewBulk builds a bulk writer.
func NewBulk(c *Client, cfg BulkConfig) *Bulk {
	if cfg.MaxDocs <= 0 {
		cfg.MaxDocs = 1000
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 5 << 20
	}
	return &Bulk{c: c, cfg: cfg}
}

// Write sends ops in as few requests as the limits allow and returns one result per op.
func (b *Bulk) Write(ctx context.Context, ops []store.BulkOp) ([]store.BulkResult, error) {
	results := make([]store.BulkResult, 0, len(ops))
	var buf bytes.Buffer
	docs := 0
	flush := func() error {
		if docs == 0 {
			return nil
		}
		res, err := b.send(ctx, buf.Bytes(), docs)
		if err != nil {
			return err
		}
		results = append(results, res...)
		buf.Reset()
		docs = 0
		return nil
	}
	for i, op := range ops {
		line, err := encodeOp(op)
		if err != nil {
			return nil, fmt.Errorf("bulk op %d: %w", i, err)
		}
		if docs > 0 && (docs >= b.cfg.MaxDocs || buf.Len()+len(line) > b.cfg.MaxBytes) {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		buf.Write(line)
		docs++
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return results, nil
}

func encodeOp(op store.BulkOp) ([]byte, error) {
	if op.Index == "" {
		return nil, fmt.Errorf("%w: bulk op without index", store.ErrInvalid)
	}
	meta := map[string]any{"_index": op.Index}
	if op.ID != "" {
		meta["_id"] = op.ID
	}
	switch op.Action {
	case store.BulkCreate, store.BulkIndex, store.BulkUpdate, store.BulkDelete:
	default:
		return nil, fmt.Errorf("%w: unknown bulk action %q", store.ErrInvalid, op.Action)
	}
	if (op.Action == store.BulkUpdate || op.Action == store.BulkDelete) && op.ID == "" {
		return nil, fmt.Errorf("%w: %s needs an id", store.ErrInvalid, op.Action)
	}
	if op.Version != nil && op.Action != store.BulkCreate {
		meta["if_seq_no"] = op.Version.SeqNo
		meta["if_primary_term"] = op.Version.PrimaryTerm
	}
	if op.Action == store.BulkUpdate && op.Retries > 0 && op.Version == nil {
		meta["retry_on_conflict"] = op.Retries
	}
	var out bytes.Buffer
	head, err := json.Marshal(map[string]any{string(op.Action): meta})
	if err != nil {
		return nil, err
	}
	out.Write(head)
	out.WriteByte('\n')
	switch op.Action {
	case store.BulkDelete:
	case store.BulkUpdate:
		body, err := json.Marshal(op.Body)
		if err != nil {
			return nil, err
		}
		out.Write(body)
		out.WriteByte('\n')
	default:
		body, err := json.Marshal(op.Doc)
		if err != nil {
			return nil, err
		}
		out.Write(body)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

type bulkItem struct {
	Index       string `json:"_index"`
	ID          string `json:"_id"`
	Status      int    `json:"status"`
	SeqNo       int64  `json:"_seq_no"`
	PrimaryTerm int64  `json:"_primary_term"`
	Error       *struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	} `json:"error"`
}

func (b *Bulk) send(ctx context.Context, body []byte, want int) ([]store.BulkResult, error) {
	path := "/_bulk"
	if b.cfg.Refresh != "" {
		path += "?refresh=" + b.cfg.Refresh
	}
	_, out, err := b.c.DoRaw(ctx, http.MethodPost, path, "application/x-ndjson", body)
	if err != nil {
		return nil, err // whole request failed (429, 5xx, network): caller retries the batch
	}
	var resp struct {
		Items []map[string]bulkItem `json:"items"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("opensearch: decode bulk response: %w", err)
	}
	if len(resp.Items) != want {
		return nil, fmt.Errorf("opensearch: bulk returned %d items for %d ops", len(resp.Items), want)
	}
	res := make([]store.BulkResult, 0, want)
	for _, wrapper := range resp.Items {
		for _, it := range wrapper { // exactly one entry: the action name
			r := store.BulkResult{
				Status: it.Status, ID: it.ID,
				Version: store.Version{Index: it.Index, SeqNo: it.SeqNo, PrimaryTerm: it.PrimaryTerm},
			}
			if it.Error != nil {
				r.Err = &store.BulkError{Type: it.Error.Type, Reason: it.Error.Reason}
			}
			res = append(res, r)
		}
	}
	return res, nil
}

// GroupConfig controls GroupWriter batching.
type GroupConfig struct {
	MaxDocs    int           // flush when this many ops are waiting (default 1000)
	FlushEvery time.Duration // or this long after the first waiting op (default 250 ms)
	Timeout    time.Duration // per bulk request (default 30 s)
}

// GroupWriter batches writes from many goroutines into shared bulk requests
// ("group commit"). Write returns only after the ops are durable in OpenSearch,
// so callers can ack their queue message right after it.
type GroupWriter struct {
	under store.BulkWriter
	cfg   GroupConfig
	in    chan *groupReq
	done  chan struct{}
	once  sync.Once
	stop  chan struct{}
}

type groupReq struct {
	ops []store.BulkOp
	res chan groupRes
}

type groupRes struct {
	results []store.BulkResult
	err     error
}

var _ store.BulkWriter = (*GroupWriter)(nil)

// NewGroupWriter starts the batching goroutine. Call Close when done.
func NewGroupWriter(under store.BulkWriter, cfg GroupConfig) *GroupWriter {
	if cfg.MaxDocs <= 0 {
		cfg.MaxDocs = 1000
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 250 * time.Millisecond
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	g := &GroupWriter{under: under, cfg: cfg, in: make(chan *groupReq), done: make(chan struct{}), stop: make(chan struct{})}
	go g.loop()
	return g
}

// ErrClosed is returned by Write after Close.
var ErrClosed = errors.New("opensearch: group writer closed")

// Write enqueues ops and waits for their results. If ctx ends after the ops were
// enqueued they may still be written; callers must tolerate that (idempotent writes).
func (g *GroupWriter) Write(ctx context.Context, ops []store.BulkOp) ([]store.BulkResult, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	req := &groupReq{ops: ops, res: make(chan groupRes, 1)}
	select {
	case g.in <- req:
	case <-g.stop:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-req.res:
		return r.results, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close flushes what is waiting and stops the goroutine.
func (g *GroupWriter) Close() {
	g.once.Do(func() { close(g.stop) })
	<-g.done
}

func (g *GroupWriter) loop() {
	defer close(g.done)
	for {
		var batch []*groupReq
		select {
		case first := <-g.in:
			batch = []*groupReq{first}
		case <-g.stop:
			return
		}
		n := len(batch[0].ops)
		timer := time.NewTimer(g.cfg.FlushEvery)
	collect:
		for n < g.cfg.MaxDocs {
			select {
			case r := <-g.in:
				batch = append(batch, r)
				n += len(r.ops)
			case <-timer.C:
				break collect
			case <-g.stop:
				break collect
			}
		}
		timer.Stop()
		g.flush(batch)
	}
}

func (g *GroupWriter) flush(batch []*groupReq) {
	var all []store.BulkOp
	for _, r := range batch {
		all = append(all, r.ops...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.cfg.Timeout)
	defer cancel()
	results, err := g.under.Write(ctx, all)
	if err == nil && len(results) != len(all) {
		err = fmt.Errorf("opensearch: %d results for %d ops", len(results), len(all))
	}
	off := 0
	for _, r := range batch {
		if err != nil {
			r.res <- groupRes{err: err}
			continue
		}
		r.res <- groupRes{results: results[off : off+len(r.ops)]}
		off += len(r.ops)
	}
}
