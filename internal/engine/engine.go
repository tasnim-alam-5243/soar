package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/yourorg/soar/internal/expr"
)

// Next applies one event to an execution and returns the new state and the side
// effects to carry out. It never mutates st.
//
// Flow rules: a node becomes decidable once every predecessor is finished. It runs
// if at least one predecessor activated it (a succeeded node activates its
// successors, a branch only the chosen case, a failed node its on_error targets),
// otherwise it is skipped and activates nothing (dead-path elimination). This makes
// joins after branches and parallel fan-outs behave without special cases.
func Next(w *Workflow, execID string, st *State, ev Event) Result {
	r := &run{w: w, execID: execID, st: st.Clone(), ev: ev}
	if r.st.Nodes == nil {
		r.st.Nodes = map[string]*NodeState{}
	}
	if r.st.Vars == nil {
		r.st.Vars = map[string]any{} // empty maps do not survive the JSON round trip
	}
	switch ev.Type {
	case EvStart:
		r.start()
	case EvStepResult:
		r.stepResult()
	case EvTimer:
		r.timer()
	case EvApproval:
		r.approval()
	case EvChildDone:
		r.childDone()
	case EvCancel:
		if !r.terminal() && r.st.Status != "" {
			r.changed = true
			reason := ev.Reason
			if reason == "" {
				reason = "cancelled"
			}
			if ev.Actor != "" {
				reason += " by " + ev.Actor
			}
			r.finish(StatusCancelled, reason)
		}
	}
	if !r.changed {
		return Result{State: st.Clone(), Changed: false}
	}
	if !ValidTransition(st.Status, r.st.Status) {
		return Result{
			State: st.Clone(), Changed: false,
			Invalid: fmt.Sprintf("illegal transition %q -> %q on %s event", st.Status, r.st.Status, ev.Type),
		}
	}
	return Result{State: r.st, Commands: r.cmds, Changed: true}
}

type run struct {
	w       *Workflow
	execID  string
	st      *State
	ev      Event
	cmds    []Command
	changed bool
}

func (r *run) at() *time.Time { t := r.ev.At.UTC(); return &t }

func (r *run) terminal() bool { return IsTerminal(r.st.Status) }

// ---- events ----

func (r *run) start() {
	if r.st.Status != "" && r.st.Status != StatusQueued {
		return // duplicate start
	}
	r.changed = true
	r.st.Status = StatusRunning
	r.st.StartedAt = r.at()
	r.st.Alert = normMap(r.ev.Alert)
	r.st.DryRun = r.ev.DryRun
	r.st.Parent = r.ev.Parent
	r.st.Vars = map[string]any{}

	inputs := map[string]any{}
	for k, v := range r.ev.Inputs {
		inputs[k] = v
	}
	r.st.Inputs = normMap(inputs)
	for _, in := range r.w.Inputs {
		if _, given := inputs[in.Name]; given {
			continue
		}
		var val any
		if in.From != nil {
			if v, err := in.From.Eval(r.vars(0, nil)); err == nil {
				val = v
			}
		}
		if val == nil {
			val = in.Default
		}
		if val == nil && in.Required {
			r.finish(StatusFailed, fmt.Sprintf("required input %q has no value", in.Name))
			return
		}
		inputs[in.Name] = val
		r.st.Inputs = normMap(inputs) // later inputs may read earlier ones
	}
	r.timerCmd(TimerDeadline, "", 0, 0, r.ev.At.Add(r.w.Deadline))
	r.advance()
}

func (r *run) stepResult() {
	n, ns := r.w.Node(r.ev.NodeID), r.st.Nodes[r.ev.NodeID]
	if r.terminal() || n == nil || ns == nil {
		return
	}
	if n.Type == TypeForeach {
		r.itemResult(n, ns)
		return
	}
	if n.Type != TypeAction || ns.Status != NodeRunning || ns.Attempt != r.ev.Attempt {
		return // duplicate or stale
	}
	r.changed = true
	if r.ev.Status == NodeSucceeded {
		ns.Output, ns.OutputRef, ns.Error = norm(r.ev.Output), r.ev.OutputRef, nil
		r.succeed(ns, n.Next)
		r.advance()
		return
	}
	e := r.ev.Error
	if e == nil {
		e = &StepError{Code: "unknown", Message: "step failed without an error"}
	}
	if r.shouldRetry(n, e, ns.Attempt) {
		ns.Status, ns.Error = NodeRetryWait, e
		r.timerCmd(TimerRetry, n.ID, 0, ns.Attempt, r.ev.At.Add(Backoff(n.Retry, r.execID, n.ID, 0, ns.Attempt)))
		r.settle()
		return
	}
	r.nodeFailed(n, ns, e)
	r.advance()
}

