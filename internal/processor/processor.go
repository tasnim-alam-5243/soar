// Package processor consumes events from NATS and writes them to OpenSearch (spec 9.9):
// exact-duplicate guard, alert upserts that group repeats, observables, workflow
// triggers. A message is acknowledged only after its writes succeeded.
package processor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/orchestrator"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/store"
)

// Starter publishes start messages of new executions.
type Starter interface {
	PublishMany(ctx context.Context, ms []queue.Msg) []error
}

// Notifier publishes best-effort UI events (core NATS ui.<topic>, relayed by the API to /stream).
type Notifier interface {
	Notify(subject string, v any)
}

// maxAlertEvents caps the per-alert UI events of one batch; a flood of new alerts is
// announced with one summary event instead, so the stream stays cheap during bursts.
const maxAlertEvents = 20

// notifyCreated announces the alerts a batch created. Severity and title ride along so
// the UI can raise a toast for critical and high alerts without fetching them.
func (p *Processor) notifyCreated(created []*item) {
	if p.UI == nil || len(created) == 0 {
		return
	}
	now := time.Now().UTC()
	if len(created) > maxAlertEvents {
		p.UI.Notify("ui.alerts", map[string]any{"topic": "alerts", "id": "", "data": map[string]any{"created": len(created)}, "at": now})
		return
	}
	for _, it := range created {
		a := it.ev.Alert
		p.UI.Notify("ui.alerts", map[string]any{"topic": "alerts", "id": it.alertID, "at": now, "data": map[string]any{
			"created": 1, "severity": a.Severity, "title": a.Title, "source_id": a.SourceID,
		}})
	}
}

// DLQ is the part of the queue client the processor needs.
type DLQ interface {
	SendToDLQ(ctx context.Context, origin, reason, subject string, data []byte, id string, extra map[string]string) error
}

// Config tunes the processor.
type Config struct {
	Batch      int           // messages per fetch (spec: 256)
	FetchWait  time.Duration // how long a fetch waits for a full batch
	Workers    int           // concurrent fetch loops
	Partitions int           // orchestrator partitions (SOAR_PARTITIONS)
	// NakDelay is the first redelivery delay of a transiently failed message; later
	// deliveries wait 5x, 30x and 120x as long (spec 8.3: 1 s, 5 s, 30 s, 2 min), so a
	// failure that outlasts the local retries cannot use up all 5 deliveries in seconds.
	NakDelay   time.Duration
	LocalRetry int // retries of retryable bulk items (other than 429/502-504) before nak-ing
	// While OpenSearch is unavailable the batch is held: HoldMax caps the pause between
	// write attempts and Heartbeat is how often the held messages get InProgress (it must
	// stay well under the consumer's 60 s AckWait).
	HoldMax   time.Duration
	Heartbeat time.Duration
}

func (c *Config) defaults() {
	if c.Batch <= 0 {
		c.Batch = 256
	}
	if c.FetchWait <= 0 {
		c.FetchWait = 500 * time.Millisecond
	}
	if c.Workers <= 0 {
		c.Workers = 2
	}
	if c.Partitions <= 0 {
		c.Partitions = 8
	}
	if c.NakDelay <= 0 {
		c.NakDelay = time.Second
	}
	if c.LocalRetry <= 0 {
		c.LocalRetry = 3
	}
	if c.HoldMax <= 0 {
		c.HoldMax = 15 * time.Second
	}
	if c.Heartbeat <= 0 {
		c.Heartbeat = 15 * time.Second
	}
}

// Counters are exported for health output and tests.
type Counters struct {
	Processed, Duplicates, DeadLettered, Retried, Executions atomic.Int64
	WaitsResumed                                             atomic.Int64 // waiting executions an event resumed
	Held                                                     atomic.Int64 // batches held while OpenSearch was unavailable
}

