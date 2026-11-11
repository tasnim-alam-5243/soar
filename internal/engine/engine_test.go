package engine

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

var t0 = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

func def(t testing.TB, src string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	return m
}

func mustCompile(t testing.TB, src string) *Workflow {
	t.Helper()
	w, errs := Compile(def(t, src), nil)
	if len(errs) > 0 {
		t.Fatalf("compile: %+v", errs)
	}
	return w
}

// sim drives a workflow the way the orchestrator would, with a scripted worker.
type sim struct {
	t        testing.TB
	w        *Workflow
	id       string
	st       *State
	now      time.Time
	timers   []*Timer
	dispatch []*Dispatch
	cmds     []Command
	worker   func(d *Dispatch) Event // nil: leave dispatches pending
}

func newSim(t testing.TB, w *Workflow) *sim {
	return &sim{t: t, w: w, id: "exe_test", st: &State{}, now: t0}
}

func (s *sim) apply(ev Event) Result {
	s.t.Helper()
	if ev.At.IsZero() {
		ev.At = s.now
	}
	res := Next(s.w, s.id, s.st, ev)
	s.st = res.State
	s.cmds = append(s.cmds, res.Commands...)
	for _, c := range res.Commands {
		switch c.Kind {
		case CmdTimer:
			s.timers = append(s.timers, c.Timer)
		case CmdDispatch:
			s.dispatch = append(s.dispatch, c.Dispatch)
		}
	}
	return res
}

// drain runs pending dispatches through the worker and fires due timers until nothing is left.
func (s *sim) drain() {
	s.t.Helper()
	for i := 0; i < 10000; i++ {
		if IsTerminal(s.st.Status) {
			return
		}
		if len(s.dispatch) > 0 && s.worker != nil {
			d := s.dispatch[0]
			s.dispatch = s.dispatch[1:]
			ev := s.worker(d)
			ev.Type, ev.NodeID, ev.Attempt, ev.Item = EvStepResult, d.NodeID, d.Attempt, d.Item
			s.apply(ev)
			continue
		}
		// fire the earliest non-deadline timer
		sort.SliceStable(s.timers, func(i, j int) bool { return s.timers[i].DueAt.Before(s.timers[j].DueAt) })
		fired := false
		for k, tm := range s.timers {
			if tm.Kind == TimerDeadline || tm.Kind == TimerApproval {
				continue
			}
			s.timers = append(s.timers[:k], s.timers[k+1:]...)
			if tm.DueAt.After(s.now) {
				s.now = tm.DueAt
			}
			s.apply(Event{Type: EvTimer, TimerKind: tm.Kind, NodeID: tm.NodeID, Attempt: tm.Attempt, Item: tm.Item})
			fired = true
			break
		}
		if !fired {
			return
		}
	}
	s.t.Fatal("simulation did not settle")
}

func ok(out any) Event { return Event{Status: NodeSucceeded, Output: out} }
func fail(code string, retryable bool) Event {
	return Event{Status: NodeFailed, Error: &StepError{Code: code, Message: code, Retryable: retryable}}
}

func (s *sim) nodeStatus(id string) string {
	if ns := s.st.Nodes[id]; ns != nil {
		return ns.Status
	}
	return ""
}

const phishing = `
name: phishing_triage
version: 1
triggers:
  - type: alert
    match: 'alert.source_id == "email_gateway" && alert.severity >= 3'
inputs:
  - name: url
    from: 'alert.observables.filter(o, o.type == "url")[0].value'
nodes:
  - id: check_url
    action: http.request
    params: { method: GET, url: '"https://intel.example/u?q=" + inputs.url' }
    retry: { max_attempts: 3, backoff: exponential, initial: 2s, max: 30s }
    timeout: 20s
  - id: decide
    type: branch
    cases:
      - when: 'steps.check_url.output.body.score > 70'
        next: [block]
    default: [close]
  - id: block
    action: connector.firewall.block_url
    idempotency_key: '"block:" + inputs.url'
    params: { url: 'inputs.url' }
  - id: close
    action: case.close
edges:
  - check_url -> decide
`