func (r *run) timer() {
	if r.terminal() || r.st.Status == "" {
		return
	}
	if r.ev.TimerKind == TimerDeadline {
		r.changed = true
		r.finish(StatusTimedOut, "execution deadline exceeded")
		return
	}
	n, ns := r.w.Node(r.ev.NodeID), r.st.Nodes[r.ev.NodeID]
	if n == nil || ns == nil {
		return
	}
	switch r.ev.TimerKind {
	case TimerRetry:
		if n.Type == TypeForeach {
			i := r.ev.Item - 1
			if ns.Status != NodeRunning || i < 0 || i >= len(ns.ItemStates) {
				return
			}
			is := ns.ItemStates[i]
			if is == nil || is.Status != NodeRetryWait || is.Attempt != r.ev.Attempt {
				return
			}
			r.changed = true
			is.Attempt++
			is.Status = NodeRunning
			r.dispatch(n, ns, r.ev.Item, is.Attempt)
			r.fillForeach(n, ns) // completes the node if the dispatch itself failed
			r.advance()
			return
		}
		if ns.Status != NodeRetryWait || ns.Attempt != r.ev.Attempt {
			return
		}
		r.changed = true
		ns.Attempt++
		ns.Status = NodeRunning
		r.dispatch(n, ns, 0, ns.Attempt)
		r.advance()
	case TimerWait:
		if n.Type != TypeWait || ns.Status != NodeWaiting {
			return
		}
		r.changed = true
		ns.Output = map[string]any{"waited_until": r.ev.At.UTC().Format(time.RFC3339Nano)}
		r.succeed(ns, n.Next)
		r.advance()
	case TimerApproval:
		if n.Type != TypeApproval || ns.Status != NodeWaiting {
			return
		}
		r.changed = true
		r.decide(n, ns, n.OnTimeout == "approve", "timeout", "no decision before the approval timeout")
		r.advance()
	}
}

func (r *run) approval() {
	n, ns := r.w.Node(r.ev.NodeID), r.st.Nodes[r.ev.NodeID]
	if r.terminal() || n == nil || ns == nil || n.Type != TypeApproval || ns.Status != NodeWaiting {
		return
	}
	r.changed = true
	r.decide(n, ns, r.ev.Approved, r.ev.Actor, r.ev.Comment)
	r.advance()
}

func (r *run) decide(n *Node, ns *NodeState, approved bool, actor, comment string) {
	ns.Output = map[string]any{
		"approved": approved, "decided_by": actor, "comment": comment,
		"decided_at": r.ev.At.UTC().Format(time.RFC3339Nano),
	}
	switch {
	case approved:
		r.succeed(ns, n.Next)
	case len(n.OnReject) > 0:
		r.succeed(ns, n.OnReject)
	default:
		code := "approval_rejected"
		if actor == "timeout" {
			code = "approval_timeout"
		}
		r.nodeFailed(n, ns, &StepError{Code: code, Message: fmt.Sprintf("not approved (%s): %s", actor, comment)})
	}
}

func (r *run) childDone() {
	n, ns := r.w.Node(r.ev.NodeID), r.st.Nodes[r.ev.NodeID]
	if r.terminal() || n == nil || ns == nil || n.Type != TypeSubworkflow || ns.Status != NodeWaiting || ns.ChildID != r.ev.ChildID {
		return
	}
	r.changed = true
	ns.Output = norm(r.ev.Output)
	if r.ev.Status == StatusSucceeded {
		r.succeed(ns, n.Next)
	} else {
		e := r.ev.Error
		if e == nil {
			e = &StepError{Code: "child_" + r.ev.Status, Message: "subworkflow ended " + r.ev.Status}
		}
		r.nodeFailed(n, ns, e)
	}
	r.advance()
}

