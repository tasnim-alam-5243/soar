// Package orchestrator advances executions (spec section 7):
//
//	pull message -> load execution (with seq/term) -> Engine.Next
//	-> write state and outbox entries in one concurrency-checked write
//	-> carry out the outbox (publish steps, create timers/approvals/child executions)
//	-> mark entries published -> ack
//
// A crash at any point leaves either nothing to do or outbox entries that the
// sweeper (scheduler role) completes; every entry is idempotent (spec 8.4).
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/blob"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/policy"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/worker"
)

// Publisher is the part of the queue client the orchestrator needs.
type Publisher interface {
	Publish(ctx context.Context, m queue.Msg) error
}

// StartPayload is the payload of a "start" envelope.
type StartPayload struct {
	Inputs map[string]any    `json:"inputs,omitempty"`
	Alert  map[string]any    `json:"alert,omitempty"`
	DryRun bool              `json:"dry_run,omitempty"`
	Parent *engine.ParentRef `json:"parent,omitempty"`
}

// TimerPayload, CancelPayload, ApprovalPayload and ChildDonePayload are the payloads
// of the other inbound envelope types.
type TimerPayload struct {
	Kind string `json:"kind"`
}

type CancelPayload struct {
	Actor  string `json:"actor,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type ApprovalPayload struct {
	Approved bool   `json:"approved"`
	Actor    string `json:"actor"`
	Comment  string `json:"comment,omitempty"`
}

type ChildDonePayload struct {
	ChildID string            `json:"child_id"`
	Status  string            `json:"status"`
	Output  any               `json:"output,omitempty"`
	Error   *engine.StepError `json:"error,omitempty"`
}

// Counters for health output and tests.
type Counters struct {
	Handled, Ignored, Conflicts, Published, Failed atomic.Int64
	Held                                           atomic.Int64 // messages held while OpenSearch was unavailable
}

// Orchestrator advances executions.
type Orchestrator struct {
	Q          Publisher
	St         *store.Store
	Registry   *actions.Registry
	Workflows  *WorkflowCache
	Blob       blob.Blob
	Policy     *policy.Engine // nil allows everything
	Log        *slog.Logger
	Partitions int
	Now        func() time.Time
	Stats      Counters
	// HoldMax caps the pause between attempts while OpenSearch is unavailable (default 15 s).
	HoldMax time.Duration
}

func (o *Orchestrator) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}

// ErrRetry asks the caller to redeliver the message later.
var ErrRetry = errors.New("orchestrator: transient failure, retry later")

// Handle applies one inbound envelope. A nil return means "acknowledge".
func (o *Orchestrator) Handle(ctx context.Context, env queue.Envelope) error {
	ev, err := toEvent(env, o.now())
	if err != nil {
		o.Log.Error("undecodable orchestrator message; dropping it", "type", env.Type, "execution_id", env.ExecutionID, "error", err.Error())
		return nil
	}
	for attempt := 0; attempt < 8; attempt++ {
		exe, ver, err := o.St.Executions.Get(ctx, env.ExecutionID)
		if errors.Is(err, store.ErrNotFound) {
			o.Log.Warn("message for an unknown execution; dropping it", "type", env.Type, "execution_id", env.ExecutionID)
			o.Stats.Ignored.Add(1)
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: load execution: %w", ErrRetry, err)
		}
		wf, err := o.Workflows.Get(ctx, exe.WorkflowID, exe.WorkflowChecksum)
		if err != nil {
			if errors.Is(err, errWorkflowGone) {
				return o.failUnloadable(ctx, exe, ver, err)
			}
			return fmt.Errorf("%w: load workflow: %w", ErrRetry, err)
		}
		st, err := engine.StateFromJSON(exe.Context)
		if err != nil {
			return o.failUnloadable(ctx, exe, ver, fmt.Errorf("execution state is corrupt: %w", err))
		}
		if err := o.hydrate(ctx, st); err != nil {
			return fmt.Errorf("%w: load step outputs: %w", ErrRetry, err)
		}
		res := engine.Next(wf, exe.ID, st, ev)
		if res.Invalid != "" {
			// an engine bug; the state is left as it was rather than corrupted
			o.Log.Error("engine refused an illegal state transition", "execution_id", exe.ID, "event", env.Type, "error", res.Invalid)
			obs.OrchestratorEvents.WithLabelValues(env.Type, "invalid").Inc()
			return nil
		}
		if !res.Changed {
			o.Stats.Ignored.Add(1)
			obs.OrchestratorEvents.WithLabelValues(env.Type, "ignored").Inc()
			return nil
		}
		next := o.apply(exe, wf, res)
		nv, err := o.St.Executions.Save(ctx, next, ver)
		if errors.Is(err, store.ErrVersionConflict) {
			o.Stats.Conflicts.Add(1)
			continue // someone else advanced it; reload and recompute
		}
		if err != nil {
			return fmt.Errorf("%w: save execution: %w", ErrRetry, err)
		}
		o.Stats.Handled.Add(1)
		obs.OrchestratorEvents.WithLabelValues(env.Type, "applied").Inc()
		if engine.IsTerminal(next.Status) && !engine.IsTerminal(exe.Status) {
			obs.Executions.WithLabelValues(next.Status, next.WorkflowName).Inc()
			if next.StartedAt != nil && next.EndedAt != nil {
				obs.ExecutionDuration.WithLabelValues(next.WorkflowName).Observe(next.EndedAt.Sub(*next.StartedAt).Seconds())
			}
		}
		o.notify("executions", next.ID, map[string]any{
			"status": next.Status, "workflow_id": next.WorkflowID,
			"workflow_name": next.WorkflowName, "pending_nodes": next.PendingNodes, "error": next.Error,
		})
		if err := o.Flush(ctx, next, nv); err != nil {
			// the state is saved; the sweeper completes the outbox
			o.Log.Warn("outbox not fully carried out; the sweeper will finish it", "execution_id", exe.ID, "error", err.Error())
		}
		return nil
	}
	return fmt.Errorf("%w: too many concurrent updates of %s", ErrRetry, env.ExecutionID)
}

func (o *Orchestrator) failUnloadable(ctx context.Context, exe domain.Execution, ver store.Version, cause error) error {
	if engine.IsTerminal(exe.Status) {
		return nil
	}
	now := o.now()
	exe.Status, exe.Error, exe.EndedAt, exe.PendingNodes = engine.StatusFailed, cause.Error(), &now, nil
	if _, err := o.St.Executions.Save(ctx, exe, ver); err != nil && !errors.Is(err, store.ErrVersionConflict) {
		return fmt.Errorf("%w: %w", ErrRetry, err)
	}
	o.Log.Error("execution failed: its workflow cannot be loaded", "execution_id", exe.ID, "error", cause.Error())
	return nil
}

// apply copies the engine result into the execution document and appends outbox entries.
func (o *Orchestrator) apply(exe domain.Execution, wf *engine.Workflow, res engine.Result) domain.Execution {
	st := res.State
	now := o.now()
	// large outputs live in object storage; keep only the reference in the document
	for _, ns := range st.Nodes {
		if ns.OutputRef != "" {
			ns.Output = nil
		}
	}
	exe.Context = st.JSON()
	exe.Status = st.Status
	exe.Error = st.Error
	exe.StartedAt, exe.EndedAt = st.StartedAt, st.EndedAt
	if exe.DeadlineAt == nil && st.StartedAt != nil {
		dl := st.StartedAt.Add(wf.Deadline)
		exe.DeadlineAt = &dl
	}
	exe.DryRun = st.DryRun
	exe.PendingNodes = st.ActiveNodes()
	sort.Strings(exe.PendingNodes)
	var kept []domain.OutboxEntry
	for _, e := range exe.Outbox {
		if !e.Published {
			kept = append(kept, e)
		}
	}
	for _, c := range res.Commands {
		var payload domain.JSON
		b, _ := json.Marshal(c)
		_ = json.Unmarshal(b, &payload)
		kept = append(kept, domain.OutboxEntry{Key: c.Key, Kind: c.Kind, Payload: payload, CreatedAt: now})
	}
	exe.Outbox = kept
	exe.OutboxPending = len(kept) > 0
	return exe
}

// hydrate loads outputs that were too large to keep in the execution document.
func (o *Orchestrator) hydrate(ctx context.Context, st *engine.State) error {
	for _, ns := range st.Nodes {
		if ns.OutputRef != "" && ns.Output == nil {
			v, err := blob.Load(ctx, o.Blob, ns.OutputRef)
			if err != nil {
				return err
			}
			ns.Output = v
		}
	}
	return nil
}

// Flush carries out the unpublished outbox entries of exe and marks them published.
// It is idempotent and safe to run concurrently with itself (the sweeper uses it).
func (o *Orchestrator) Flush(ctx context.Context, exe domain.Execution, _ store.Version) error {
	var done []string
	var firstErr error
	for _, e := range exe.Outbox {
		if e.Published {
			continue
		}
		if err := o.carryOut(ctx, exe, e); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s %s: %w", e.Kind, e.Key, err)
			}
			continue
		}
		done = append(done, e.Key)
		o.Stats.Published.Add(1)
	}
	if engine.IsTerminal(exe.Status) {
		o.closeApprovals(ctx, exe)
	}
	if len(done) > 0 {
		doneSet := map[string]bool{}
		for _, k := range done {
			doneSet[k] = true
		}
		now := o.now()
		_, _, err := o.St.Executions.Update(ctx, exe.ID, func(x *domain.Execution) error {
			var kept []domain.OutboxEntry
			for _, e := range x.Outbox {
				if doneSet[e.Key] {
					continue // published: drop it to keep the document small
				}
				kept = append(kept, e)
			}
			x.Outbox, x.OutboxPending = kept, len(kept) > 0
			x.UpdatedAt = now
			return nil
		})
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("mark outbox published: %w", err)
		}
	}
	return firstErr
}

func decodeCmd(e domain.OutboxEntry) (engine.Command, error) {
	var c engine.Command
	b, err := json.Marshal(e.Payload)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func (o *Orchestrator) carryOut(ctx context.Context, exe domain.Execution, e domain.OutboxEntry) error {
	c, err := decodeCmd(e)
	if err != nil {
		return err
	}
	switch c.Kind {
	case engine.CmdDispatch:
		return o.dispatch(ctx, exe, c)
	case engine.CmdTimer:
		t := c.Timer
		doc := domain.Timer{
			Meta: domain.Meta{ID: t.ID}, ExecutionID: t.ExecutionID, NodeID: t.NodeID, DueAt: t.DueAt,
			Kind: t.Kind, Attempt: t.Attempt, Item: t.Item, Partition: exe.Partition,
		}
		if _, err := o.St.Timers.Create(ctx, doc); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return err
		}
		return nil
	case engine.CmdApproval:
		a := c.Approval
		doc := domain.Approval{
			Meta: domain.Meta{ID: a.ID}, ExecutionID: a.ExecutionID, NodeID: a.NodeID, WorkflowID: exe.WorkflowID,
			WorkflowName: exe.WorkflowName, Partition: exe.Partition, Approvers: a.Approvers, Message: a.Message, Status: "pending", DueAt: a.DueAt,
		}
		if _, err := o.St.Approvals.Create(ctx, doc); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return err
		}
		o.notify("approvals", doc.ID, map[string]any{"execution_id": doc.ExecutionID, "status": doc.Status})
		return nil
	case engine.CmdStartChild:
		return o.startChild(ctx, exe, c)
	case engine.CmdNotifyParent:
		n := c.Parent
		env, err := queue.NewEnvelope(queue.MsgChildDone, c.Key, n.Parent.ExecutionID, n.Parent.Partition,
			ChildDonePayload{ChildID: n.ChildID, Status: n.Status, Output: n.Output, Error: n.Error})
		if err != nil {
			return err
		}
		env.NodeID = n.Parent.NodeID
		return o.publish(ctx, env, queue.ResultSubject(n.Parent.Partition))
	}
	return fmt.Errorf("unknown outbox kind %q", c.Kind)
}

func (o *Orchestrator) publish(ctx context.Context, env queue.Envelope, subject string) error {
	m, err := env.Msg(subject)
	if err != nil {
		return err
	}
	return o.Q.Publish(ctx, m)
}

func (o *Orchestrator) dispatch(ctx context.Context, exe domain.Execution, c engine.Command) error {
	if o.Policy != nil && o.Registry != nil {
		if a, ok := o.Registry.Get(c.Dispatch.Action); ok {
			man := a.Manifest()
			d := c.Dispatch
			dec := o.Policy.Decide(policy.Request{
				ActorType: "workflow", Actor: "workflow:" + exe.WorkflowName, Action: man.ID,
				SideEffect: man.SideEffect, Risk: man.Risk, Args: d.Params, DryRun: d.DryRun,
				PreApproved: o.preApproved(ctx, exe, d.NodeID), ApprovalPolicy: man.ApprovalPolicy,
			})
			obs.PolicyDecisions.WithLabelValues(dec.Effect, man.ID).Inc()
			switch dec.Effect {
			case policy.Deny:
				m, err := StepResultMessage(exe, c, worker.ResultPayload{
					Status: engine.NodeFailed,
					Error:  &engine.StepError{Code: "denied_by_policy", Message: dec.Reason},
				})
				if err != nil {
					return err
				}
				return o.Q.Publish(ctx, m)
			case policy.RequireApproval:
				return o.requestPolicyApproval(ctx, exe, c, man, dec.Reason)
			}
		}
	}
	m, err := DispatchMessage(exe, c, o.Registry)
	if err != nil {
		return err
	}
	return o.Q.Publish(ctx, m)
}

// preApproved reports whether an approval node directly before nodeID was approved,
// which satisfies the "destructive actions need approval" policy.
func (o *Orchestrator) preApproved(ctx context.Context, exe domain.Execution, nodeID string) bool {
	wf, err := o.Workflows.Get(ctx, exe.WorkflowID, exe.WorkflowChecksum)
	if err != nil {
		return false
	}
	st, err := engine.StateFromJSON(exe.Context)
	if err != nil {
		return false
	}
	for _, p := range wf.Preds(nodeID) {
		n, ns := wf.Node(p), st.Nodes[p]
		if n == nil || ns == nil || n.Type != engine.TypeApproval || ns.Status != engine.NodeSucceeded {
			continue
		}
		if out, ok := ns.Output.(map[string]any); ok && out["approved"] == true {
			return true
		}
	}
	return false
}

// DispatchMessage builds the step message for a dispatch command: subject
// steps.<priority>.<class>, Nats-Msg-Id = the step key.
func DispatchMessage(exe domain.Execution, c engine.Command, reg *actions.Registry) (queue.Msg, error) {
	d := c.Dispatch
	class, prio := actions.ClassBuiltin, d.Priority
	if prio == "" {
		prio = "normal"
	}
	if reg != nil {
		if a, ok := reg.Get(d.Action); ok {
			class = worker.ClassOf(a.Manifest())
		}
	}
	env, err := queue.NewEnvelope(queue.MsgStep, c.Key, exe.ID, exe.Partition, worker.StepPayload{Dispatch: *d, WorkflowID: exe.WorkflowID, WorkflowName: exe.WorkflowName, AlertID: exe.Trigger.AlertID})
	if err != nil {
		return queue.Msg{}, err
	}
	env.NodeID, env.Attempt, env.Item, env.IdempotencyKey = d.NodeID, d.Attempt, d.Item, d.IdempotencyKey
	return env.Msg(queue.StepSubject(prio, class))
}

// StepResultMessage answers a step without running it (a policy denial or rejection).
func StepResultMessage(exe domain.Execution, c engine.Command, res worker.ResultPayload) (queue.Msg, error) {
	d := c.Dispatch
	env, err := queue.NewEnvelope(queue.MsgStepResult, "res:"+c.Key, exe.ID, exe.Partition, res)
	if err != nil {
		return queue.Msg{}, err
	}
	env.NodeID, env.Attempt, env.Item = d.NodeID, d.Attempt, d.Item
	return env.Msg(queue.ResultSubject(exe.Partition))
}

// DecodeCommand turns a stored command (outbox entry or approval) back into a Command.
func DecodeCommand(m domain.JSON) (engine.Command, error) {
	return decodeCmd(domain.OutboxEntry{Payload: m})
}

func (o *Orchestrator) startChild(ctx context.Context, exe domain.Execution, c engine.Command) error {
	cs := c.Child
	parent := cs.Parent
	parent.Partition = exe.Partition
	wdoc, err := o.Workflows.LatestPublished(ctx, cs.Workflow)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			env, eerr := queue.NewEnvelope(queue.MsgChildDone, c.Key+":missing", exe.ID, exe.Partition, ChildDonePayload{
				ChildID: cs.ExecutionID,
				Status:  engine.StatusFailed, Error: &engine.StepError{Code: "workflow_not_found", Message: "no published workflow named " + cs.Workflow},
			})
			if eerr != nil {
				return eerr
			}
			env.NodeID = parent.NodeID
			return o.publish(ctx, env, queue.ResultSubject(exe.Partition))
		}
		return err
	}
	child := NewExecution(wdoc, cs.ExecutionID, domain.ExecutionTrigger{
		Type: "subworkflow", AlertID: exe.Trigger.AlertID,
		ParentExecutionID: exe.ID, ParentNodeID: parent.NodeID,
	}, o.Partitions, cs.DryRun)
	if _, err := o.St.Executions.Create(ctx, child); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	var alert map[string]any
	if st, err := engine.StateFromJSON(exe.Context); err == nil {
		alert = st.Alert
	}
	m, err := StartMessage(child, StartPayload{Inputs: cs.Inputs, Alert: alert, DryRun: cs.DryRun, Parent: &parent})
	if err != nil {
		return err
	}
	return o.Q.Publish(ctx, m)
}

// requestPolicyApproval turns a dispatch that policy holds back into an approval
// request; the decision is delivered as the step result (approved: the step is then
// dispatched by the API, rejected: the step fails).
func (o *Orchestrator) requestPolicyApproval(ctx context.Context, exe domain.Execution, c engine.Command, man actions.Manifest, reason string) error {
	d := c.Dispatch
	id := domain.DeterministicID("apr", exe.ID, c.Key)
	payload, _ := json.Marshal(c)
	var cmd domain.JSON
	_ = json.Unmarshal(payload, &cmd)
	doc := domain.Approval{
		Meta: domain.Meta{ID: id}, ExecutionID: exe.ID, NodeID: d.NodeID, WorkflowID: exe.WorkflowID,
		WorkflowName: exe.WorkflowName, Partition: exe.Partition, Approvers: engine.DefaultApprovers, Action: man.ID, Risk: man.Risk, Command: cmd,
		Message: fmt.Sprintf("%s wants to run %s (%s, risk %s): %s", exe.WorkflowName, man.ID, man.SideEffect, man.Risk, reason),
		Status:  "pending", DueAt: o.now().Add(engine.DefaultApproval),
	}
	if _, err := o.St.Approvals.Create(ctx, doc); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	return nil
}

// closeApprovals marks approvals of a finished execution as cancelled.
func (o *Orchestrator) closeApprovals(ctx context.Context, exe domain.Execution) {
	page, err := o.St.Approvals.List(ctx, store.ListQuery{Terms: map[string][]string{"execution_id": {exe.ID}, "status": {"pending"}}, Limit: 200})
	if err != nil {
		return
	}
	for _, a := range page.Items {
		_, _, _ = o.St.Approvals.Update(ctx, a.ID, func(x *domain.Approval) error {
			if x.Status == "pending" {
				x.Status = "cancelled"
				x.Comment = "the execution ended (" + exe.Status + ") before a decision"
			}
			return nil
		})
	}
}

// notify tells connected UIs about a change (best effort, core NATS ui.<topic>).
func (o *Orchestrator) notify(topic, id string, data map[string]any) {
	if n, ok := o.Q.(interface{ Notify(string, any) }); ok {
		n.Notify("ui."+topic, map[string]any{"topic": topic, "id": id, "data": data, "at": o.now()})
	}
}

// ---- helpers shared with the processor, API and scheduler ----

// NewExecution builds a queued execution of a workflow version.
func NewExecution(w domain.Workflow, id string, trig domain.ExecutionTrigger, partitions int, dryRun bool) domain.Execution {
	now := time.Now().UTC()
	if id == "" {
		id = domain.NewID(domain.PrefixExecution)
	}
	return domain.Execution{
		Meta:       domain.Meta{ID: id, CreatedAt: now, UpdatedAt: now, SchemaVersion: domain.SchemaVersion},
		WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: w.Version, WorkflowChecksum: w.Checksum,
		Status: engine.StatusQueued, Trigger: trig, Partition: queue.Partition(id, partitions), DryRun: dryRun,
	}
}

// StartMessage builds the start message of an execution (Nats-Msg-Id = "start:" + id).
func StartMessage(exe domain.Execution, p StartPayload) (queue.Msg, error) {
	env, err := queue.NewEnvelope(queue.MsgStart, "start:"+exe.ID, exe.ID, exe.Partition, p)
	if err != nil {
		return queue.Msg{}, err
	}
	return env.Msg(queue.ResultSubject(exe.Partition))
}

// ControlMessage builds a cancel or approval message for an execution.
func ControlMessage(typ, id string, exe domain.Execution, nodeID string, payload any) (queue.Msg, error) {
	env, err := queue.NewEnvelope(typ, id, exe.ID, exe.Partition, payload)
	if err != nil {
		return queue.Msg{}, err
	}
	env.NodeID = nodeID
	return env.Msg(queue.ResultSubject(exe.Partition))
}

// TimerMessage builds the message that fires a timer (Nats-Msg-Id = timer id).
func TimerMessage(t domain.Timer) (queue.Msg, error) {
	env, err := queue.NewEnvelope(queue.MsgTimer, t.ID, t.ExecutionID, t.Partition, TimerPayload{Kind: t.Kind})
	if err != nil {
		return queue.Msg{}, err
	}
	env.NodeID, env.Attempt, env.Item = t.NodeID, t.Attempt, t.Item
	return env.Msg(queue.ResultSubject(t.Partition))
}

func toEvent(env queue.Envelope, now time.Time) (engine.Event, error) {
	ev := engine.Event{At: now, NodeID: env.NodeID, Attempt: env.Attempt, Item: env.Item}
	dec := func(v any) error {
		if len(env.Payload) == 0 {
			return nil
		}
		return json.Unmarshal(env.Payload, v)
	}
	switch env.Type {
	case queue.MsgStart:
		var p StartPayload
		if err := dec(&p); err != nil {
			return ev, err
		}
		ev.Type, ev.Inputs, ev.Alert, ev.DryRun, ev.Parent = engine.EvStart, p.Inputs, p.Alert, p.DryRun, p.Parent
	case queue.MsgStepResult:
		var p worker.ResultPayload
		if err := dec(&p); err != nil {
			return ev, err
		}
		ev.Type, ev.Status, ev.Output, ev.OutputRef, ev.Error = engine.EvStepResult, p.Status, p.Output, p.OutputRef, p.Error
	case queue.MsgTimer:
		var p TimerPayload
		if err := dec(&p); err != nil {
			return ev, err
		}
		ev.Type, ev.TimerKind = engine.EvTimer, p.Kind
	case queue.MsgCancel:
		var p CancelPayload
		if err := dec(&p); err != nil {
			return ev, err
		}
		ev.Type, ev.Actor, ev.Reason = engine.EvCancel, p.Actor, p.Reason
	case queue.MsgApproval:
		var p ApprovalPayload
		if err := dec(&p); err != nil {
			return ev, err
		}
		ev.Type, ev.Approved, ev.Actor, ev.Comment = engine.EvApproval, p.Approved, p.Actor, p.Comment
	case queue.MsgChildDone:
		var p ChildDonePayload
		if err := dec(&p); err != nil {
			return ev, err
		}
		ev.Type, ev.ChildID, ev.Status, ev.Output, ev.Error = engine.EvChildDone, p.ChildID, p.Status, p.Output, p.Error
	default:
		return ev, fmt.Errorf("unknown message type %q", env.Type)
	}
	return ev, nil
}

// ---- workflow cache ----

var errWorkflowGone = errors.New("workflow version no longer exists or changed")

// WorkflowCache compiles workflow versions once. Published versions are immutable,
// so the cache key (id, checksum) never goes stale.
type WorkflowCache struct {
	St *store.Store
	mu sync.RWMutex
	m  map[string]*engine.Workflow
}

// NewWorkflowCache returns an empty cache.
func NewWorkflowCache(st *store.Store) *WorkflowCache {
	return &WorkflowCache{St: st, m: map[string]*engine.Workflow{}}
}

// Get returns the compiled workflow version. checksum "" accepts any content.
func (c *WorkflowCache) Get(ctx context.Context, id, checksum string) (*engine.Workflow, error) {
	key := id + "@" + checksum
	c.mu.RLock()
	w, ok := c.m[key]
	c.mu.RUnlock()
	if ok {
		return w, nil
	}
	doc, _, err := c.St.Workflows.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", errWorkflowGone, id)
	}
	if err != nil {
		return nil, err
	}
	if checksum != "" && doc.Checksum != checksum {
		return nil, fmt.Errorf("%w: draft %s was edited after this execution started", errWorkflowGone, id)
	}
	w, errs := engine.Compile(doc.Definition, nil)
	if len(errs) > 0 {
		return nil, fmt.Errorf("%w: %s does not compile: %s %s", errWorkflowGone, id, errs[0].Path, errs[0].Message)
	}
	w.ID, w.Name, w.Version = doc.ID, doc.Name, doc.Version
	c.mu.Lock()
	c.m[key] = w
	c.mu.Unlock()
	return w, nil
}

// LatestPublished returns the newest published version of a workflow name.
func (c *WorkflowCache) LatestPublished(ctx context.Context, name string) (domain.Workflow, error) {
	page, err := c.St.Workflows.List(ctx, store.ListQuery{
		Terms: map[string][]string{"name.raw": {name}, "status": {"published"}},
		Sort:  "version", Desc: true, Limit: 1,
	})
	if err != nil {
		return domain.Workflow{}, err
	}
	if len(page.Items) == 0 {
		return domain.Workflow{}, store.ErrNotFound
	}
	return page.Items[0], nil
}
