package processor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/queue/queuetest"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/memory"
)

// fakeBulk interprets just the operations the processor emits, so tests can check
// behaviour without OpenSearch. The scripted update and 409 semantics are verified
// against the real server by the integration tests.
type fakeBulk struct {
	mu          sync.Mutex
	docs        map[string]map[string]any
	requests    int
	failRequest int            // next N Write calls fail as a whole
	failItem    map[string]int // doc id -> times to answer 429
	downUntil   time.Time      // until then every Write fails like an unreachable OpenSearch
	mirror      func(index, id string, doc map[string]any)
}

func newFakeBulk() *fakeBulk {
	return &fakeBulk{docs: map[string]map[string]any{}, failItem: map[string]int{}}
}

func (f *fakeBulk) count(index string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k := range f.docs {
		if strings.HasPrefix(k, index+"/") {
			n++
		}
	}
	return n
}

func asMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func (f *fakeBulk) Write(_ context.Context, ops []store.BulkOp) ([]store.BulkResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if time.Now().Before(f.downUntil) {
		return nil, &url.Error{Op: "Post", URL: "http://opensearch:9200/_bulk", Err: errors.New("dial tcp 172.18.0.2:9200: connect: connection refused")}
	}
	if f.failRequest > 0 {
		f.failRequest--
		return nil, errors.New("connection refused")
	}
	out := make([]store.BulkResult, len(ops))
	for i, op := range ops {
		key := op.Index + "/" + op.ID
		if f.failItem[op.ID] > 0 {
			f.failItem[op.ID]--
			out[i] = store.BulkResult{Status: 429, ID: op.ID, Err: &store.BulkError{Type: "es_rejected_execution_exception", Reason: "queue full"}}
			continue
		}
		switch op.Action {
		case store.BulkCreate:
			if _, ok := f.docs[key]; ok {
				out[i] = store.BulkResult{Status: 409, ID: op.ID, Err: &store.BulkError{Type: "version_conflict_engine_exception", Reason: "exists"}}
				continue
			}
			f.docs[key] = asMap(op.Doc)
			if f.mirror != nil {
				f.mirror(op.Index, op.ID, f.docs[key])
			}
			out[i] = store.BulkResult{Status: 201, ID: op.ID}
		case store.BulkUpdate:
			cur, ok := f.docs[key]
			if partial, isDoc := op.Body["doc"].(map[string]any); isDoc { // partial update, like OpenSearch
				if !ok {
					out[i] = store.BulkResult{Status: 404, ID: op.ID, Err: &store.BulkError{Type: "document_missing_exception", Reason: "missing"}}
					continue
				}
				for k, v := range partial {
					cur[k] = v
				}
				if f.mirror != nil {
					f.mirror(op.Index, op.ID, cur)
				}
				out[i] = store.BulkResult{Status: 200, ID: op.ID}
				continue
			}
			if !ok {
				f.docs[key] = asMap(op.Body["upsert"])
				out[i] = store.BulkResult{Status: 201, ID: op.ID}
				continue
			}
			script := op.Body["script"].(map[string]any)
			params := script["params"].(map[string]any)
			if strings.Contains(script["source"].(string), "occurrence_count") {
				cur["occurrence_count"] = cur["occurrence_count"].(float64) + 1
				if sev, ok := params["sev"].(int); ok && float64(sev) > cur["severity"].(float64) {
					cur["severity"] = float64(sev)
				}
			}
			cur["last_seen"] = params["t"]
			out[i] = store.BulkResult{Status: 200, ID: op.ID}
		default:
			out[i] = store.BulkResult{Status: 400, Err: &store.BulkError{Type: "unsupported", Reason: string(op.Action)}}
		}
	}
	return out, nil
}

type dlqRec struct{ reason string }

type fakeDLQ struct {
	mu   sync.Mutex
	recs []dlqRec
}

func (d *fakeDLQ) SendToDLQ(_ context.Context, _, reason, _ string, _ []byte, _ string, _ map[string]string) error {
	d.mu.Lock()
	d.recs = append(d.recs, dlqRec{reason})
	d.mu.Unlock()
	return nil
}