// ---- flow ----

func (r *run) advance() {
	for !r.terminal() {
		progressed := false
		for _, n := range r.w.Nodes {
			if r.terminal() {
				return
			}
			if r.st.Nodes[n.ID] != nil {
				continue
			}
			preds := r.w.preds[n.ID]
			fired, ready := len(preds) == 0, true
			for _, p := range preds {
				ps := r.st.Nodes[p]
				if ps == nil || !isTerminalNode(ps.Status) {
					ready = false
					break
				}
				if contains(ps.Fired, n.ID) {
					fired = true
				}
			}
			if !ready {
				continue
			}
			progressed = true
			if !fired {
				r.st.Nodes[n.ID] = &NodeState{Status: NodeSkipped, EndedAt: r.at()}
				continue
			}
			r.activate(n)
		}
		if !progressed {
			break
		}
	}
	r.settle()
}

// settle derives the execution status from the node states.
func (r *run) settle() {
	if r.terminal() {
		return
	}
	running, waiting := false, false
	for _, n := range r.w.Nodes {
		ns := r.st.Nodes[n.ID]
		if ns == nil {
			continue
		}
		switch ns.Status {
		case NodeRunning, NodeRetryWait:
			running = true
		case NodeWaiting:
			waiting = true
		}
	}
	switch {
	case running:
		r.st.Status = StatusRunning
	case waiting:
		r.st.Status = StatusWaiting
	default:
		// nothing in progress: every node is finished (in a DAG an unfinished node always
		// has an unfinished predecessor, which would be in progress)
		r.finish(StatusSucceeded, "")
	}
}

func (r *run) activate(n *Node) {
	ns := &NodeState{Status: NodeRunning, StartedAt: r.at()}
	r.st.Nodes[n.ID] = ns
	if n.When != nil {
		ok, err := n.When.Bool(r.vars(0, nil))
		if err != nil {
			r.nodeFailed(n, ns, exprError("when", err))
			return
		}
		if !ok {
			ns.Status, ns.Passed, ns.EndedAt = NodeSkipped, true, r.at()
			ns.Fired = passTargets(n)
			return
		}
	}
	switch n.Type {
	case TypeAction:
		ns.Attempt = 1
		r.dispatch(n, ns, 0, 1)
	case TypeForeach:
		r.startForeach(n, ns)
	case TypeBranch:
		vars := r.vars(0, nil)
		for i, cs := range n.Cases {
			ok, err := cs.When.Bool(vars)
			if err != nil {
				r.nodeFailed(n, ns, exprError(fmt.Sprintf("cases[%d].when", i), err))
				return
			}
			if ok {
				ns.Output = map[string]any{"case": float64(i), "next": toAny(cs.Next)}
				r.succeed(ns, cs.Next)
				return
			}
		}
		ns.Output = map[string]any{"case": "default", "next": toAny(n.Default)}
		r.succeed(ns, n.Default)
	case TypeParallel:
		r.succeed(ns, passTargets(n))
	case TypeSet:
		assigned := map[string]any{}
		for _, a := range n.Assign {
			v, err := a.Value.Eval(r.vars(0, nil))
			if err != nil {
				r.nodeFailed(n, ns, exprError("vars."+a.Name, err))
				return
			}
			v = norm(v)
			r.st.Vars[a.Name] = v
			assigned[a.Name] = v
		}
		ns.Output = assigned
		r.succeed(ns, n.Next)
	case TypeWait:
		due := r.ev.At.Add(n.Duration)
		if n.Until != nil {
			v, err := n.Until.Eval(r.vars(0, nil))
			if err != nil {
				r.nodeFailed(n, ns, exprError("until", err))
				return
			}
			t, err := time.Parse(time.RFC3339Nano, expr.Text(v))
			if err != nil {
				r.nodeFailed(n, ns, &StepError{Code: "expression_error", Message: fmt.Sprintf("until must return an RFC 3339 time, got %v", v)})
				return
			}
			if t.Before(r.ev.At) {
				t = r.ev.At
			}
			due = t
		}
		ns.Status = NodeWaiting
		r.timerCmd(TimerWait, n.ID, 0, 0, due)
	case TypeApproval:
		msg := ""
		if n.Message != nil {
			v, err := n.Message.Eval(r.vars(0, nil))
			if err != nil {
				r.nodeFailed(n, ns, exprError("message", err))
				return
			}
			msg = expr.Text(v)
		}
		ns.Status = NodeWaiting
		ns.ApprovalID = ApprovalID(r.execID, n.ID)
		due := r.ev.At.Add(n.ApprovalTimeout)
		r.cmds = append(r.cmds, Command{Kind: CmdApproval, Key: "approval:" + ns.ApprovalID, Approval: &ApprovalRequest{
			ID: ns.ApprovalID, ExecutionID: r.execID, NodeID: n.ID, Approvers: n.Approvers, Message: msg, DueAt: due.UTC(),
		}})
		r.timerCmd(TimerApproval, n.ID, 0, 0, due)
	case TypeSubworkflow:
		inputs := map[string]any{}
		if n.SubInputs != nil {
			v, err := n.SubInputs.Eval(r.vars(0, nil))
			if err != nil {
				r.nodeFailed(n, ns, exprError("inputs", err))
				return
			}
			inputs, _ = norm(v).(map[string]any)
		}
		ns.Status = NodeWaiting
		ns.ChildID = ChildID(r.execID, n.ID)
		r.cmds = append(r.cmds, Command{Kind: CmdStartChild, Key: "child:" + ns.ChildID, Child: &ChildStart{
			ExecutionID: ns.ChildID, Workflow: n.Workflow, Inputs: inputs,
			Parent: ParentRef{ExecutionID: r.execID, NodeID: n.ID}, DryRun: r.st.DryRun,
		}})
	case TypeEnd:
		var out any
		if n.EndOutput != nil {
			v, err := n.EndOutput.Eval(r.vars(0, nil))
			if err != nil {
				r.nodeFailed(n, ns, exprError("output", err))
				return
			}
			out = norm(v)
		}
		ns.Output = out
		r.succeed(ns, nil)
		r.st.Output = out
		msg := ""
		if n.EndStatus == StatusFailed {
			msg = "ended by node " + n.ID
		}
		r.finish(n.EndStatus, msg)
	}
}