var phishAlert = map[string]any{
	"source_id": "email_gateway", "severity": 4,
	"observables": []any{map[string]any{"type": "ip", "value": "1.2.3.4"}, map[string]any{"type": "url", "value": "http://bad.example/x"}},
}

func TestSpecExampleBlocksHighScore(t *testing.T) {
	w := mustCompile(t, phishing)
	s := newSim(t, w)
	s.worker = func(d *Dispatch) Event {
		switch d.Action {
		case "http.request":
			if d.Params["url"] != "https://intel.example/u?q=http://bad.example/x" || d.Params["method"] != "GET" {
				t.Errorf("params not resolved: %v", d.Params)
			}
			if d.TimeoutMS != 20000 {
				t.Errorf("timeout %d", d.TimeoutMS)
			}
			return ok(map[string]any{"status": 200, "body": map[string]any{"score": 91}})
		case "connector.firewall.block_url":
			if d.IdempotencyKey != "key:block:http://bad.example/x" {
				t.Errorf("custom idempotency key not used: %q", d.IdempotencyKey)
			}
			return ok(map[string]any{"blocked": true})
		}
		t.Fatalf("unexpected action %s", d.Action)
		return Event{}
	}
	s.apply(Event{Type: EvStart, Alert: phishAlert})
	s.drain()
	if s.st.Status != StatusSucceeded {
		t.Fatalf("status %s (%s)", s.st.Status, s.st.Error)
	}
	if s.nodeStatus("block") != NodeSucceeded || s.nodeStatus("close") != NodeSkipped {
		t.Errorf("block=%s close=%s", s.nodeStatus("block"), s.nodeStatus("close"))
	}
	if s.st.Inputs["url"] != "http://bad.example/x" {
		t.Errorf("input: %v", s.st.Inputs)
	}
}

func TestSpecExampleClosesLowScore(t *testing.T) {
	w := mustCompile(t, phishing)
	s := newSim(t, w)
	s.worker = func(d *Dispatch) Event {
		if d.Action == "http.request" {
			return ok(map[string]any{"body": map[string]any{"score": 10}})
		}
		return ok(nil)
	}
	s.apply(Event{Type: EvStart, Alert: phishAlert})
	s.drain()
	if s.st.Status != StatusSucceeded || s.nodeStatus("block") != NodeSkipped || s.nodeStatus("close") != NodeSucceeded {
		t.Fatalf("status=%s block=%s close=%s", s.st.Status, s.nodeStatus("block"), s.nodeStatus("close"))
	}
}

func TestRetryThenSucceedAndDuplicatesIgnored(t *testing.T) {
	w := mustCompile(t, `
nodes:
  - id: a
    action: http.request
    retry: { max_attempts: 3, initial: 2s, max: 30s }
`)
	s := newSim(t, w)
	calls := 0
	s.worker = func(d *Dispatch) Event {
		calls++
		if d.Attempt != calls {
			t.Errorf("attempt %d on call %d", d.Attempt, calls)
		}
		if calls < 3 {
			return fail("timeout", true)
		}
		return ok("done")
	}
	s.apply(Event{Type: EvStart})
	s.drain()
	if s.st.Status != StatusSucceeded || calls != 3 {
		t.Fatalf("status=%s calls=%d err=%s", s.st.Status, calls, s.st.Error)
	}
	if s.st.Nodes["a"].Attempt != 3 {
		t.Errorf("attempt %d", s.st.Nodes["a"].Attempt)
	}
	// a late duplicate of attempt 1's failure changes nothing
	before := s.st.Clone()
	res := s.apply(Event{Type: EvStepResult, NodeID: "a", Attempt: 1, Status: NodeFailed, Error: &StepError{Code: "timeout", Retryable: true}})
	if res.Changed || len(res.Commands) > 0 || !reflect.DeepEqual(before, s.st) {
		t.Error("duplicate result must be a no-op")
	}
	// backoff grows and stays within bounds
	var retries []time.Duration
	for _, c := range s.cmds {
		if c.Kind == CmdTimer && c.Timer.Kind == TimerRetry {
			retries = append(retries, c.Timer.DueAt.Sub(t0))
		}
	}
	if len(retries) != 2 || retries[0] < time.Second || retries[0] >= 2*time.Second {
		t.Errorf("retry timers: %v", retries)
	}
}