// Processor turns queued events into alerts, observables and executions.
type Processor struct {
	Cons     jetstream.Consumer
	Bulk     store.BulkWriter
	St       *store.Store
	DLQ      DLQ
	Triggers *TriggerIndex
	Waits    *WaitIndex // open event waits (nil: none are matched)
	Starts   Starter    // publishes execution start messages (nil: the sweeper starts them)
	UI       Notifier   // tells connected UIs about new alerts (nil: no live updates)
	Log      *slog.Logger
	Cfg      Config
	Stats    Counters

	// outage is set while a batch is held because OpenSearch is unavailable; idle fetch
	// loops wait instead of taking more messages (spec 8.7: the processor pauses and the
	// queue absorbs the backlog).
	outage atomic.Bool
}

type item struct {
	msg      jetstream.Msg
	ev       domain.Event
	alertID  string
	seenID   string
	retryErr error // transient failure: nak
	dup      bool  // exact duplicate: nothing to write, just acknowledge
	applied  bool  // own redelivery whose alert increment is already written: repeat only the idempotent writes
	done     bool  // already acked / termed / nak-ed
}

// Run fetches and processes batches until ctx ends.
func (p *Processor) Run(ctx context.Context) error {
	p.Cfg.defaults()
	var wg sync.WaitGroup
	for i := 0; i < p.Cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if p.outage.Load() {
					sleep(ctx, 250*time.Millisecond) // paused: another loop is holding a batch until OpenSearch is back
					continue
				}
				batch, err := p.Cons.Fetch(p.Cfg.Batch, jetstream.FetchMaxWait(p.Cfg.FetchWait))
				if err != nil {
					if ctx.Err() == nil {
						p.Log.Warn("fetch failed", "error", err.Error())
						sleep(ctx, time.Second)
					}
					continue
				}
				var msgs []jetstream.Msg
				for m := range batch.Messages() {
					msgs = append(msgs, m)
				}
				if len(msgs) > 0 {
					p.HandleBatch(ctx, msgs)
				}
			}
		}()
	}
	wg.Wait()
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (p *Processor) deadLetter(ctx context.Context, it *item, reason string) {
	if it.done {
		return
	}
	id := "proc-" + it.msg.Subject()
	if md, err := it.msg.Metadata(); err == nil {
		id += "-" + strconv.FormatUint(md.Sequence.Stream, 10)
	}
	if err := p.DLQ.SendToDLQ(ctx, "events", reason, it.msg.Subject(), it.msg.Data(), id, nil); err != nil {
		// Without a durable copy we must not discard the message: let it be redelivered.
		p.Log.Error("could not dead-letter; message will be redelivered", "reason", reason, "error", err.Error())
		_ = it.msg.NakWithDelay(p.nakDelay(it))
		it.done = true
		return
	}
	_ = it.msg.Term() // a terminated work-queue message is removed
	it.done = true
	p.Stats.DeadLettered.Add(1)
}

// nakBackoff multiplies NakDelay per delivery: 1 s, 5 s, 30 s, 2 min with the default (spec 8.3).
var nakBackoff = []time.Duration{1, 5, 30, 120}

// nakDelay grows with the message's delivery count.
func (p *Processor) nakDelay(it *item) time.Duration {
	n := 1
	if md, err := it.msg.Metadata(); err == nil && md.NumDelivered > 0 {
		n = int(md.NumDelivered)
	}
	return p.Cfg.NakDelay * nakBackoff[min(n, len(nakBackoff))-1]
}

func (p *Processor) nak(it *item) {
	if it.done {
		return
	}
	_ = it.msg.NakWithDelay(p.nakDelay(it))
	it.done = true
	p.Stats.Retried.Add(1)
}

func (p *Processor) ack(ctx context.Context, it *item) {
	if it.done {
		return
	}
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := it.msg.DoubleAck(actx); err != nil {
		p.Log.Warn("ack was not confirmed; the message may be redelivered (writes are idempotent)", "error", err.Error())
	}
	it.done = true
}