func (r *run) succeed(ns *NodeState, fired []string) {
	ns.Status, ns.Fired, ns.EndedAt = NodeSucceeded, fired, r.at()
	ns.Error = nil
}

func (r *run) nodeFailed(n *Node, ns *NodeState, e *StepError) {
	ns.Status, ns.Error, ns.EndedAt = NodeFailed, e, r.at()
	switch {
	case len(n.OnError) > 0:
		ns.Fired = n.OnError
	case n.ContinueOnError:
		ns.Fired = passTargets(n)
	default:
		r.finish(StatusFailed, fmt.Sprintf("node %s failed: %s", n.ID, errText(e)))
	}
}

// finish ends the execution. Unfinished nodes are cancelled; late step results for
// them are ignored.
func (r *run) finish(status, msg string) {
	if r.terminal() {
		return
	}
	if status == StatusSucceeded && r.w.Outputs != nil && r.st.Output == nil {
		v, err := r.w.Outputs.Eval(r.vars(0, nil))
		if err != nil {
			status, msg = StatusFailed, "outputs: "+err.Error()
		} else {
			r.st.Output = norm(v)
		}
	}
	r.st.Status, r.st.Error, r.st.EndedAt = status, msg, r.at()
	for _, n := range r.w.Nodes {
		if ns := r.st.Nodes[n.ID]; ns != nil && !isTerminalNode(ns.Status) {
			ns.Status, ns.EndedAt = NodeCancelled, r.at()
		}
	}
	if p := r.st.Parent; p != nil {
		notice := &ParentNotice{Parent: *p, ChildID: r.execID, Status: status, Output: r.st.Output}
		if status != StatusSucceeded {
			notice.Error = &StepError{Code: "child_" + status, Message: msg}
		}
		r.cmds = append(r.cmds, Command{Kind: CmdNotifyParent, Key: "child-done:" + r.execID, Parent: notice})
	}
}

// ---- actions and foreach ----