func TestNonRetryableFailsAndOnErrorRecovers(t *testing.T) {
	src := `
nodes:
  - id: a
    action: http.request
  - id: b
    action: notify.log
`
	w := mustCompile(t, src+"edges: [a -> b]\n")
	s := newSim(t, w)
	s.worker = func(d *Dispatch) Event { return fail("http_400", false) }
	s.apply(Event{Type: EvStart})
	s.drain()
	if s.st.Status != StatusFailed || !strings.Contains(s.st.Error, "node a failed") {
		t.Fatalf("status=%s err=%q", s.st.Status, s.st.Error)
	}
	if s.nodeStatus("b") != "" {
		t.Errorf("b must not start, got %s", s.nodeStatus("b"))
	}

	w = mustCompile(t, `
nodes:
  - id: a
    action: http.request
    on_error: [handler]
  - id: b
    action: notify.log
  - id: handler
    action: notify.log
edges: [a -> b]
`)
	s = newSim(t, w)
	s.worker = func(d *Dispatch) Event {
		if d.NodeID == "a" {
			return fail("http_400", false)
		}
		return ok(nil)
	}
	s.apply(Event{Type: EvStart})
	s.drain()
	if s.st.Status != StatusSucceeded || s.nodeStatus("handler") != NodeSucceeded || s.nodeStatus("b") != NodeSkipped {
		t.Fatalf("status=%s handler=%s b=%s", s.st.Status, s.nodeStatus("handler"), s.nodeStatus("b"))
	}
}

func TestParallelJoinWaitsForAllActivated(t *testing.T) {
	w := mustCompile(t, `
nodes:
  - { id: start, type: parallel, branches: [x, y] }
  - { id: x, action: notify.log }
  - { id: y, action: notify.log }
  - { id: join, action: notify.log }
edges: [x -> join, y -> join]
`)
	s := newSim(t, w)
	s.apply(Event{Type: EvStart})
	if len(s.dispatch) != 2 {
		t.Fatalf("want x and y dispatched, got %d", len(s.dispatch))
	}
	s.apply(Event{Type: EvStepResult, NodeID: "x", Attempt: 1, Status: NodeSucceeded})
	if s.nodeStatus("join") != "" {
		t.Fatal("join must wait for y")
	}
	s.apply(Event{Type: EvStepResult, NodeID: "y", Attempt: 1, Status: NodeSucceeded})
	if s.nodeStatus("join") != NodeRunning {
		t.Fatalf("join=%s", s.nodeStatus("join"))
	}
	s.apply(Event{Type: EvStepResult, NodeID: "join", Attempt: 1, Status: NodeSucceeded})
	if s.st.Status != StatusSucceeded {
		t.Fatal(s.st.Status)
	}
}

func TestBranchJoinRunsAfterSkippedArm(t *testing.T) {
	w := mustCompile(t, `
nodes:
  - id: b
    type: branch
    cases: [{ when: 'inputs.n > 1', next: [big] }]
    default: [small]
  - { id: big, action: notify.log }
  - { id: small, action: notify.log }
  - { id: after, action: notify.log }
edges: [big -> after, small -> after]
`)
	s := newSim(t, w)
	s.worker = func(*Dispatch) Event { return ok(nil) }
	s.apply(Event{Type: EvStart, Inputs: map[string]any{"n": 5}})
	s.drain()
	if s.st.Status != StatusSucceeded || s.nodeStatus("small") != NodeSkipped || s.nodeStatus("after") != NodeSucceeded {
		t.Fatalf("status=%s small=%s after=%s", s.st.Status, s.nodeStatus("small"), s.nodeStatus("after"))
	}
}

