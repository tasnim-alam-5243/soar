package e2e

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/orchestrator"
	"github.com/yourorg/soar/internal/store"
)

// unreachable is what the OpenSearch client returns when the node is down.
func unreachable(path string) error {
	return &url.Error{Op: "Get", URL: "http://opensearch:9200" + path, Err: errors.New("dial tcp 172.18.0.2:9200: connect: connection refused")}
}

// outageIdem is the worker's idempotency store while OpenSearch is down, until a deadline.
type outageIdem struct {
	store.Repo[domain.Idempotency]
	until time.Time
}

func (o outageIdem) Get(ctx context.Context, id string) (domain.Idempotency, store.Version, error) {
	if time.Now().Before(o.until) {
		return domain.Idempotency{}, store.Version{}, unreachable("/soar-idempotency/_doc/" + id)
	}
	return o.Repo.Get(ctx, id)
}

// Spec 8.10 test 6 for steps: while OpenSearch is down the worker holds the step
// (heartbeats keep it) instead of nak-ing it. Before the fix the step's 3 deliveries
// were gone in well under a second here, it was dead-lettered and its node failed with
// delivery_failed. The outage outlasts the 3 s AckWait of this rig.
func TestFlowStepIsHeldWhileOpenSearchIsDown(t *testing.T) {
	f := newFlow(t, flowOpts{noLoops: true})
	f.w.Cfg.HoldMax = 300 * time.Millisecond
	wst := *f.st
	wst.Idempotency = outageIdem{Repo: f.st.Idempotency, until: time.Now().Add(6 * time.Second)}
	f2 := newFlowSharing(t, f, &wst)
	w := f2.workflow("held", "nodes: [{ id: a, action: test.count }]\n")
	e := f2.done(f2.run(w, nil, false))
	if e.Status != engine.StatusSucceeded {
		t.Fatalf("the step must run once OpenSearch is back: status=%s a=%+v", e.Status, nodeState(t, e, "a"))
	}
	if out := nodeState(t, e, "a").Output.(map[string]any); out["count"] != float64(1) {
		t.Errorf("the side effect must run exactly once: %v", out)
	}
	dl, err := f2.q.DLQList(f2.ctx, 10, 0)
	if err != nil || len(dl) != 0 {
		t.Fatalf("nothing may be dead-lettered by an outage: %+v %v", dl, err)
	}
	if f.w.Stats.Held.Load() == 0 || f.w.Stats.Retried.Load() != 0 {
		t.Errorf("held=%d retried=%d", f.w.Stats.Held.Load(), f.w.Stats.Retried.Load())
	}
}

// outageExecutions is the orchestrator's execution store while OpenSearch is down.
type outageExecutions struct {
	store.Repo[domain.Execution]
	until time.Time
}

func (o outageExecutions) Get(ctx context.Context, id string) (domain.Execution, store.Version, error) {
	if time.Now().Before(o.until) {
		return domain.Execution{}, store.Version{}, unreachable("/soar-executions/_doc/" + id)
	}
	return o.Repo.Get(ctx, id)
}

// The orchestrator holds a message while OpenSearch is down instead of nak-ing it
// every 2 s (10 deliveries: dead-lettered after ~20 s), then applies it.
func TestOrchestratorHoldsMessagesWhileOpenSearchIsDown(t *testing.T) {
	f := newFlow(t, flowOpts{noLoops: true})
	w := f.workflow("held-start", "nodes: [{ id: a, action: test.count }]\n")
	exe := orchestrator.NewExecution(w, "", domain.ExecutionTrigger{Type: "manual", User: "test"}, 2, false)
	exe.Context = (&engine.State{Status: engine.StatusQueued}).JSON()
	if _, err := f.st.Executions.Create(f.ctx, exe); err != nil {
		t.Fatal(err)
	}
	start, err := orchestrator.StartMessage(exe, orchestrator.StartPayload{})
	if err != nil {
		t.Fatal(err)
	}
	ost := *f.st
	ost.Executions = outageExecutions{Repo: f.st.Executions, until: time.Now().Add(3 * time.Second)}
	o := &orchestrator.Orchestrator{
		Q: f.q, St: &ost, Registry: f.reg, Workflows: orchestrator.NewWorkflowCache(&ost), Log: f.log,
		Partitions: 2, HoldMax: 200 * time.Millisecond,
	}
	m := &fakeMsg{data: start.Data}
	began := time.Now()
	o.HandleBatch(f.ctx, []orchestrator.Msg{m})
	if time.Since(began) < 3*time.Second {
		t.Fatal("the message must be held until OpenSearch answers")
	}
	if m.nakked.Load() != 0 || m.acked.Load() != 1 || o.Stats.Held.Load() != 1 {
		t.Fatalf("nakked=%d acked=%d held=%d", m.nakked.Load(), m.acked.Load(), o.Stats.Held.Load())
	}
	if got, _, _ := f.st.Executions.Get(f.ctx, exe.ID); got.Status != engine.StatusRunning {
		t.Fatalf("the start must be applied after the outage, status %s", got.Status)
	}
}