type rig struct {
	t    *testing.T
	q    *queue.Client
	cons jetstream.Consumer
	p    *Processor
	bulk *fakeBulk
	st   *store.Store
	dlq  *fakeDLQ
}

func newRig(t *testing.T) *rig {
	t.Helper()
	q, err := queue.Connect([]string{queuetest.Start(t)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	ctx := context.Background()
	if err := q.EnsureStreams(ctx, queue.StreamOptions{EventsMaxBytes: 256 << 20}); err != nil {
		t.Fatal(err)
	}
	cons, err := q.EventsConsumer(ctx, func(c *jetstream.ConsumerConfig) {
		c.AckWait, c.BackOff, c.MaxDeliver = 2*time.Second, nil, 10
	})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := memory.NewStore("soar")
	tr, err := NewTriggerIndex(st, log)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, q: q, cons: cons, bulk: newFakeBulk(), st: st, dlq: &fakeDLQ{}}
	// In production the bulk writer and the store talk to the same OpenSearch; share the seen keys here too.
	r.bulk.mirror = func(index, id string, doc map[string]any) {
		if index == "soar-seen" {
			b, _ := json.Marshal(doc)
			var s domain.Seen
			_ = json.Unmarshal(b, &s)
			_, _ = st.Seen.Put(context.Background(), s)
		}
	}
	r.p = &Processor{
		Cons: cons, Bulk: r.bulk, St: st, DLQ: r.dlq, Triggers: tr, Log: log,
		Cfg: Config{Batch: 256, FetchWait: 300 * time.Millisecond, NakDelay: 50 * time.Millisecond, LocalRetry: 3, Partitions: 8},
	}
	return r
}

func (r *rig) publish(ev domain.Event, msgID string) {
	r.t.Helper()
	data, _ := json.Marshal(ev)
	if msgID == "" {
		msgID = ev.EventID
	}
	if err := r.q.Publish(context.Background(), queue.Msg{Subject: queue.EventSubject(ev.SourceID), ID: msgID, Data: data}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) pending() uint64 {
	st, _ := r.q.JS().Stream(context.Background(), queue.EventsStream)
	info, _ := st.Info(context.Background())
	return info.State.Msgs
}

// drain processes until the stream is empty or the deadline passes.
func (r *rig) drain(timeout time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		batch, err := r.cons.Fetch(256, jetstream.FetchMaxWait(300*time.Millisecond))
		if err != nil {
			continue
		}
		var msgs []jetstream.Msg
		for m := range batch.Messages() {
			msgs = append(msgs, m)
		}
		if len(msgs) > 0 {
			r.p.HandleBatch(context.Background(), msgs)
		}
		time.Sleep(20 * time.Millisecond) // let acks reach the server
		if r.pending() == 0 && len(msgs) == 0 {
			return
		}
	}
}

var seq int

func event(external, title, fingerprint string, sev int, obs ...domain.Observable) domain.Event {
	seq++
	now := time.Now().UTC()
	return domain.Event{
		EventID: fmt.Sprintf("evt_%d_%s", seq, external), SourceID: "wazuh-prod", ReceivedAt: now, DedupWindowSec: 600,
		Alert: domain.Alert{
			EventID: external, Kind: "alert", SourceID: "wazuh-prod", System: "wazuh-prod", ExternalID: external,
			Fingerprint: fingerprint, Title: title, Severity: sev, Observables: obs, OccurrenceCount: 1, ReceivedAt: now, LastSeen: &now, Status: "new",
		},
	}
}

func TestEventsBecomeAlertsObservablesAndRepeatsAreGrouped(t *testing.T) {
	r := newRig(t)
	host := domain.Observable{Type: "host", Value: "web01"}
	r.publish(event("a.1", "ssh brute force", "fp-ssh", 2, host, domain.Observable{Type: "ip", Value: "10.0.0.1", Role: "source"}), "")
	r.publish(event("a.2", "ssh brute force", "fp-ssh", 4, host, domain.Observable{Type: "ip", Value: "10.0.0.1", Role: "source"}), "") // same alert, later and worse
	r.publish(event("a.3", "disk full", "fp-disk", 1, host), "")
	r.drain(5 * time.Second)

	if r.pending() != 0 {
		t.Fatalf("every message must be acknowledged, %d left", r.pending())
	}
	if n := r.bulk.count("soar-alerts"); n != 2 {
		t.Fatalf("three events with two fingerprints make 2 alert documents, got %d", n)
	}
	var ssh map[string]any
	for k, d := range r.bulk.docs {
		if strings.HasPrefix(k, "soar-alerts/") && d["title"] == "ssh brute force" {
			ssh = d
		}
	}
	if ssh["occurrence_count"] != float64(2) || ssh["severity"] != float64(4) {
		t.Errorf("repeats must increment occurrence_count and raise severity: %v", ssh)
	}
	if n := r.bulk.count("soar-observables"); n != 2 { // web01 and 10.0.0.1, once each
		t.Errorf("unique observables only: got %d", n)
	}
	if r.p.Stats.Processed.Load() != 3 {
		t.Errorf("processed=%d", r.p.Stats.Processed.Load())
	}
}

func TestSameExternalIdIsADuplicateAndNotCountedTwice(t *testing.T) {
	r := newRig(t)
	r.publish(event("dup.1", "x", "fp-x", 3), "m1")
	second := event("dup.1", "x", "fp-x", 3) // same alert arriving again by the other transport
	r.publish(second, "m2")
	r.drain(5 * time.Second)
	if r.pending() != 0 {
		t.Fatalf("both messages must be settled, %d left", r.pending())
	}
	if r.p.Stats.Duplicates.Load() != 1 {
		t.Errorf("duplicates=%d, want 1", r.p.Stats.Duplicates.Load())
	}
	for k, d := range r.bulk.docs {
		if strings.HasPrefix(k, "soar-alerts/") && d["occurrence_count"] != float64(1) {
			t.Errorf("a duplicate must not raise the count: %v", d)
		}
	}
}

func TestFailedWriteIsRetriedWithoutLoss(t *testing.T) {
	r := newRig(t)
	r.publish(event("r.1", "retry me", "fp-r", 3, domain.Observable{Type: "host", Value: "h"}), "")
	r.bulk.failRequest = 2 // seen-guard request, then the main one: whole requests fail
	r.drain(10 * time.Second)
	if r.pending() != 0 {
		t.Fatalf("the message must eventually be written and acked, %d left", r.pending())
	}
	if r.bulk.count("soar-alerts") != 1 {
		t.Fatalf("alert missing after retry")
	}
	for k, d := range r.bulk.docs {
		if strings.HasPrefix(k, "soar-alerts/") && d["occurrence_count"] != float64(1) {
			t.Errorf("a retried event must not be counted twice: %v", d)
		}
	}
	if r.p.Stats.Retried.Load() == 0 {
		t.Error("expected at least one nak/redelivery")
	}
}

func TestRedeliveryAfterPartialProgressDoesNotLoseTheEvent(t *testing.T) {
	r := newRig(t)
	e := event("p.1", "partial", "fp-p", 3)
	r.publish(e, "")
	// first attempt: the seen key is written, then the main write fails as a whole
	r.bulk.failRequest = 0
	var once sync.Once
	wrapped := &hookBulk{inner: r.bulk, onWrite: func(n int) error {
		if n == 2 {
			var err error
			once.Do(func() { err = errors.New("crash between the two writes") })
			return err
		}
		return nil
	}}
	r.p.Bulk = wrapped
	r.drain(10 * time.Second)
	if r.bulk.count("soar-seen") != 1 || r.bulk.count("soar-alerts") != 1 || r.pending() != 0 {
		t.Fatalf("seen=%d alerts=%d pending=%d: the redelivered event must recognise its own seen key and finish",
			r.bulk.count("soar-seen"), r.bulk.count("soar-alerts"), r.pending())
	}
	if r.p.Stats.Duplicates.Load() != 0 {
		t.Error("the event's own retry is not a duplicate")
	}
}

// The chaos case of spec 8.10 #2: the main write reaches OpenSearch, but the processor
// dies before it can acknowledge (here: the reply is lost and the message is redelivered).
// The redelivery recognises its own seen key AND that the increment is already written,
// so the alert is counted once; found by killing a processor during a 1,000/s load test.
func TestRedeliveryAfterTheMainWriteDoesNotCountTwice(t *testing.T) {
	r := newRig(t)
	e := event("w.1", "written", "fp-w", 3)
	r.publish(e, "")
	var once sync.Once
	r.p.Bulk = &afterBulk{inner: r.bulk, after: func(n int) error {
		if n == 2 {
			var err error
			once.Do(func() { err = errors.New("process died after sending the main write") })
			return err
		}
		return nil
	}}
	r.drain(10 * time.Second)
	if r.pending() != 0 || r.bulk.count("soar-alerts") != 1 {
		t.Fatalf("pending=%d alerts=%d", r.pending(), r.bulk.count("soar-alerts"))
	}
	for k, d := range r.bulk.docs {
		if strings.HasPrefix(k, "soar-alerts/") && d["occurrence_count"] != float64(1) {
			t.Errorf("the redelivered event was counted again: %v", d)
		}
		if strings.HasPrefix(k, "soar-seen/") && d["applied"] != true {
			t.Errorf("the seen key must record the applied increment: %v", d)
		}
	}
	if r.p.Stats.Duplicates.Load() != 0 {
		t.Error("the event's own retry is not a duplicate")
	}
}

// afterBulk performs the write, then may fail as if the reply never arrived.
type afterBulk struct {
	inner *fakeBulk
	after func(n int) error
	n     int
}

func (h *afterBulk) Write(ctx context.Context, ops []store.BulkOp) ([]store.BulkResult, error) {
	h.n++
	res, err := h.inner.Write(ctx, ops)
	if aerr := h.after(h.n); aerr != nil {
		return nil, aerr
	}
	return res, err
}

type hookBulk struct {
	inner   *fakeBulk
	onWrite func(n int) error
	n       int
}

func (h *hookBulk) Write(ctx context.Context, ops []store.BulkOp) ([]store.BulkResult, error) {
	h.n++
	if err := h.onWrite(h.n); err != nil {
		return nil, err
	}
	return h.inner.Write(ctx, ops)
}

func TestTransientItemFailuresAreRetriedLocally(t *testing.T) {
	r := newRig(t)
	e := event("t.1", "busy", "fp-t", 3)
	r.publish(e, "")
	r.bulk.failItem[AlertID("fp-t", e.ReceivedAt, 600)] = 2 // 429 twice, then fine
	r.drain(10 * time.Second)
	if r.pending() != 0 || r.bulk.count("soar-alerts") != 1 {
		t.Fatalf("pending=%d alerts=%d", r.pending(), r.bulk.count("soar-alerts"))
	}
	if r.p.Stats.Retried.Load() != 0 {
		t.Error("a transient item failure must be retried in place, not by redelivering the message (that would double-count)")
	}
}

func TestPoisonMessagesGoToTheDLQAndDoNotBlockOthers(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_ = r.q.Publish(ctx, queue.Msg{Subject: queue.EventSubject("wazuh-prod"), ID: "bad1", Data: []byte("not json")})
	_ = r.q.Publish(ctx, queue.Msg{Subject: queue.EventSubject("wazuh-prod"), ID: "bad2", Data: []byte(`{"event_id":"x"}`)}) // no alert
	r.publish(event("ok.1", "fine", "fp-ok", 3), "")
	r.drain(5 * time.Second)
	if r.pending() != 0 {
		t.Fatalf("%d messages stuck", r.pending())
	}
	if len(r.dlq.recs) != 2 || r.p.Stats.DeadLettered.Load() != 2 {
		t.Errorf("want 2 dead letters, got %+v", r.dlq.recs)
	}
	if r.bulk.count("soar-alerts") != 1 {
		t.Error("the good event must still be processed")
	}
}

func TestPublishedWorkflowsAreTriggeredOncePerAlertGroup(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	wf := domain.Workflow{
		Meta: domain.NewMeta(domain.PrefixWorkflow), Name: "triage", Version: 2, Status: "published",
		Definition: domain.JSON{"nodes": []any{}}, Triggers: []domain.Trigger{{Type: "alert", Match: `alert.severity >= 3 && alert.source_id == "wazuh-prod"`}},
	}
	old := wf
	old.ID, old.Version, old.Triggers = domain.NewID(domain.PrefixWorkflow), 1, []domain.Trigger{{Type: "alert", Match: "false"}}
	other := domain.Workflow{
		Meta: domain.NewMeta(domain.PrefixWorkflow), Name: "never", Version: 1, Status: "published", Definition: domain.JSON{},
		Triggers: []domain.Trigger{{Type: "alert", Match: "alert.severity >= 5"}},
	}
	draft := domain.Workflow{
		Meta: domain.NewMeta(domain.PrefixWorkflow), Name: "draft", Version: 1, Status: "draft", Definition: domain.JSON{},
		Triggers: []domain.Trigger{{Type: "alert"}},
	}
	for _, w := range []domain.Workflow{wf, old, other, draft} {
		if _, err := r.st.Workflows.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.p.Triggers.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	r.publish(event("w.1", "brute force", "fp-w", 4), "")
	r.publish(event("w.2", "brute force", "fp-w", 4), "") // same alert group
	r.publish(event("w.3", "minor", "fp-minor", 2), "")   // below the trigger's severity
	r.drain(5 * time.Second)
	if r.pending() != 0 {
		t.Fatalf("%d left", r.pending())
	}
	if n := r.bulk.count("soar-executions"); n != 1 {
		t.Fatalf("one execution for the grouped alert, none for the minor one, got %d", n)
	}
	for k, d := range r.bulk.docs {
		if strings.HasPrefix(k, "soar-executions/") {
			if d["workflow_id"] != wf.ID || d["workflow_version"] != float64(2) || d["status"] != "queued" {
				t.Errorf("execution must pin the latest published version: %v", d)
			}
			if d["trigger"].(map[string]any)["alert_id"] == "" || d["partition"] == nil {
				t.Errorf("execution lacks trigger/partition: %v", d)
			}
		}
	}
}

func TestRouteFilterStartsTheNamedWorkflow(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.st.Workflows.Create(ctx, domain.Workflow{
		Meta: domain.NewMeta(domain.PrefixWorkflow), Name: "ransomware_response", Version: 1,
		Status: "published", Definition: domain.JSON{},
	}); err != nil {
		t.Fatal(err)
	}
	_ = r.p.Triggers.Refresh(ctx)
	e := event("route.1", "ransom", "fp-route", 1)
	e.Route = "ransomware_response"
	r.publish(e, "")
	r.drain(5 * time.Second)
	if r.bulk.count("soar-executions") != 1 {
		t.Fatalf("route must start the workflow even without a matching trigger, got %d", r.bulk.count("soar-executions"))
	}
}

func TestLargeBacklogIsDrainedInBatches(t *testing.T) {
	r := newRig(t)
	const n = 1000
	msgs := make([]queue.Msg, n)
	for i := range msgs {
		e := event(fmt.Sprint("bulk.", i), "load", fmt.Sprint("fp-", i%50), 2, domain.Observable{Type: "host", Value: fmt.Sprint("h", i%10)})
		data, _ := json.Marshal(e)
		msgs[i] = queue.Msg{Subject: queue.EventSubject("wazuh-prod"), ID: e.EventID, Data: data}
	}
	for i, err := range r.q.PublishMany(context.Background(), msgs) {
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	r.drain(30 * time.Second)
	if r.pending() != 0 {
		t.Fatalf("%d left", r.pending())
	}
	total := 0.0
	for k, d := range r.bulk.docs {
		if strings.HasPrefix(k, "soar-alerts/") {
			total += d["occurrence_count"].(float64)
		}
	}
	if total != n || r.bulk.count("soar-seen") != n || r.bulk.count("soar-observables") != 10 {
		t.Errorf("occurrences=%v seen=%d observables=%d: every event exactly once", total, r.bulk.count("soar-seen"), r.bulk.count("soar-observables"))
	}
}

// A workflow stored with its triggers inside the definition (the spec 6 DSL form,
// written before the API lifted them into the document) must still fire.
func TestTriggersInsideTheDefinitionFire(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	legacy := domain.Workflow{
		Meta: domain.NewMeta(domain.PrefixWorkflow), Name: "legacy", Version: 1, Status: "published",
		Definition: domain.JSON{"nodes": []any{}, "triggers": []any{map[string]any{"type": "alert", "match": `alert.title == "from definition"`}}},
	}
	if _, err := r.st.Workflows.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := r.p.Triggers.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	r.publish(event("d.1", "from definition", "fp-def", 3), "")
	r.publish(event("d.2", "something else", "fp-other", 3), "")
	r.drain(5 * time.Second)
	if n := r.bulk.count("soar-executions"); n != 1 {
		t.Fatalf("the definition's trigger must start exactly one execution, got %d", n)
	}
}

// Spec 8.10 test 6: while OpenSearch is down the processor holds what it fetched and
// pauses; nothing is nak-ed, redelivered or dead-lettered, and the backlog drains once
// OpenSearch is back. Before the fix a ~10 s outage used up the 5 deliveries and the
// events went to the DLQ. The outage here outlasts the rig's 2 s AckWait, so the held
// messages survive only through InProgress heartbeats.
func TestOpenSearchOutageHoldsEventsInsteadOfDeadLettering(t *testing.T) {
	r := newRig(t)
	r.p.Cfg.Workers, r.p.Cfg.HoldMax, r.p.Cfg.Heartbeat = 2, 300*time.Millisecond, 300*time.Millisecond
	r.bulk.mu.Lock()
	r.bulk.downUntil = time.Now().Add(5 * time.Second)
	r.bulk.mu.Unlock()
	for i := 0; i < 20; i++ {
		r.publish(event(fmt.Sprintf("o.%d", i), "outage", fmt.Sprintf("fp-o%d", i), 3), "")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.p.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	time.Sleep(3 * time.Second) // mid-outage
	if !r.p.outage.Load() {
		t.Fatal("the processor must be paused while OpenSearch is down")
	}
	for i := 20; i < 40; i++ { // arrivals during the outage wait in the stream
		r.publish(event(fmt.Sprintf("o.%d", i), "outage", fmt.Sprintf("fp-o%d", i), 3), "")
	}
	info, err := r.cons.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.NumAckPending > 20 {
		t.Errorf("paused loops must not fetch more: %d in flight", info.NumAckPending)
	}

	deadline := time.Now().Add(20 * time.Second)
	for r.pending() != 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if r.pending() != 0 || r.bulk.count("soar-alerts") != 40 {
		t.Fatalf("after recovery the backlog must drain: pending=%d alerts=%d", r.pending(), r.bulk.count("soar-alerts"))
	}
	if info, _ = r.cons.Info(ctx); info.NumRedelivered != 0 {
		t.Errorf("no delivery may be spent on an outage: %d redelivered", info.NumRedelivered)
	}
	if r.p.Stats.Retried.Load() != 0 || len(r.dlq.recs) != 0 || r.p.Stats.Held.Load() == 0 {
		t.Errorf("retried=%d dead-lettered=%d held=%d", r.p.Stats.Retried.Load(), len(r.dlq.recs), r.p.Stats.Held.Load())
	}
	if r.p.outage.Load() {
		t.Error("the pause must end when OpenSearch is back")
	}
}

// Items rejected for overload (429) are held like an outage, however long it lasts,
// rather than nak-ed after LocalRetry attempts.
func TestOverloadRejectionsBeyondLocalRetriesAreHeld(t *testing.T) {
	r := newRig(t)
	r.p.Cfg.HoldMax, r.p.Cfg.Heartbeat = 100*time.Millisecond, 100*time.Millisecond
	e := event("ov.1", "overload", "fp-ov", 3)
	r.publish(e, "")
	r.bulk.failItem[AlertID("fp-ov", e.ReceivedAt, 600)] = 8 // well past LocalRetry (3)
	r.drain(15 * time.Second)
	if r.pending() != 0 || r.bulk.count("soar-alerts") != 1 {
		t.Fatalf("pending=%d alerts=%d", r.pending(), r.bulk.count("soar-alerts"))
	}
	if r.p.Stats.Retried.Load() != 0 || len(r.dlq.recs) != 0 {
		t.Errorf("an overload must not cost deliveries: retried=%d dlq=%d", r.p.Stats.Retried.Load(), len(r.dlq.recs))
	}
}