func TestForeachConcurrencyAndResults(t *testing.T) {
	w := mustCompile(t, `
nodes:
  - id: each
    type: foreach
    items: 'inputs.boxes'
    concurrency: 2
    action: outlook.mail.move
    params: { mailbox: 'item', position: 'index' }
    retry: { max_attempts: 2, initial: 1s }
`)
	s := newSim(t, w)
	s.apply(Event{Type: EvStart, Inputs: map[string]any{"boxes": []any{"a@x", "b@x", "c@x", "d@x", "e@x"}}})
	if len(s.dispatch) != 2 {
		t.Fatalf("concurrency 2: dispatched %d", len(s.dispatch))
	}
	if s.dispatch[0].Params["mailbox"] != "a@x" || s.dispatch[1].Params["position"] != float64(1) {
		t.Fatalf("item params: %v %v", s.dispatch[0].Params, s.dispatch[1].Params)
	}
	seenRetry := false
	s.worker = func(d *Dispatch) Event {
		if d.Params["mailbox"] == "c@x" && d.Attempt == 1 {
			seenRetry = true
			return fail("http_429", true)
		}
		if d.Params["mailbox"] == "e@x" {
			return fail("not_found", false)
		}
		return ok(map[string]any{"moved": d.Params["mailbox"]})
	}
	s.drain()
	if !seenRetry {
		t.Error("c@x should be retried")
	}
	if s.st.Status != StatusFailed || s.st.Nodes["each"].Error.Code != "foreach_failed" {
		t.Fatalf("status=%s node=%+v", s.st.Status, s.st.Nodes["each"])
	}
	out := s.st.Nodes["each"].Output.([]any)
	if len(out) != 5 || out[2].(map[string]any)["status"] != NodeSucceeded || out[4].(map[string]any)["status"] != NodeFailed {
		t.Fatalf("results: %v", out)
	}
	// keys are distinct per item and attempt
	keys := map[string]bool{}
	for _, c := range s.cmds {
		if c.Kind == CmdDispatch {
			if keys[c.Key] {
				t.Errorf("duplicate dispatch key %s", c.Key)
			}
			keys[c.Key] = true
		}
	}
	if !keys["exe_test:each[3]:2"] {
		t.Errorf("missing retry key; have %v", keys)
	}
}

func TestEmptyForeachSucceeds(t *testing.T) {
	w := mustCompile(t, "nodes: [{ id: each, type: foreach, items: '[]', action: notify.log }]\n")
	s := newSim(t, w)
	s.apply(Event{Type: EvStart})
	if s.st.Status != StatusSucceeded {
		t.Fatal(s.st.Status)
	}
}

func TestWaitAndSet(t *testing.T) {
	w := mustCompile(t, `
nodes:
  - { id: pause, type: wait, duration: 5m }
  - { id: s, type: set, vars: { count: '1 + 2', msg: '"n=" + string(1 + 2)' } }
  - { id: log, action: notify.log, params: { message: 'vars.msg' } }
edges: [pause -> s, s -> log]
`)
	s := newSim(t, w)
	s.apply(Event{Type: EvStart})
	if s.st.Status != StatusWaiting {
		t.Fatalf("waiting expected, got %s", s.st.Status)
	}
	var wait *Timer
	for _, tm := range s.timers {
		if tm.Kind == TimerWait {
			wait = tm
		}
	}
	if wait == nil || !wait.DueAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("wait timer %+v", wait)
	}
	s.worker = func(d *Dispatch) Event {
		if d.Params["message"] != "n=3" {
			t.Errorf("message %v", d.Params["message"])
		}
		return ok(nil)
	}
	s.drain()
	if s.st.Status != StatusSucceeded || s.st.Vars["count"] != float64(3) {
		t.Fatalf("status=%s vars=%v", s.st.Status, s.st.Vars)
	}
}

