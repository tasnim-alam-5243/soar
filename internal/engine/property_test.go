package engine

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// randomWorkflow builds an acyclic definition: edges only go from a node to a later one.
func randomWorkflow(rng *rand.Rand) map[string]any {
	n := 2 + rng.Intn(14)
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("n%d", i)
	}
	later := func(i, max int) []any {
		var out []any
		seen := map[int]bool{}
		for k := 0; k < max && i+1 < n; k++ {
			j := i + 1 + rng.Intn(n-i-1)
			if !seen[j] {
				seen[j] = true
				out = append(out, ids[j])
			}
		}
		return out
	}
	var nodes []any
	for i, id := range ids {
		node := map[string]any{"id": id}
		switch r := rng.Intn(100); {
		case r < 50:
			node["action"] = "test.op"
			node["params"] = map[string]any{"n": float64(i), "expr": "inputs.x + 1"}
			node["retry"] = map[string]any{"max_attempts": float64(1 + rng.Intn(3)), "initial": "1s"}
		case r < 60 && i+1 < n:
			targets := later(i, 2)
			cases := []any{map[string]any{"when": fmt.Sprintf("inputs.x %% %d == 0", 2+rng.Intn(3)), "next": targets[:1]}}
			node["type"] = "branch"
			node["cases"] = cases
			if len(targets) > 1 {
				node["default"] = targets[1:]
			}
		case r < 68:
			if t := later(i, 3); len(t) > 0 {
				node["type"] = "parallel"
				node["branches"] = t
			} else {
				node["type"] = "set"
				node["vars"] = map[string]any{"v" + id: "inputs.x * 2"}
			}
		case r < 74:
			node["type"] = "set"
			node["vars"] = map[string]any{"v" + id: "inputs.x * 2"}
		case r < 80:
			node["type"] = "wait"
			node["duration"] = fmt.Sprintf("%ds", 1+rng.Intn(60))
		case r < 88:
			node["type"] = "foreach"
			node["items"] = fmt.Sprintf("[1, 2, 3, %d]", i)
			node["action"] = "test.op"
			node["concurrency"] = float64(1 + rng.Intn(3))
			node["params"] = map[string]any{"v": "item"}
		case r < 94:
			node["type"] = "approval"
			node["timeout"] = "1h"
		default:
			node["type"] = "subworkflow"
			node["workflow"] = "child"
		}
		if node["type"] != "branch" && node["type"] != "parallel" && i+1 < n && rng.Intn(3) > 0 {
			node["next"] = later(i, 2)
		}
		if rng.Intn(6) == 0 && i+1 < n {
			node["on_error"] = later(i, 1)
		} else if rng.Intn(6) == 0 {
			node["continue_on_error"] = true
		}
		nodes = append(nodes, node)
	}
	return map[string]any{"nodes": nodes}
}

type pending struct {
	ev  Event
	key string
}

func outcome(rng *rand.Rand) Event {
	switch r := rng.Intn(10); {
	case r < 6:
		return Event{Status: NodeSucceeded, Output: map[string]any{"r": float64(r)}}
	case r < 9:
		return Event{Status: NodeFailed, Error: &StepError{Code: "timeout", Retryable: true}}
	default:
		return Event{Status: NodeFailed, Error: &StepError{Code: "http_400", Retryable: false}}
	}
}

