package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/orchestrator"
	"github.com/yourorg/soar/internal/processor"
	"github.com/yourorg/soar/internal/store"
)

func (f *flow) subscription(exeID, node string) domain.Subscription {
	f.t.Helper()
	var s domain.Subscription
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		if s, _, err = f.st.Subscriptions.Get(f.ctx, engine.SubscriptionID(exeID, node)); err == nil {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("no subscription for %s/%s", exeID, node)
	return s
}

// deliver plays the processor's part: match the event against the open waits and
// publish the matches.
func (f *flow) deliver(idx *processor.WaitIndex, event map[string]any) int {
	f.t.Helper()
	if err := idx.Refresh(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	subs := idx.Match(event)
	for _, s := range subs {
		m, err := orchestrator.EventMessage(s, event)
		if err != nil {
			f.t.Fatal(err)
		}
		if err := f.q.Publish(f.ctx, m); err != nil {
			f.t.Fatal(err)
		}
		_, _, _ = f.st.Subscriptions.Update(f.ctx, s.ID, func(x *domain.Subscription) error { x.Status = "matched"; return nil })
	}
	return len(subs)
}

// Step 10: a workflow waits for an ingested event; only an event that satisfies the
// match (with the values fixed when the wait started) resumes it.
func TestFlowWaitForEventResumesOnTheMatchingEvent(t *testing.T) {
	f := newFlow(t, flowOpts{})
	idx, err := processor.NewWaitIndex(f.st, f.log)
	if err != nil {
		t.Fatal(err)
	}
	wf := f.workflow("wait-event", `
inputs: [{ name: host, default: "" }]
nodes:
  - id: isolated
    type: wait
    event:
      match: 'event.title == "Host isolated" && wait.host in event.entities.hosts'
      with: { host: 'inputs.host' }
    timeout: 1h
  - { id: after, action: test.count, params: { host: 'steps.isolated.output.event.entities.hosts[0]' } }
edges: [isolated -> after]
`)
	id := f.run(wf, map[string]any{"host": "web01"}, false)
	f.wait(id, func(e domain.Execution) bool { return e.Status == "waiting" }, 10*time.Second)
	sub := f.subscription(id, "isolated")
	if sub.Status != "open" || sub.With["host"] != "web01" || sub.Partition != f.mustExe(id).Partition {
		t.Fatalf("subscription %+v", sub)
	}
	// another host's isolation does not match
	if n := f.deliver(idx, map[string]any{"title": "Host isolated", "entities": map[string]any{"hosts": []any{"db02"}}}); n != 0 {
		t.Fatalf("a non-matching event matched %d waits", n)
	}
	if n := f.deliver(idx, map[string]any{"title": "Host isolated", "entities": map[string]any{"hosts": []any{"web01"}}}); n != 1 {
		t.Fatalf("the matching event matched %d waits", n)
	}
	e := f.done(id)
	if e.Status != "succeeded" {
		t.Fatalf("status %s", e.Status)
	}
	if out, _ := nodeState(t, e, "after").Output.(map[string]any); out["params"].(map[string]any)["host"] != "web01" {
		t.Errorf("the event must be the wait's output: %v", nodeState(t, e, "after").Output)
	}
	if s, _, _ := f.st.Subscriptions.Get(f.ctx, sub.ID); s.Status != "matched" {
		t.Errorf("subscription status %s, want matched", s.Status)
	}
}

// A wait that times out closes its subscription; cancelling closes it too.
func TestFlowWaitForEventTimeoutAndCancelCloseTheSubscription(t *testing.T) {
	f := newFlow(t, flowOpts{})
	wf := f.workflow("wait-timeout", `
nodes:
  - { id: w, type: wait, event: { match: 'event.title == "never"' }, timeout: 1s, on_timeout: continue }
`)
	id := f.run(wf, nil, false)
	e := f.done(id)
	if e.Status != "succeeded" || nodeState(t, e, "w").Output.(map[string]any)["timed_out"] != true {
		t.Fatalf("timeout: %s %v", e.Status, nodeState(t, e, "w").Output)
	}
	f.waitSub(id, "w", "closed")

	wf2 := f.workflow("wait-cancel", `
nodes:
  - { id: w, type: wait, event: { match: 'event.title == "never"' }, timeout: 1h }
`)
	id2 := f.run(wf2, nil, false)
	f.wait(id2, func(e domain.Execution) bool { return e.Status == "waiting" }, 10*time.Second)
	f.subscription(id2, "w")
	m, err := orchestrator.ControlMessage("cancel", "cancel:"+id2, f.mustExe(id2), "", orchestrator.CancelPayload{Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.q.Publish(f.ctx, m); err != nil {
		t.Fatal(err)
	}
	if e := f.done(id2); e.Status != "cancelled" {
		t.Fatalf("cancel: %s", e.Status)
	}
	f.waitSub(id2, "w", "closed")
	open, _ := f.st.Subscriptions.List(context.Background(), store.ListQuery{Terms: map[string][]string{"status": {"open"}}, Limit: 10})
	if len(open.Items) != 0 {
		t.Errorf("open subscriptions left: %+v", open.Items)
	}
}

func (f *flow) mustExe(id string) domain.Execution {
	f.t.Helper()
	e, _, err := f.st.Executions.Get(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

func (f *flow) waitSub(exeID, node, status string) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, _, err := f.st.Subscriptions.Get(f.ctx, engine.SubscriptionID(exeID, node)); err == nil && s.Status == status {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s, _, _ := f.st.Subscriptions.Get(f.ctx, engine.SubscriptionID(exeID, node))
	f.t.Fatalf("subscription %s/%s: status %q, want %q", exeID, node, s.Status, status)
}