func TestApproval(t *testing.T) {
	src := `
nodes:
  - { id: ask, type: approval, message: '"Delete mail from " + inputs.who', timeout: 1h }
  - { id: purge, action: outlook.mail.delete }
edges: [ask -> purge]
`
	w := mustCompile(t, src)
	s := newSim(t, w)
	s.apply(Event{Type: EvStart, Inputs: map[string]any{"who": "eve"}})
	var req *ApprovalRequest
	for _, c := range s.cmds {
		if c.Kind == CmdApproval {
			req = c.Approval
		}
	}
	if req == nil || req.Message != "Delete mail from eve" || !reflect.DeepEqual(req.Approvers, DefaultApprovers) || !req.DueAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("approval request %+v", req)
	}
	if s.st.Status != StatusWaiting {
		t.Fatal(s.st.Status)
	}
	s.apply(Event{Type: EvApproval, NodeID: "ask", Approved: true, Actor: "alice"})
	if s.nodeStatus("purge") != NodeRunning {
		t.Fatalf("purge=%s", s.nodeStatus("purge"))
	}
	// a second decision is ignored
	if res := s.apply(Event{Type: EvApproval, NodeID: "ask", Approved: false, Actor: "bob"}); res.Changed {
		t.Error("second decision must be ignored")
	}

	// rejection without on_reject fails the execution; timeout rejects by default
	for _, ev := range []Event{
		{Type: EvApproval, NodeID: "ask", Approved: false, Actor: "bob", Comment: "no"},
		{Type: EvTimer, TimerKind: TimerApproval, NodeID: "ask"},
	} {
		s := newSim(t, w)
		s.apply(Event{Type: EvStart, Inputs: map[string]any{"who": "eve"}})
		s.apply(ev)
		if s.st.Status != StatusFailed || s.nodeStatus("purge") != "" {
			t.Errorf("%s: status=%s purge=%s", ev.Type, s.st.Status, s.nodeStatus("purge"))
		}
	}
}

func TestCancelAndDeadline(t *testing.T) {
	w := mustCompile(t, "nodes: [{ id: a, action: http.request }]\ndeadline: 1h\n")
	s := newSim(t, w)
	s.apply(Event{Type: EvStart})
	var dl *Timer
	for _, tm := range s.timers {
		if tm.Kind == TimerDeadline {
			dl = tm
		}
	}
	if dl == nil || !dl.DueAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("deadline timer %+v", dl)
	}
	s.apply(Event{Type: EvTimer, TimerKind: TimerDeadline})
	if s.st.Status != StatusTimedOut || s.nodeStatus("a") != NodeCancelled {
		t.Fatalf("status=%s a=%s", s.st.Status, s.nodeStatus("a"))
	}
	if res := s.apply(Event{Type: EvStepResult, NodeID: "a", Attempt: 1, Status: NodeSucceeded}); res.Changed {
		t.Error("results after the end must be ignored")
	}

	s = newSim(t, w)
	s.apply(Event{Type: EvStart})
	s.apply(Event{Type: EvCancel, Actor: "alice"})
	if s.st.Status != StatusCancelled || !strings.Contains(s.st.Error, "alice") {
		t.Fatalf("status=%s err=%s", s.st.Status, s.st.Error)
	}
}

func TestWhenSkipsButPassesFlow(t *testing.T) {
	w := mustCompile(t, `
nodes:
  - { id: a, action: notify.log, when: 'inputs.go == true' }
  - { id: b, action: notify.log }
edges: [a -> b]
`)
	s := newSim(t, w)
	s.worker = func(*Dispatch) Event { return ok(nil) }
	s.apply(Event{Type: EvStart, Inputs: map[string]any{"go": false}})
	s.drain()
	if s.nodeStatus("a") != NodeSkipped || s.nodeStatus("b") != NodeSucceeded || s.st.Status != StatusSucceeded {
		t.Fatalf("a=%s b=%s status=%s", s.nodeStatus("a"), s.nodeStatus("b"), s.st.Status)
	}
}

