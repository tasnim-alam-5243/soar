package e2e

// Execution flow end to end (Steps 6-8): a real JetStream server, the orchestrator,
// the worker and the scheduler jobs in one process over the in-memory store.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"gopkg.in/yaml.v3"

	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/audit"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/orchestrator"
	"github.com/yourorg/soar/internal/policy"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/queue/queuetest"
	"github.com/yourorg/soar/internal/scheduler"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/memory"
	"github.com/yourorg/soar/internal/worker"
)

type flow struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	q      *queue.Client
	reg    *actions.Registry
	orch   *orchestrator.Orchestrator
	w      *worker.Worker
	jobs   *scheduler.Jobs
	log    *slog.Logger
	counts sync.Map // idempotency key -> *atomic.Int64 (side effects of test.count)
	failN  sync.Map // node id -> *atomic.Int64 (attempts seen by test.fail_twice)
}

type flowOpts struct {
	workerStore *store.Store // a different store view for the worker (fault injection)
	pub         orchestrator.Publisher
	noLoops     bool
	workerLog   io.Writer // where the worker logs (default: discarded)
}

func newFlow(t *testing.T, o flowOpts) *flow {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	q, err := queue.Connect([]string{queuetest.Start(t)}, "flow-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	if err := q.EnsureStreams(ctx, queue.StreamOptions{EventsMaxBytes: 64 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := q.EnsureFlowStreams(ctx, queue.FlowOptions{MaxBytes: 64 << 20}); err != nil {
		t.Fatal(err)
	}
	st := memory.NewStore("soar")
	f := &flow{t: t, ctx: ctx, st: st, q: q, log: log}
	reg, err := actions.Build(actions.Deps{Store: st, Log: log, HTTP: actions.HTTPOptions{AllowLoopback: true}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.reg = reg
	f.registerTestActions()
	pol, _ := policy.New(nil)
	var pub orchestrator.Publisher = q
	if o.pub != nil {
		pub = o.pub
	}
	f.orch = &orchestrator.Orchestrator{Q: pub, St: st, Registry: reg, Workflows: orchestrator.NewWorkflowCache(st), Log: log, Partitions: 2, Policy: pol}
	keys, _ := security.NewKeyring("k1", make([]byte, 32), nil)
	secrets := &security.SecretStore{Repo: st.Secrets, Keys: keys}
	if _, err := secrets.Put(ctx, "intel_token", "tok-123", ""); err != nil {
		t.Fatal(err)
	}
	wst := st
	if o.workerStore != nil {
		wst = o.workerStore
	}
	wlog := log
	if o.workerLog != nil {
		wlog = slog.New(slog.NewTextHandler(o.workerLog, nil))
	}
	f.w = &worker.Worker{
		Q: q, St: wst, Registry: reg, Secrets: secrets, Log: wlog, Audit: &audit.Auditor{Sink: st.Audit, Log: log},
		Cfg: worker.Config{AckWait: 3 * time.Second, NakDelay: 100 * time.Millisecond},
	}
	f.jobs = &scheduler.Jobs{St: st, Q: q, Orch: &orchestrator.Orchestrator{Q: q, St: st, Registry: reg, Workflows: orchestrator.NewWorkflowCache(st), Log: log, Partitions: 2, Policy: pol}, Log: log, Partitions: 2}
	if o.noLoops {
		return f
	}
	for p := 0; p < 2; p++ {
		cons, err := q.ResultsConsumer(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		go f.orch.RunPartition(ctx, cons, 64)
	}
	consumers := map[[2]string]jetstream.Consumer{}
	for _, pc := range f.w.Consumers() {
		cons, err := q.StepsConsumer(ctx, pc[0], pc[1], func(c *jetstream.ConsumerConfig) { c.AckWait = 3 * time.Second })
		if err != nil {
			t.Fatal(err)
		}
		consumers[pc] = cons
	}
	go func() { _ = f.w.Run(ctx, consumers) }()
	go func() { _ = q.MoveExhausted(ctx, queue.StepsStream, "steps", f.w.OnDeadLetter) }()
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				_, _ = f.jobs.FireDueTimers(ctx, 500)
				_, _ = f.jobs.SweepOutbox(ctx, 200*time.Millisecond, 100)
			}
		}
	}()
	return f
}