// runRandom executes a workflow with random outcomes, random delivery order and
// duplicated deliveries, checking invariants after every step. It returns the event
// log so the run can be replayed.
func runRandom(t *testing.T, w *Workflow, rng *rand.Rand, seed int64) ([]Event, *State, []Command) {
	t.Helper()
	st := &State{}
	now := t0
	var log []Event
	var allCmds []Command
	var queue []pending
	var delivered []Event
	dispatched := map[string]bool{}

	step := func(ev Event) {
		ev.At = now
		before, _ := json.Marshal(st)
		res := Next(w, "exe_p", st, ev)
		log = append(log, ev)
		after, _ := json.Marshal(res.State)
		if !res.Changed && (string(before) != string(after) || len(res.Commands) > 0) {
			t.Fatalf("seed %d: unchanged result must equal input and emit nothing", seed)
		}
		if IsTerminal(st.Status) && res.Changed {
			t.Fatalf("seed %d: a terminal execution changed on %s", seed, ev.Type)
		}
		if res.Invalid != "" {
			t.Fatalf("seed %d: %s", seed, res.Invalid)
		}
		if res.Changed && !ValidTransition(st.Status, res.State.Status) {
			t.Fatalf("seed %d: %s -> %s is not a state-machine transition (spec 7)", seed, st.Status, res.State.Status)
		}
		st = res.State
		allCmds = append(allCmds, res.Commands...)
		for _, c := range res.Commands {
			switch c.Kind {
			case CmdDispatch:
				if dispatched[c.Key] {
					t.Fatalf("seed %d: dispatch key %s emitted twice", seed, c.Key)
				}
				dispatched[c.Key] = true
				d := c.Dispatch
				if d.Params["expr"] != nil && d.Params["expr"] != float64(8) {
					t.Fatalf("seed %d: params not evaluated: %v", seed, d.Params)
				}
				o := outcome(rng)
				o.Type, o.NodeID, o.Attempt, o.Item = EvStepResult, d.NodeID, d.Attempt, d.Item
				queue = append(queue, pending{ev: o, key: c.Key})
			case CmdTimer:
				if c.Timer.Kind == TimerDeadline {
					continue
				}
				queue = append(queue, pending{ev: Event{Type: EvTimer, TimerKind: c.Timer.Kind, NodeID: c.Timer.NodeID, Attempt: c.Timer.Attempt, Item: c.Timer.Item}, key: c.Key})
			case CmdApproval:
				queue = append(queue, pending{ev: Event{Type: EvApproval, NodeID: c.Approval.NodeID, Approved: rng.Intn(2) == 0, Actor: "u"}, key: c.Key})
			case CmdStartChild:
				status := StatusSucceeded
				if rng.Intn(4) == 0 {
					status = StatusFailed
				}
				queue = append(queue, pending{ev: Event{Type: EvChildDone, NodeID: c.Child.Parent.NodeID, ChildID: c.Child.ExecutionID, Status: status}, key: c.Key})
			}
		}
		if res.Changed {
			delivered = append(delivered, ev)
		}
	}

	step(Event{Type: EvStart, Inputs: map[string]any{"x": float64(7)}})
	for i := 0; len(queue) > 0; i++ {
		if i > 100000 {
			t.Fatalf("seed %d: no progress", seed)
		}
		now = now.Add(time.Duration(rng.Intn(5000)) * time.Millisecond)
		// sometimes redeliver something old (at-least-once delivery)
		if len(delivered) > 0 && rng.Intn(4) == 0 {
			step(delivered[rng.Intn(len(delivered))])
			continue
		}
		k := rng.Intn(len(queue))
		p := queue[k]
		queue = append(queue[:k], queue[k+1:]...)
		step(p.ev)
	}

	if !IsTerminal(st.Status) {
		t.Fatalf("seed %d: execution did not finish: status %s, active %v", seed, st.Status, st.ActiveNodes())
	}
	for id, ns := range st.Nodes {
		if !isTerminalNode(ns.Status) {
			t.Fatalf("seed %d: node %s left %s", seed, id, ns.Status)
		}
	}
	if st.Status == StatusSucceeded {
		for _, n := range w.Nodes {
			ns := st.Nodes[n.ID]
			if ns == nil {
				t.Fatalf("seed %d: succeeded but node %s never decided", seed, n.ID)
			}
			if ns.Status == NodeFailed && len(n.OnError) == 0 && !n.ContinueOnError {
				t.Fatalf("seed %d: succeeded with unhandled failure in %s", seed, n.ID)
			}
		}
	}
	// a succeeded action ran exactly attempts 1..Attempt
	for _, n := range w.Nodes {
		ns := st.Nodes[n.ID]
		if n.Type != TypeAction || ns == nil || ns.Status != NodeSucceeded {
			continue
		}
		for a := 1; a <= ns.Attempt; a++ {
			if !dispatched[StepKey("exe_p", n.ID, 0, a)] {
				t.Fatalf("seed %d: node %s attempt %d was never dispatched", seed, n.ID, a)
			}
		}
		if dispatched[StepKey("exe_p", n.ID, 0, ns.Attempt+1)] {
			t.Fatalf("seed %d: node %s dispatched beyond its final attempt", seed, n.ID)
		}
	}
	return log, st, allCmds
}

func TestPropertyRandomWorkflows(t *testing.T) {
	runs := 600
	if testing.Short() {
		runs = 100
	}
	compiled := 0
	for seed := int64(1); seed <= int64(runs); seed++ {
		rng := rand.New(rand.NewSource(seed))
		d := randomWorkflow(rng)
		w, errs := Compile(d, nil)
		if len(errs) > 0 {
			t.Fatalf("seed %d: generated workflow does not compile: %+v\n%v", seed, errs, d)
		}
		compiled++
		log, final, cmds := runRandom(t, w, rng, seed)

		// determinism: replaying the same event log gives the same commands and state
		st := &State{}
		var replayCmds []Command
		for _, ev := range log {
			res := Next(w, "exe_p", st, ev)
			st = res.State
			replayCmds = append(replayCmds, res.Commands...)
		}
		a, _ := json.Marshal(final)
		b, _ := json.Marshal(st)
		if string(a) != string(b) {
			t.Fatalf("seed %d: replay produced a different state", seed)
		}
		ca, _ := json.Marshal(cmds)
		cb, _ := json.Marshal(replayCmds)
		if string(ca) != string(cb) {
			t.Fatalf("seed %d: replay produced different commands", seed)
		}
	}
	if compiled != runs {
		t.Fatalf("compiled %d of %d", compiled, runs)
	}
}

// FuzzCompile makes sure arbitrary definitions never panic the validator.
func FuzzCompile(f *testing.F) {
	f.Add(phishing)
	f.Add("nodes: [{id: a, type: foreach, items: '[1]', action: x}]")
	f.Fuzz(func(t *testing.T, src string) {
		var m map[string]any
		if err := json.Unmarshal([]byte(src), &m); err != nil {
			return
		}
		_, _ = Compile(m, nil)
	})
}