func TestEndNodeAndOutputs(t *testing.T) {
	w := mustCompile(t, `
nodes:
  - { id: a, action: notify.log }
  - { id: stop, type: end, status: failed, output: '{"why": "policy"}' }
  - { id: never, action: notify.log }
edges: [a -> stop, a -> never]
`)
	s := newSim(t, w)
	s.apply(Event{Type: EvStart})
	s.apply(Event{Type: EvStepResult, NodeID: "a", Attempt: 1, Status: NodeSucceeded})
	if s.st.Status != StatusFailed || s.st.Output.(map[string]any)["why"] != "policy" {
		t.Fatalf("status=%s out=%v", s.st.Status, s.st.Output)
	}

	w = mustCompile(t, `
nodes: [{ id: a, action: notify.log }]
outputs: { result: 'steps.a.output.n * 2' }
`)
	s = newSim(t, w)
	s.apply(Event{Type: EvStart})
	s.apply(Event{Type: EvStepResult, NodeID: "a", Attempt: 1, Status: NodeSucceeded, Output: map[string]any{"n": 21}})
	if s.st.Status != StatusSucceeded || s.st.Output.(map[string]any)["result"] != float64(42) {
		t.Fatalf("status=%s err=%s out=%v", s.st.Status, s.st.Error, s.st.Output)
	}
}

func TestSubworkflow(t *testing.T) {
	w := mustCompile(t, `
nodes:
  - { id: child, type: subworkflow, workflow: enrich_ip, inputs: { ip: '"10.0.0.1"' } }
  - { id: after, action: notify.log, params: { v: 'steps.child.output.score' } }
edges: [child -> after]
`)
	s := newSim(t, w)
	s.apply(Event{Type: EvStart})
	var cs *ChildStart
	for _, c := range s.cmds {
		if c.Kind == CmdStartChild {
			cs = c.Child
		}
	}
	if cs == nil || cs.Workflow != "enrich_ip" || cs.Inputs["ip"] != "10.0.0.1" || cs.Parent.ExecutionID != "exe_test" {
		t.Fatalf("child start %+v", cs)
	}
	s.apply(Event{Type: EvChildDone, NodeID: "child", ChildID: cs.ExecutionID, Status: StatusSucceeded, Output: map[string]any{"score": 7}})
	if len(s.dispatch) != 1 || s.dispatch[0].Params["v"] != float64(7) {
		t.Fatalf("after dispatch %+v", s.dispatch)
	}

	// the child notifies its parent when it ends
	cw := mustCompile(t, "nodes: [{ id: x, type: set, vars: { a: '1' } }]\noutputs: { score: '5' }\n")
	cs2 := newSim(t, cw)
	cs2.id = cs.ExecutionID
	cs2.apply(Event{Type: EvStart, Inputs: cs.Inputs, Parent: &cs.Parent})
	var notice *ParentNotice
	for _, c := range cs2.cmds {
		if c.Kind == CmdNotifyParent {
			notice = c.Parent
		}
	}
	if notice == nil || notice.Status != StatusSucceeded || notice.Parent.NodeID != "child" || notice.Output.(map[string]any)["score"] != float64(5) {
		t.Fatalf("notice %+v", notice)
	}
}

func TestSecretsBecomePlaceholders(t *testing.T) {
	w := mustCompile(t, `
nodes:
  - id: call
    action: http.request
    params:
      url: '"https://x.example"'
      headers: { Authorization: '"Bearer " + secrets.vt_key' }
`)
	s := newSim(t, w)
	s.apply(Event{Type: EvStart})
	p := s.dispatch[0].Params
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "hunter2") {
		t.Fatal("secret leaked")
	}
	if names := SecretNames(p); !reflect.DeepEqual(names, []string{"vt_key"}) {
		t.Fatalf("names %v", names)
	}
	out, err := ResolveSecrets(p, func(n string) (string, error) { return "hunter2", nil })
	if err != nil || out.(map[string]any)["headers"].(map[string]any)["Authorization"] != "Bearer hunter2" {
		t.Fatalf("resolved %v %v", out, err)
	}
	if RedactSecrets(p).(map[string]any)["headers"].(map[string]any)["Authorization"] != "Bearer [secret:vt_key]" {
		t.Fatal("redaction")
	}
	// the state itself never holds a secret marker
	if b, _ := json.Marshal(s.st); strings.Contains(string(b), "secret:") {
		t.Fatal("state must not contain secrets")
	}
}