// bulkWithRetry writes ops and returns the final result of every op.
//
// OpenSearch being unavailable (unreachable, timing out, or answering 429/502/503/504
// for the request or for items) says nothing about these messages, so the batch is
// held: the write is retried with backoff for as long as the outage lasts while the
// in-flight messages get InProgress, so JetStream neither redelivers them nor counts a
// delivery, and the other fetch loops pause (spec 8.7). Before this, every failure was
// nak-ed with a short delay and a ~10 s outage used up the 5 deliveries: the events
// went to the DLQ. Other transient item failures (500) get LocalRetry attempts; any
// other whole-request error is returned as err.
func (p *Processor) bulkWithRetry(ctx context.Context, ops []store.BulkOp, inflight []*item) ([]store.BulkResult, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	final := make([]store.BulkResult, len(ops))
	pending := make([]int, len(ops))
	for i := range pending {
		pending[i] = i
	}
	hold := p.holder(inflight)
	defer hold.release()
	for local := 0; len(pending) > 0; {
		batch := make([]store.BulkOp, len(pending))
		for i, idx := range pending {
			batch[i] = ops[idx]
		}
		res, err := p.Bulk.Write(ctx, batch)
		if err != nil {
			if store.Unavailable(err) && ctx.Err() == nil {
				if werr := hold.wait(ctx, err); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, err
		}
		if len(res) != len(batch) {
			return nil, fmt.Errorf("bulk returned %d results for %d operations", len(res), len(batch))
		}
		var next []int
		var rejected error
		for i, idx := range pending {
			final[idx] = res[i]
			switch {
			case store.UnavailableStatus(res[i].Status):
				next = append(next, idx)
				rejected = fmt.Errorf("bulk items rejected with HTTP %d: %s", res[i].Status, errText(res[i].Err))
			case res[i].Retryable() && local < p.Cfg.LocalRetry:
				next = append(next, idx)
			}
		}
		pending = next
		if len(pending) == 0 {
			break
		}
		if rejected != nil && ctx.Err() == nil {
			if werr := hold.wait(ctx, rejected); werr != nil {
				return nil, werr
			}
			continue
		}
		sleep(ctx, time.Duration(200<<local)*time.Millisecond)
		local++
	}
	hold.recovered()
	return final, nil
}

// batchHold is one held batch: it paces the retries, keeps the messages alive and
// maintains the processor-wide pause.
type batchHold struct {
	p        *Processor
	inflight []*item
	w        store.Waiter
	held     bool
	since    time.Time
	stopBeat func()
}

func (p *Processor) holder(inflight []*item) *batchHold {
	return &batchHold{p: p, inflight: inflight, w: store.Waiter{Max: p.Cfg.HoldMax}}
}

func (h *batchHold) wait(ctx context.Context, cause error) error {
	if !h.held {
		h.held, h.since = true, time.Now()
		// Messages settled before the hold (dead letters) are not touched; nothing settles
		// any during it, so reading done here is race-free.
		h.stopBeat = store.Heartbeat(h.p.Cfg.Heartbeat, func() {
			for _, it := range h.inflight {
				if !it.done {
					_ = it.msg.InProgress()
				}
			}
		})
		h.p.Stats.Held.Add(1)
		obs.StoreHolds.WithLabelValues("processor").Inc()
		if h.p.outage.CompareAndSwap(false, true) {
			obs.StoreUnavailable.WithLabelValues("processor").Set(1)
			h.p.Log.Warn("OpenSearch is unavailable: holding in-flight events and pausing fetches until it answers again", "error", cause.Error())
		}
	}
	return h.w.Wait(ctx)
}

// recovered is called after a successful write.
func (h *batchHold) recovered() {
	if h.held && h.p.outage.Load() {
		h.p.Log.Info("OpenSearch answers again: resuming", "held_for", time.Since(h.since).Round(time.Second).String())
	}
}

// release ends the hold whatever the outcome, so an error path never leaves the
// other fetch loops paused; if OpenSearch is still down they pause again at once.
func (h *batchHold) release() {
	if h.stopBeat != nil {
		h.stopBeat()
	}
	if h.held && h.p.outage.CompareAndSwap(true, false) {
		obs.StoreUnavailable.WithLabelValues("processor").Set(0)
	}
}

func partition(id string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % uint32(n))
}

// AlertID is deterministic in (fingerprint, window bucket): repeats of the same alert
// inside the window upsert one document, and a redelivered event hits the same document.
func AlertID(fingerprint string, receivedAt time.Time, windowSec int) string {
	if windowSec <= 0 {
		windowSec = 600
	}
	return domain.DeterministicID(domain.PrefixAlert, fingerprint, receivedAt.Unix()/int64(windowSec))
}