func (r *run) dispatch(n *Node, ns *NodeState, item, attempt int) {
	var itemVal any
	if item > 0 {
		itemVal = ns.Items[item-1]
	}
	vars := r.vars(item, itemVal)
	fail := func(e *StepError) {
		if item > 0 {
			is := ns.ItemStates[item-1]
			is.Status, is.Error = NodeFailed, e
			return
		}
		r.nodeFailed(n, ns, e)
	}
	raw, err := n.Params.Eval(vars)
	if err != nil {
		fail(exprError("params", err))
		return
	}
	params, _ := norm(raw).(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	key := StepKey(r.execID, n.ID, item, attempt)
	idem := key
	if n.IdemKey != nil {
		v, err := n.IdemKey.Eval(vars)
		if err != nil || v == nil {
			if err == nil {
				err = errors.New("evaluated to null")
			}
			fail(exprError("idempotency_key", err))
			return
		}
		idem = "key:" + expr.Text(v)
	}
	r.cmds = append(r.cmds, Command{Kind: CmdDispatch, Key: key, Dispatch: &Dispatch{
		ExecutionID: r.execID, NodeID: n.ID, Action: n.Action, Attempt: attempt, Item: item, Params: params,
		TimeoutMS: n.Timeout.Milliseconds(), Priority: n.Priority, IdempotencyKey: idem, DryRun: r.st.DryRun,
	}})
}

func (r *run) startForeach(n *Node, ns *NodeState) {
	v, err := n.Items.Eval(r.vars(0, nil))
	if err != nil {
		r.nodeFailed(n, ns, exprError("items", err))
		return
	}
	if v == nil {
		v = []any{}
	}
	list, ok := norm(v).([]any)
	if !ok {
		r.nodeFailed(n, ns, &StepError{Code: "expression_error", Message: fmt.Sprintf("items must return a list, got %T", v)})
		return
	}
	if len(list) > MaxForeachItems {
		r.nodeFailed(n, ns, &StepError{Code: "limit_exceeded", Message: fmt.Sprintf("foreach has %d items; the limit is %d", len(list), MaxForeachItems)})
		return
	}
	ns.Items = list
	ns.ItemStates = make([]*ItemState, len(list))
	r.fillForeach(n, ns)
}

// fillForeach starts items up to the concurrency cap and completes the node when every
// item is finished.
func (r *run) fillForeach(n *Node, ns *NodeState) {
	active := 0
	for _, is := range ns.ItemStates {
		if is != nil && (is.Status == NodeRunning || is.Status == NodeRetryWait) {
			active++
		}
	}
	for i := range ns.ItemStates {
		if active >= n.Concurrency {
			break
		}
		if ns.ItemStates[i] != nil {
			continue
		}
		ns.ItemStates[i] = &ItemState{Status: NodeRunning, Attempt: 1}
		r.dispatch(n, ns, i+1, 1)
		if ns.ItemStates[i].Status == NodeRunning {
			active++
		}
	}
	if active > 0 {
		return
	}
	for _, is := range ns.ItemStates {
		if is == nil {
			return // cannot happen: a free slot would have started it
		}
	}
	results := make([]any, len(ns.ItemStates))
	failed := 0
	for i, is := range ns.ItemStates {
		res := map[string]any{"item": ns.Items[i], "status": is.Status, "output": is.Output}
		if is.Error != nil {
			res["error"] = errMap(is.Error)
		}
		if is.Status == NodeFailed {
			failed++
		}
		results[i] = res
	}
	ns.Output = results
	if failed > 0 {
		r.nodeFailed(n, ns, &StepError{Code: "foreach_failed", Message: fmt.Sprintf("%d of %d items failed", failed, len(results))})
		return
	}
	r.succeed(ns, n.Next)
}

func (r *run) itemResult(n *Node, ns *NodeState) {
	i := r.ev.Item - 1
	if ns.Status != NodeRunning || i < 0 || i >= len(ns.ItemStates) {
		return
	}
	is := ns.ItemStates[i]
	if is == nil || is.Status != NodeRunning || is.Attempt != r.ev.Attempt {
		return
	}
	r.changed = true
	if r.ev.Status == NodeSucceeded {
		is.Status, is.Output, is.Error = NodeSucceeded, norm(r.ev.Output), nil
	} else {
		e := r.ev.Error
		if e == nil {
			e = &StepError{Code: "unknown", Message: "step failed without an error"}
		}
		if r.shouldRetry(n, e, is.Attempt) {
			is.Status, is.Error = NodeRetryWait, e
			r.timerCmd(TimerRetry, n.ID, r.ev.Item, is.Attempt, r.ev.At.Add(Backoff(n.Retry, r.execID, n.ID, r.ev.Item, is.Attempt)))
		} else {
			is.Status, is.Error = NodeFailed, e
		}
	}
	r.fillForeach(n, ns)
	r.advance()
}

func (r *run) shouldRetry(n *Node, e *StepError, attempt int) bool {
	return e != nil && e.Retryable && attempt < n.Retry.MaxAttempts
}

// Backoff is exponential (or fixed) with deterministic jitter in [d/2, d): the same
// execution, node and attempt always wait the same time, keeping Next pure.
func Backoff(p RetryPolicy, execID, nodeID string, item, attempt int) time.Duration {
	d := p.Initial
	if d <= 0 {
		d = DefaultInitial
	}
	if p.Backoff != "fixed" {
		for i := 1; i < attempt && d < p.Max; i++ {
			d *= 2
		}
	}
	if p.Max > 0 && d > p.Max {
		d = p.Max
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s|%d|%d", execID, nodeID, item, attempt)
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(h.Sum64()%uint64(half))
}

func (r *run) timerCmd(kind, nodeID string, item, attempt int, due time.Time) {
	id := TimerID(r.execID, kind, nodeID, item, attempt)
	r.cmds = append(r.cmds, Command{Kind: CmdTimer, Key: "timer:" + id, Timer: &Timer{
		ID: id, ExecutionID: r.execID, NodeID: nodeID, Kind: kind, Attempt: attempt, Item: item, DueAt: due.UTC(),
	}})
}

// ---- expression variables ----

func (r *run) vars(item int, itemVal any) map[string]any {
	steps := make(map[string]any, len(r.st.Nodes))
	for id, ns := range r.st.Nodes {
		s := map[string]any{"status": ns.Status, "output": ns.Output, "attempt": float64(ns.Attempt)}
		if ns.Error != nil {
			s["error"] = errMap(ns.Error)
		}
		steps[id] = s
	}
	m := map[string]any{
		"alert":   celify(orEmpty(r.st.Alert)),
		"inputs":  celify(orEmpty(r.st.Inputs)),
		"vars":    celify(orEmpty(r.st.Vars)),
		"steps":   celify(steps),
		"secrets": map[string]any{},
		"env": map[string]any{
			"execution_id": r.execID, "workflow": r.w.Name, "workflow_id": r.w.ID,
			"workflow_version": float64(r.w.Version), "dry_run": r.st.DryRun,
		},
	}
	if item > 0 {
		m["item"] = celify(itemVal)
		m["index"] = int64(item - 1)
	}
	return m
}

// celify turns whole-number doubles (all JSON numbers decode as float64) into int64,
// so arithmetic with integer literals works in expressions: steps.a.output.n * 2.
// Comparisons across int and double work either way. The conversion is a pure
// function of stored state, so evaluation stays deterministic.
func celify(v any) any {
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) && t < 1<<53 && t > -(1<<53) {
			return int64(t)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = celify(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = celify(x)
		}
		return out
	}
	return v
}

// ---- helpers ----

func passTargets(n *Node) []string {
	var out []string
	for _, l := range [][]string{n.Next, n.Branches, n.Default} {
		for _, s := range l {
			out = appendUnique(out, s)
		}
	}
	return out
}

func exprError(where string, err error) *StepError {
	return &StepError{Code: "expression_error", Message: where + ": " + err.Error()}
}

func errText(e *StepError) string {
	if e == nil {
		return "unknown error"
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func errMap(e *StepError) map[string]any {
	return map[string]any{"code": e.Code, "message": e.Message, "retryable": e.Retryable}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func toAny(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// norm makes a value look exactly as it will after being stored and loaded again
// (numbers become float64), so evaluation before and after a reload agrees.
func norm(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func normMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out, _ := norm(m).(map[string]any)
	return out
}