func (f *flow) registerTestActions() {
	reg := func(m actions.Manifest, fn func(context.Context, actions.Input) (any, error)) {
		m.Version, m.Class = 1, actions.ClassBuiltin
		if m.Description == "" {
			m.Description = m.ID
		}
		if m.Risk == "" {
			m.Risk = "low"
		}
		if err := f.reg.Register(actions.Func{M: m, Fn: fn}); err != nil {
			f.t.Fatal(err)
		}
	}
	reg(actions.Manifest{ID: "test.count", SideEffect: "write", SupportsDryRun: true}, func(_ context.Context, in actions.Input) (any, error) {
		v, _ := f.counts.LoadOrStore(in.IdempotencyKey, &atomic.Int64{})
		n := v.(*atomic.Int64).Add(1)
		return map[string]any{"count": n, "params": in.Params}, nil
	})
	reg(actions.Manifest{ID: "test.fail_twice", SideEffect: "read", SupportsDryRun: true, RetryableErrors: []string{"http_503"}}, func(_ context.Context, in actions.Input) (any, error) {
		v, _ := f.failN.LoadOrStore(in.NodeID, &atomic.Int64{})
		if v.(*atomic.Int64).Add(1) <= 2 {
			return nil, actions.Errf("http_503", false, "upstream unavailable") // retryable through the manifest
		}
		return map[string]any{"ok": true, "attempt": in.Attempt}, nil
	})
	reg(actions.Manifest{ID: "test.destroy", SideEffect: "destructive", Risk: "high", SupportsDryRun: true}, func(_ context.Context, in actions.Input) (any, error) {
		if in.DryRun {
			return map[string]any{"dry_run": true, "would_destroy": in.Params["target"]}, nil
		}
		v, _ := f.counts.LoadOrStore("destroy:"+in.ExecutionID, &atomic.Int64{})
		v.(*atomic.Int64).Add(1)
		return map[string]any{"destroyed": in.Params["target"]}, nil
	})
}

func (f *flow) workflow(name, src string) domain.Workflow {
	f.t.Helper()
	var def map[string]any
	if err := yaml.Unmarshal([]byte(src), &def); err != nil {
		f.t.Fatal(err)
	}
	b, _ := json.Marshal(def) // stored definitions are JSON
	_ = json.Unmarshal(b, &def)
	if _, errs := engine.Compile(def, f.reg); len(errs) > 0 {
		f.t.Fatalf("workflow %s: %+v", name, errs)
	}
	w := domain.Workflow{Meta: domain.NewMeta(domain.PrefixWorkflow), Name: name, Version: 1, Status: "published", Definition: def, Checksum: "c-" + name}
	if _, err := f.st.Workflows.Create(f.ctx, w); err != nil {
		f.t.Fatal(err)
	}
	return w
}

func (f *flow) run(w domain.Workflow, inputs map[string]any, dryRun bool) string {
	f.t.Helper()
	exe := orchestrator.NewExecution(w, "", domain.ExecutionTrigger{Type: "manual", User: "test"}, 2, dryRun)
	exe.Context = (&engine.State{Status: engine.StatusQueued, Inputs: inputs, DryRun: dryRun}).JSON()
	if _, err := f.st.Executions.Create(f.ctx, exe); err != nil {
		f.t.Fatal(err)
	}
	m, err := orchestrator.StartMessage(exe, orchestrator.StartPayload{Inputs: inputs, DryRun: dryRun})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.q.Publish(f.ctx, m); err != nil {
		f.t.Fatal(err)
	}
	return exe.ID
}