const alertScript = `ctx._source.occurrence_count += 1;
ctx._source.last_seen = params.t;
ctx._source.updated_at = params.now;
if (params.sev > ctx._source.severity) { ctx._source.severity = params.sev; }`

const touchScript = `ctx._source.last_seen = params.t; ctx._source.updated_at = params.now;`

func stamp(id string, now time.Time) domain.Meta {
	return domain.Meta{ID: id, CreatedAt: now, UpdatedAt: now, SchemaVersion: domain.SchemaVersion}
}

// HandleBatch processes one fetched batch. Every message ends acknowledged, dead-lettered
// or scheduled for redelivery; none is left hanging.
func (p *Processor) HandleBatch(ctx context.Context, msgs []jetstream.Msg) {
	p.Cfg.defaults()
	now := time.Now().UTC()
	names := p.St.Names
	items := make([]*item, 0, len(msgs))

	// 1. decode; undecodable messages are poison and go straight to the DLQ
	for _, m := range msgs {
		it := &item{msg: m}
		if err := json.Unmarshal(m.Data(), &it.ev); err != nil || it.ev.Alert.Title == "" || it.ev.Alert.Fingerprint == "" {
			reason := "invalid event"
			if err != nil {
				reason = "invalid event: " + err.Error()
			}
			p.deadLetter(ctx, it, reason)
			continue
		}
		it.alertID = AlertID(it.ev.Alert.Fingerprint, it.ev.ReceivedAt, it.ev.DedupWindowSec)
		items = append(items, it)
	}

	// 2. exact-duplicate guard: create a soar-seen key per external id; a conflict means
	// the same event already arrived (push and pull overlap, or a sender retry)
	var seenOps []store.BulkOp
	var seenFor []*item
	for _, it := range items {
		if it.ev.Alert.ExternalID == "" {
			continue
		}
		key := it.ev.Alert.System + ":" + it.ev.Alert.ExternalID
		it.seenID = domain.DeterministicID("see", key)
		seenOps = append(seenOps, store.BulkOp{
			Action: store.BulkCreate, Index: names.Index("seen"), ID: it.seenID,
			Doc: domain.Seen{Meta: stamp(it.seenID, now), Key: key, EventID: it.ev.EventID, FirstSeen: now},
		})
		seenFor = append(seenFor, it)
	}
	if res, err := p.bulkWithRetry(ctx, seenOps, items); err != nil {
		p.Log.Warn("duplicate guard failed; retrying the whole batch later", "error", err.Error())
		for _, it := range items {
			p.nak(it)
		}
		return
	} else {
		for i, it := range seenFor {
			r := res[i]
			switch {
			case r.OK():
			case r.Conflict():
				own, applied, err := p.isOwnRetry(ctx, it)
				switch {
				case err != nil:
					it.retryErr = err // cannot tell a retry from a duplicate: never guess, never lose the event
				case own:
					// our own earlier attempt created the key; carry on, but if it also wrote
					// the alert increment (the processor died before acknowledging), only the
					// idempotent writes are repeated
					it.applied = applied
				default:
					p.Stats.Duplicates.Add(1)
					it.dup = true // acknowledged with the rest of the batch
				}
			case r.Retryable():
				it.retryErr = r.Err
			default:
				p.deadLetter(ctx, it, "duplicate guard rejected the event: "+errText(r.Err))
			}
		}
	}

	// 3. alerts, observables, executions in one bulk
	type ref struct {
		it   *item
		kind string
		exe  *domain.Execution
		snap map[string]any
	}
	var ops []store.BulkOp
	var refs []ref
	obsSeen := map[string]bool{}
	for _, it := range items {
		if it.done || it.dup || it.retryErr != nil {
			continue
		}
		a := it.ev.Alert
		t := it.ev.ReceivedAt
		if a.LastSeen != nil {
			t = *a.LastSeen
		}
		doc := a
		doc.Meta = stamp(it.alertID, now)
		doc.ReceivedAt = it.ev.ReceivedAt
		doc.LastSeen = &t
		doc.OccurrenceCount = 1
		if doc.Status == "" {
			doc.Status = "new" // the upsert only applies on creation; triage moves it on from there
		}
		if !it.applied {
			ops = append(ops, store.BulkOp{Action: store.BulkUpdate, Index: names.Index("alerts"), ID: it.alertID, Retries: 5, Body: map[string]any{
				"script": map[string]any{
					"lang": "painless", "source": alertScript,
					"params": map[string]any{"t": t.Format(time.RFC3339Nano), "now": now.Format(time.RFC3339Nano), "sev": a.Severity},
				},
				"upsert": doc,
			}})
			refs = append(refs, ref{it: it, kind: "alert"})
			// The increment is the one write that is not idempotent. Its "applied" mark rides
			// in the same bulk request, so OpenSearch completes both even when this process
			// dies right after sending it; a redelivery then skips the increment.
			if it.seenID != "" {
				ops = append(ops, store.BulkOp{
					Action: store.BulkUpdate, Index: names.Index("seen"), ID: it.seenID, Retries: 5,
					Body: map[string]any{"doc": map[string]any{"applied": true}},
				})
				refs = append(refs, ref{it: it, kind: "seen"})
			}
		}

		for _, o := range a.Observables {
			id := domain.ObservableID(o.Type, o.Value)
			if obsSeen[id] {
				continue
			}
			obsSeen[id] = true
			od := domain.ObservableDoc{Meta: stamp(id, now), Type: o.Type, Value: o.Value, FirstSeen: t, LastSeen: t}
			ops = append(ops, store.BulkOp{Action: store.BulkUpdate, Index: names.Index("observables"), ID: id, Retries: 5, Body: map[string]any{
				"script": map[string]any{
					"lang": "painless", "source": touchScript,
					"params": map[string]any{"t": t.Format(time.RFC3339Nano), "now": now.Format(time.RFC3339Nano)},
				},
				"upsert": od,
			}})
			refs = append(refs, ref{it: it, kind: "observable"})
		}

		if p.Triggers != nil {
			for _, m := range p.Triggers.For(it.ev) {
				id := domain.DeterministicID(domain.PrefixExecution, it.alertID, m.WorkflowName)
				snap := toMap(doc)
				queued := &engine.State{Status: engine.StatusQueued, Alert: snap} // lets the sweeper restart it without the message
				ex := domain.Execution{
					Meta: stamp(id, now), WorkflowID: m.WorkflowID, WorkflowName: m.WorkflowName, WorkflowVersion: m.Version,
					WorkflowChecksum: m.Checksum, Status: engine.StatusQueued, Context: queued.JSON(),
					Trigger: domain.ExecutionTrigger{Type: "alert", Kind: a.Kind, AlertID: it.alertID, EventID: it.ev.EventID}, Partition: partition(id, p.Cfg.Partitions),
				}
				ops = append(ops, store.BulkOp{Action: store.BulkCreate, Index: names.Index("executions"), ID: id, Doc: ex})
				refs = append(refs, ref{it: it, kind: "execution", exe: &ex, snap: snap})
			}
		}
	}
	res, err := p.bulkWithRetry(ctx, ops, items)
	if err != nil {
		p.Log.Warn("write failed; retrying the batch later", "error", err.Error())
		for _, it := range items {
			p.nak(it)
		}
		return
	}
	var starts []queue.Msg
	var created []*item        // alerts this batch created
	marked := map[*item]bool{} // "applied" mark written
	incOK := map[*item]bool{}  // alert increment written
	defer func() { p.notifyCreated(created) }()
	for i, r := range res {
		it := refs[i].it
		switch {
		case refs[i].kind == "seen":
			// a failed mark is not a reason to retry the event: its increment is written
			// (or will be retried below); the only risk is one extra count on a redelivery
			if r.OK() {
				marked[it] = true
			} else {
				p.Log.Warn("could not mark the event as applied; a redelivery would count it twice", "event_id", it.ev.EventID, "error", errText(r.Err))
			}
		case r.OK():
			if refs[i].kind == "alert" {
				incOK[it] = true
				if r.Status == http.StatusCreated { // the upsert created the alert (200: a repeat counted)
					created = append(created, it)
				}
			}
			if refs[i].kind == "execution" {
				p.Stats.Executions.Add(1)
				if m, err := orchestrator.StartMessage(*refs[i].exe, orchestrator.StartPayload{Alert: refs[i].snap}); err == nil {
					starts = append(starts, m)
				}
			}
		case refs[i].kind == "execution" && r.Conflict():
			// this alert already started that workflow: exactly what idempotency is for
		case r.Retryable() || r.Conflict():
			if it.retryErr == nil {
				it.retryErr = r.Err
			}
		default:
			p.deadLetter(ctx, it, fmt.Sprintf("%s write rejected: %s", refs[i].kind, errText(r.Err)))
		}
	}

	// An event whose increment failed but whose mark was written must not be skipped on
	// its redelivery: take the mark back first (never lose a count to save a double one).
	var revert []store.BulkOp
	var revertFor []*item
	for it := range marked {
		if !incOK[it] {
			revert = append(revert, store.BulkOp{
				Action: store.BulkUpdate, Index: names.Index("seen"), ID: it.seenID, Retries: 5,
				Body: map[string]any{"doc": map[string]any{"applied": false}},
			})
			revertFor = append(revertFor, it)
		}
	}
	if len(revert) > 0 {
		rres, err := p.bulkWithRetry(ctx, revert, revertFor)
		for i, it := range revertFor {
			if err != nil || !rres[i].OK() {
				// keep the message unacknowledged and unretried: it is redelivered after its
				// ack deadline, by which time the mark may be fixable; never guess
				p.Log.Error("could not take back an applied mark; holding the event for redelivery", "event_id", it.ev.EventID)
				if it.retryErr == nil {
					it.retryErr = fmt.Errorf("applied mark could not be reverted")
				}
			}
		}
	}

	// start the new executions; a failed publish is retried by the scheduler's queued
	// sweeper (the execution document holds everything needed to start it)
	if len(starts) > 0 && p.Starts != nil {
		for i, err := range p.Starts.PublishMany(ctx, starts) {
			if err != nil {
				p.Log.Warn("could not publish execution start; the sweeper will retry", "id", starts[i].ID, "error", err.Error())
			}
		}
	}

	// waits for an event (Step 10): every stored event is matched against the open
	// subscriptions; a match resumes its execution
	p.resumeWaits(ctx, items)

	// 4. settle every message. Acknowledgements are confirmed round trips (DoubleAck) and
	// each one is a write in the work-queue stream, so they run concurrently.
	var toAck []*item
	for _, it := range items {
		switch {
		case it.done:
		case it.retryErr != nil:
			p.nak(it)
		default:
			toAck = append(toAck, it)
		}
	}
	p.ackAll(ctx, toAck)
}

const ackParallelism = 32

func (p *Processor) ackAll(ctx context.Context, items []*item) {
	sem := make(chan struct{}, ackParallelism)
	var wg sync.WaitGroup
	for _, it := range items {
		sem <- struct{}{}
		wg.Add(1)
		go func(it *item) {
			defer func() { <-sem; wg.Done() }()
			p.ack(ctx, it)
			if !it.dup {
				p.Stats.Processed.Add(1)
			}
		}(it)
	}
	wg.Wait()
}

// isOwnRetry tells a redelivery of an event we started processing earlier from a true
// duplicate: on redelivery the seen key may carry this very event id. applied reports
// whether that earlier attempt also wrote the alert increment.
func (p *Processor) isOwnRetry(ctx context.Context, it *item) (own, applied bool, err error) {
	md, err := it.msg.Metadata()
	if err != nil || md.NumDelivered < 2 {
		return false, false, nil // a first delivery that conflicts is a genuine duplicate
	}
	s, _, err := p.St.Seen.Get(ctx, it.seenID)
	if errors.Is(err, store.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	own = s.EventID == it.ev.EventID
	return own, own && s.Applied, nil
}

func errText(e *store.BulkError) string {
	if e == nil {
		return "unknown error"
	}
	return e.Error()
}