func TestRequiredInputMissingFails(t *testing.T) {
	w := mustCompile(t, `
inputs: [{ name: url, from: 'alert.observables.filter(o, o.type == "url")[0].value', required: true }]
nodes: [{ id: a, action: notify.log }]
`)
	s := newSim(t, w)
	s.apply(Event{Type: EvStart, Alert: map[string]any{"observables": []any{}}})
	if s.st.Status != StatusFailed || !strings.Contains(s.st.Error, "url") {
		t.Fatalf("status=%s err=%s", s.st.Status, s.st.Error)
	}
}

func TestDuplicateStartIsNoop(t *testing.T) {
	w := mustCompile(t, phishing)
	s := newSim(t, w)
	s.apply(Event{Type: EvStart, Alert: phishAlert})
	if res := s.apply(Event{Type: EvStart, Alert: phishAlert}); res.Changed || len(res.Commands) != 0 {
		t.Fatal("second start must be a no-op")
	}
}

func TestNextDoesNotMutateInput(t *testing.T) {
	w := mustCompile(t, phishing)
	st := &State{}
	r1 := Next(w, "exe_1", st, Event{Type: EvStart, At: t0, Alert: phishAlert})
	snapshot, _ := json.Marshal(r1.State)
	_ = Next(w, "exe_1", r1.State, Event{Type: EvStepResult, At: t0, NodeID: "check_url", Attempt: 1, Status: NodeSucceeded, Output: map[string]any{"body": map[string]any{"score": 99}}})
	if after, _ := json.Marshal(r1.State); string(after) != string(snapshot) {
		t.Fatal("Next mutated its input state")
	}
}

// ---- validation ----

type fakeCatalog map[string]ActionInfo

func (f fakeCatalog) Lookup(a string) (ActionInfo, bool) { i, ok := f[a]; return i, ok }

func TestValidation(t *testing.T) {
	cat := fakeCatalog{"http.request": {ID: "http.request", ParamsSchema: map[string]any{
		"type": "object", "required": []any{"url"}, "additionalProperties": false,
		"properties": map[string]any{"url": map[string]any{"type": "string"}, "method": map[string]any{"type": "string", "enum": []any{"GET", "POST"}}},
	}}}
	cases := []struct {
		name, src, path, msg string
	}{
		{"cycle", "nodes: [{id: a, action: x, next: [b]}, {id: b, action: x, next: [a]}]", "definition", "cycle"},
		{"unknown target", "nodes: [{id: a, action: x, next: [zz]}]", "definition.nodes[0].next[0]", "unknown node"},
		{"agent reserved", "nodes: [{id: a, type: agent}]", "definition.nodes[0].type", "reserved"},
		{"bad cel", "nodes: [{id: a, type: branch, cases: [{when: 'a ==', next: []}]}]", "definition.nodes[0].cases[0].when", "does not compile"},
		{"duplicate id", "nodes: [{id: a, action: x}, {id: a, action: x}]", "definition.nodes[1].id", "duplicate"},
		{"misspelled field", "nodes: [{id: a, action: x, parms: {}}]", "definition.nodes[0].parms", "not a field"},
		{"unknown top field", "nods: []\nnodes: [{id: a, action: x}]", "definition.nods", "unknown"},
		{"retry limit", "nodes: [{id: a, action: x, retry: {max_attempts: 11}}]", "definition.nodes[0].retry.max_attempts", "1 to 10"},
		{"timeout limit", "nodes: [{id: a, action: x, timeout: 2h}]", "definition.nodes[0].timeout", "1h"},
		{"wait needs one", "nodes: [{id: a, type: wait}]", "definition.nodes[0]", "exactly one"},
		{"edge from branch", "nodes: [{id: a, type: branch, cases: [{when: 'true', next: [b]}]}, {id: b, action: x}]\nedges: [a -> b]", "definition.edges[0]", "routes through cases"},
		{"self loop", "nodes: [{id: a, action: x, next: [a]}]", "definition.nodes[0].next[0]", "itself"},
		{"bad trigger", "triggers: [{type: alert, match: 'alert.x =='}]\nnodes: [{id: a, action: x}]", "definition.triggers[0].match", "does not compile"},
		{"bad cron", "triggers: [{type: schedule, cron: '61 * * * *'}]\nnodes: [{id: a, action: x}]", "definition.triggers[0].cron", "outside"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errs := Compile(def(t, c.src), nil)
			for _, e := range errs {
				if e.Path == c.path && strings.Contains(e.Message, c.msg) {
					return
				}
			}
			t.Fatalf("want %s: %q, got %+v", c.path, c.msg, errs)
		})
	}
	t.Run("catalog", func(t *testing.T) {
		_, errs := Compile(def(t, "nodes: [{id: a, action: nope}, {id: b, action: http.request, params: {method: PUT, extra: 1}}]"), cat)
		want := map[string]string{
			"definition.nodes[0].action":        "unknown action",
			"definition.nodes[1].params.url":    "is required",
			"definition.nodes[1].params.extra":  "not a known parameter",
			"definition.nodes[1].params.method": "must be one of",
		}
		for p, m := range want {
			found := false
			for _, e := range errs {
				if e.Path == p && strings.Contains(e.Message, m) {
					found = true
				}
			}
			if !found {
				t.Errorf("missing %s: %s in %+v", p, m, errs)
			}
		}
		// an expression value is not type-checked at save time
		if _, errs := Compile(def(t, "nodes: [{id: b, action: http.request, params: {url: 'inputs.u', method: 'inputs.m'}}]"), cat); len(errs) > 0 {
			t.Errorf("expressions must pass: %+v", errs)
		}
	})
	t.Run("literal vs expression", func(t *testing.T) {
		w := mustCompile(t, "nodes: [{id: a, action: x, params: {method: GET, title: Phishing alert, ip: 10.0.0.1, url: 'inputs.u', n: 5}}]")
		p := w.Node("a").Params.Map
		if !p["method"].Literal() || !p["title"].Literal() || !p["ip"].Literal() || p["url"].Literal() || !p["n"].Literal() {
			t.Fatal("literal detection")
		}
	})
}