func (f *flow) wait(id string, pred func(domain.Execution) bool, timeout time.Duration) domain.Execution {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	var last domain.Execution
	for time.Now().Before(deadline) {
		e, _, err := f.st.Executions.Get(f.ctx, id)
		if err == nil {
			last = e
			if pred(e) {
				return e
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := engine.StateFromJSON(last.Context)
	b, _ := json.MarshalIndent(st, "", " ")
	f.t.Fatalf("execution %s did not reach the expected state; status=%s error=%q outbox=%d\n%s", id, last.Status, last.Error, len(last.Outbox), b)
	return last
}

func (f *flow) done(id string) domain.Execution {
	return f.wait(id, func(e domain.Execution) bool { return engine.IsTerminal(e.Status) }, 15*time.Second)
}

func nodeState(t *testing.T, e domain.Execution, node string) *engine.NodeState {
	t.Helper()
	st, err := engine.StateFromJSON(e.Context)
	if err != nil || st.Nodes[node] == nil {
		t.Fatalf("no state for node %s: %v", node, err)
	}
	return st.Nodes[node]
}

// ---- tests ----

func TestFlowHTTPBranchAndCase(t *testing.T) {
	var auth atomic.Value
	intel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"score": 91, "url": "` + r.URL.Query().Get("q") + `"}`))
	}))
	defer intel.Close()
	f := newFlow(t, flowOpts{})
	w := f.workflow("phishing_triage", `
inputs: [{ name: url }, { name: intel }]
nodes:
  - id: check_url
    action: http.request
    params:
      url: 'inputs.intel + "/u?q=" + inputs.url'
      headers: { Authorization: '"Bearer " + secrets.intel_token' }
    retry: { max_attempts: 3, initial: 100ms, max: 200ms }
  - id: decide
    type: branch
    cases: [{ when: 'steps.check_url.output.body.score > 70', next: [open_case] }]
    default: [log_benign]
  - id: open_case
    action: case.create
    params: { title: '"Malicious URL " + inputs.url', severity: 4, tags: '["phishing"]' }
  - id: log_benign
    action: notify.log
    params: { message: benign }
edges: [check_url -> decide]
outputs: { case_id: 'steps.open_case.output.case_id' }
`)
	id := f.run(w, map[string]any{"url": "http://bad.example", "intel": intel.URL}, false)
	e := f.done(id)
	if e.Status != engine.StatusSucceeded {
		t.Fatalf("status %s: %s", e.Status, e.Error)
	}
	if auth.Load() != "Bearer tok-123" {
		t.Errorf("secret not resolved in the worker: %v", auth.Load())
	}
	if raw, _ := json.Marshal(e); strings.Contains(string(raw), "tok-123") {
		t.Fatal("a secret leaked into the execution document")
	}
	st, _ := engine.StateFromJSON(e.Context)
	caseID, _ := st.Output.(map[string]any)["case_id"].(string)
	c, _, err := f.st.Cases.Get(f.ctx, caseID)
	if err != nil || c.Title != "Malicious URL http://bad.example" || c.Severity != 4 {
		t.Fatalf("case %q: %+v %v", caseID, c, err)
	}
	if nodeState(t, e, "log_benign").Status != engine.NodeSkipped {
		t.Error("the other branch must be skipped")
	}
	runs, _ := f.st.StepRuns.List(f.ctx, store.ListQuery{Terms: map[string][]string{"execution_id": {id}}})
	if len(runs.Items) != 2 {
		t.Fatalf("want 2 step runs, got %d", len(runs.Items))
	}
	for _, r := range runs.Items {
		if raw, _ := json.Marshal(r); strings.Contains(string(raw), "tok-123") {
			t.Fatal("a secret leaked into a step run")
		}
	}
}

func TestFlowRetriesThroughTimers(t *testing.T) {
	f := newFlow(t, flowOpts{})
	w := f.workflow("flaky", `
nodes:
  - { id: call, action: test.fail_twice, retry: { max_attempts: 3, initial: 100ms, max: 200ms } }
`)
	e := f.done(f.run(w, nil, false))
	if e.Status != engine.StatusSucceeded || nodeState(t, e, "call").Attempt != 3 {
		t.Fatalf("status=%s attempt=%d err=%s", e.Status, nodeState(t, e, "call").Attempt, e.Error)
	}
	timers, _ := f.st.Timers.List(f.ctx, store.ListQuery{Terms: map[string][]string{"execution_id": {e.ID}, "kind": {"retry"}}})
	if len(timers.Items) != 2 {
		t.Errorf("want 2 retry timers, got %d", len(timers.Items))
	}
}

func TestFlowRetriesExhaustedTakeErrorPath(t *testing.T) {
	f := newFlow(t, flowOpts{})
	w := f.workflow("exhausted", `
nodes:
  - { id: call, action: test.fail_twice, retry: { max_attempts: 2, initial: 50ms }, on_error: [recover] }
  - { id: recover, action: notify.log, params: { message: '"recovered from " + steps.call.error.code' } }
`)
	e := f.done(f.run(w, nil, false))
	if e.Status != engine.StatusSucceeded || nodeState(t, e, "call").Status != engine.NodeFailed || nodeState(t, e, "recover").Status != engine.NodeSucceeded {
		t.Fatalf("status=%s call=%s recover=%s", e.Status, nodeState(t, e, "call").Status, nodeState(t, e, "recover").Status)
	}
}

type fakeMsg struct {
	data   []byte
	acked  atomic.Int32
	nakked atomic.Int32
}

func (m *fakeMsg) Data() []byte                     { return m.data }
func (m *fakeMsg) Subject() string                  { return "steps.normal.builtin" }
func (m *fakeMsg) InProgress() error                { return nil }
func (m *fakeMsg) NakWithDelay(time.Duration) error { m.nakked.Add(1); return nil }
func (m *fakeMsg) Term() error                      { return nil }
func (m *fakeMsg) DoubleAck(context.Context) error  { m.acked.Add(1); return nil }

func TestFlowRedeliveredStepRunsItsSideEffectOnce(t *testing.T) {
	f := newFlow(t, flowOpts{noLoops: true})
	exe := domain.Execution{Meta: domain.NewMeta(domain.PrefixExecution), Partition: 1}
	d := engine.Dispatch{
		ExecutionID: exe.ID, NodeID: "n", Action: "test.count", Attempt: 1, Params: map[string]any{}, TimeoutMS: 5000,
		IdempotencyKey: engine.StepKey(exe.ID, "n", 0, 1),
	}
	m, err := orchestrator.DispatchMessage(exe, engine.Command{Kind: engine.CmdDispatch, Key: d.IdempotencyKey, Dispatch: &d}, f.reg)
	if err != nil {
		t.Fatal(err)
	}
	// the worker processes the step, then "crashes" before its ack lands; JetStream redelivers
	for i := 0; i < 3; i++ {
		f.w.Handle(f.ctx, &fakeMsg{data: m.Data})
	}
	v, _ := f.counts.Load(d.IdempotencyKey)
	if n := v.(*atomic.Int64).Load(); n != 1 {
		t.Fatalf("side effect ran %d times, want exactly once", n)
	}
	if f.w.Stats.Replayed.Load() != 2 {
		t.Errorf("replays %d, want 2", f.w.Stats.Replayed.Load())
	}
}

// failingPublisher drops the first n publishes, like a NATS outage right after the
// orchestrator saved its state.
type failingPublisher struct {
	q    *queue.Client
	left atomic.Int32
}

func (p *failingPublisher) Publish(ctx context.Context, m queue.Msg) error {
	if p.left.Add(-1) >= 0 {
		return errors.New("nats unavailable")
	}
	return p.q.Publish(ctx, m)
}

func TestFlowOutboxSweeperCompletesAfterPublishFailure(t *testing.T) {
	fp := &failingPublisher{}
	fp.left.Store(1)
	f := newFlow(t, flowOpts{pub: fp})
	fp.q = f.q
	w := f.workflow("outbox", "nodes: [{ id: a, action: test.count }, { id: b, action: test.count }]\nedges: [a -> b]\n")
	e := f.done(f.run(w, nil, false))
	if e.Status != engine.StatusSucceeded {
		t.Fatalf("the sweeper must finish the outbox: %s %s", e.Status, e.Error)
	}
	if fp.left.Load() >= 0 {
		t.Fatal("the publish failure was never injected")
	}
}

func TestFlowDestructiveActionNeedsApproval(t *testing.T) {
	f := newFlow(t, flowOpts{})
	w := f.workflow("purge", "nodes: [{ id: purge, action: test.destroy, params: { target: mailbox-1 } }]\n")
	id := f.run(w, nil, false)
	var ap domain.Approval
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		page, _ := f.st.Approvals.List(f.ctx, store.ListQuery{Terms: map[string][]string{"execution_id": {id}}})
		if len(page.Items) == 1 {
			ap = page.Items[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ap.ID == "" || ap.Action != "test.destroy" || len(ap.Command) == 0 {
		t.Fatalf("policy approval not requested: %+v", ap)
	}
	if v, ok := f.counts.Load("destroy:" + id); ok && v.(*atomic.Int64).Load() > 0 {
		t.Fatal("the destructive action ran before approval")
	}
	// what the API does on approval: release the held-back step
	exe, _, _ := f.st.Executions.Get(f.ctx, id)
	cmd, _ := orchestrator.DecodeCommand(ap.Command)
	m, _ := orchestrator.DispatchMessage(exe, cmd, f.reg)
	if err := f.q.Publish(f.ctx, m); err != nil {
		t.Fatal(err)
	}
	if e := f.done(id); e.Status != engine.StatusSucceeded {
		t.Fatalf("status %s %s", e.Status, e.Error)
	}

	// an approval node right before the destructive step satisfies the policy
	w2 := f.workflow("purge_approved", `
nodes:
  - { id: ask, type: approval, message: '"purge?"' }
  - { id: purge, action: test.destroy, params: { target: mailbox-2 } }
edges: [ask -> purge]
`)
	id2 := f.run(w2, nil, false)
	e2 := f.wait(id2, func(e domain.Execution) bool { return e.Status == engine.StatusWaiting }, 10*time.Second)
	m2, _ := orchestrator.ControlMessage(queue.MsgApproval, "approval:x", e2, "ask", orchestrator.ApprovalPayload{Approved: true, Actor: "alice"})
	if err := f.q.Publish(f.ctx, m2); err != nil {
		t.Fatal(err)
	}
	if e := f.done(id2); e.Status != engine.StatusSucceeded {
		t.Fatalf("pre-approved step: %s %s", e.Status, e.Error)
	}

	// a dry run needs no approval and changes nothing
	e3 := f.done(f.run(w, nil, true))
	out := nodeState(t, e3, "purge").Output.(map[string]any)
	if e3.Status != engine.StatusSucceeded || out["dry_run"] != true {
		t.Fatalf("dry run: %s %v", e3.Status, out)
	}
}

// flakyIdem makes the worker's idempotency store fail, so every delivery of a step
// is nak-ed until JetStream gives up.
type flakyIdem struct{ store.Repo[domain.Idempotency] }

func (flakyIdem) Get(context.Context, string) (domain.Idempotency, store.Version, error) {
	return domain.Idempotency{}, store.Version{}, errors.New("opensearch unavailable")
}

func TestFlowPoisonStepIsDeadLetteredAndFailsTheNode(t *testing.T) {
	f := newFlow(t, flowOpts{noLoops: true})
	wst := *f.st
	wst.Idempotency = flakyIdem{f.st.Idempotency}
	f2 := newFlowSharing(t, f, &wst)
	w := f2.workflow("poison", `
nodes:
  - { id: a, action: test.count, on_error: [handler] }
  - { id: handler, type: set, vars: { reason: 'steps.a.error.code' } }
`)
	e := f2.done(f2.run(w, nil, false))
	if e.Status != engine.StatusSucceeded || nodeState(t, e, "a").Error == nil || nodeState(t, e, "a").Error.Code != "delivery_failed" {
		t.Fatalf("status=%s a=%+v", e.Status, nodeState(t, e, "a"))
	}
	dl, err := f2.q.DLQList(f2.ctx, 10, 0)
	if err != nil || len(dl) != 1 || !strings.HasPrefix(dl[0].OriginSubject, "steps.") {
		t.Fatalf("dlq: %+v %v", dl, err)
	}
}

// newFlowSharing starts the loops of f with the worker reading through wst.
func newFlowSharing(t *testing.T, f *flow, wst *store.Store) *flow {
	f.w.St = wst
	ctx := f.ctx
	for p := 0; p < 2; p++ {
		cons, err := f.q.ResultsConsumer(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		go f.orch.RunPartition(ctx, cons, 64)
	}
	consumers := map[[2]string]jetstream.Consumer{}
	for _, pc := range f.w.Consumers() {
		cons, err := f.q.StepsConsumer(ctx, pc[0], pc[1], func(c *jetstream.ConsumerConfig) { c.AckWait = 3 * time.Second })
		if err != nil {
			t.Fatal(err)
		}
		consumers[pc] = cons
	}
	go func() { _ = f.w.Run(ctx, consumers) }()
	go func() { _ = f.q.MoveExhausted(ctx, queue.StepsStream, "steps", f.w.OnDeadLetter) }()
	go func() {
		for ctx.Err() == nil {
			_, _ = f.jobs.FireDueTimers(ctx, 500)
			time.Sleep(50 * time.Millisecond)
		}
	}()
	return f
}

func TestFlowSubworkflowForeachWaitCancel(t *testing.T) {
	f := newFlow(t, flowOpts{})
	f.workflow("enrich_ip", `
inputs: [{ name: ip, required: true }]
nodes: [{ id: look, action: transform.map, params: { value: '{"ip": inputs.ip, "score": 7}' } }]
outputs: { score: 'steps.look.output.score' }
`)
	parent := f.workflow("parent", `
nodes:
  - { id: child, type: subworkflow, workflow: enrich_ip, inputs: { ip: '"10.0.0.1"' } }
  - id: each
    type: foreach
    items: '["a@x", "b@x", "c@x"]'
    concurrency: 2
    action: test.count
    params: { mailbox: 'item', score: 'steps.child.output.score' }
edges: [child -> each]
`)
	e := f.done(f.run(parent, nil, false))
	if e.Status != engine.StatusSucceeded {
		t.Fatalf("parent: %s %s", e.Status, e.Error)
	}
	items := nodeState(t, e, "each").Output.([]any)
	if len(items) != 3 || items[2].(map[string]any)["output"].(map[string]any)["params"].(map[string]any)["score"] != float64(7) {
		t.Fatalf("foreach output: %v", items)
	}
	children, _ := f.st.Executions.List(f.ctx, store.ListQuery{Terms: map[string][]string{"trigger.parent_execution_id": {e.ID}}})
	if len(children.Items) != 1 || children.Items[0].Status != engine.StatusSucceeded {
		t.Fatalf("child execution: %+v", children.Items)
	}

	waiter := f.workflow("waiter", "nodes: [{ id: pause, type: wait, duration: 1h }, { id: after, action: test.count }]\nedges: [pause -> after]\n")
	id := f.run(waiter, nil, false)
	we := f.wait(id, func(e domain.Execution) bool { return e.Status == engine.StatusWaiting }, 10*time.Second)
	m, _ := orchestrator.ControlMessage(queue.MsgCancel, "cancel:"+id, we, "", orchestrator.CancelPayload{Actor: "alice", Reason: "test"})
	if err := f.q.Publish(f.ctx, m); err != nil {
		t.Fatal(err)
	}
	ce := f.done(id)
	if ce.Status != engine.StatusCancelled || !strings.Contains(ce.Error, "alice") {
		t.Fatalf("cancel: %s %q", ce.Status, ce.Error)
	}
}