func TestCron(t *testing.T) {
	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{"*/15 * * * *", time.Date(2026, 1, 1, 10, 7, 30, 0, time.UTC), time.Date(2026, 1, 1, 10, 15, 0, 0, time.UTC)},
		{"0 9 * * mon-fri", time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)}, // Sat -> Mon
		{"@daily", time.Date(2026, 1, 31, 23, 59, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{"30 2 29 2 *", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2028, 2, 29, 2, 30, 0, 0, time.UTC)},
		{"@every 10m", time.Date(2026, 1, 1, 10, 7, 0, 0, time.UTC), time.Date(2026, 1, 1, 10, 10, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		cr, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := cr.Next(c.from); !got.Equal(c.want) {
			t.Errorf("%s from %s: got %s want %s", c.expr, c.from, got, c.want)
		}
	}
	for _, bad := range []string{"* * *", "60 * * * *", "* * * 13 *", "*/0 * * * *", "@every 1s"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestStateMachineTable(t *testing.T) {
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{"", StatusRunning, true},
		{StatusQueued, StatusRunning, true},
		{StatusQueued, StatusSucceeded, true},
		{StatusQueued, StatusCancelled, true},
		{StatusRunning, StatusWaiting, true},
		{StatusWaiting, StatusRunning, true},
		{StatusRunning, StatusTimedOut, true},
		{StatusWaiting, StatusCancelled, true},
		{StatusRunning, StatusRunning, true},
		{StatusSucceeded, StatusRunning, false},
		{StatusFailed, StatusSucceeded, false},
		{StatusCancelled, StatusRunning, false},
		{StatusTimedOut, StatusFailed, false},
		{StatusRunning, StatusQueued, false},
		{StatusWaiting, StatusQueued, false},
		{StatusRunning, "paused", false},
	} {
		if got := ValidTransition(c.from, c.to); got != c.ok {
			t.Errorf("%q -> %q: got %v, want %v", c.from, c.to, got, c.ok)
		}
	}
}
