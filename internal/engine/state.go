package engine

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/yourorg/soar/internal/domain"
)

// Execution statuses (spec section 7).
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusWaiting   = "waiting"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusTimedOut  = "timed_out"
)

// Node statuses.
const (
	NodeRunning   = "running"    // a step is dispatched (or foreach items are)
	NodeRetryWait = "retry_wait" // a failed attempt waits for its retry timer
	NodeWaiting   = "waiting"    // wait, approval or subworkflow in progress
	NodeSucceeded = "succeeded"
	NodeFailed    = "failed"
	NodeSkipped   = "skipped"
	NodeCancelled = "cancelled"
)

// Event types.
const (
	EvStart      = "start"
	EvStepResult = "step_result"
	EvTimer      = "timer"
	EvApproval   = "approval"
	EvChildDone  = "child_done"
	EvCancel     = "cancel"
)

// Timer kinds.
const (
	TimerRetry    = "retry"
	TimerWait     = "wait"
	TimerDeadline = "deadline"
	TimerApproval = "approval_timeout"
)

// Command kinds.
const (
	CmdDispatch     = "dispatch"
	CmdTimer        = "timer"
	CmdApproval     = "approval"
	CmdStartChild   = "start_child"
	CmdNotifyParent = "notify_parent"
)

// IsTerminal reports whether an execution status is final.
func IsTerminal(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusTimedOut:
		return true
	}
	return false
}

func isTerminalNode(s string) bool {
	switch s {
	case NodeSucceeded, NodeFailed, NodeSkipped, NodeCancelled:
		return true
	}
	return false
}

// StepError is a failed step's error (mirrors the action error of spec section 10).
type StepError struct {
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	Retryable bool   `json:"retryable"`
}

// ParentRef links a child execution to the subworkflow node that started it.
type ParentRef struct {
	ExecutionID string `json:"execution_id"`
	NodeID      string `json:"node_id"`
	Partition   int    `json:"partition"`
}

// ItemState is the progress of one foreach item.
type ItemState struct {
	Status  string     `json:"status"`
	Attempt int        `json:"attempt"`
	Output  any        `json:"output,omitempty"`
	Error   *StepError `json:"error,omitempty"`
}

// NodeState is the progress of one node.
type NodeState struct {
	Status     string       `json:"status"`
	Attempt    int          `json:"attempt,omitempty"`
	Output     any          `json:"output,omitempty"`
	OutputRef  string       `json:"output_ref,omitempty"`
	Error      *StepError   `json:"error,omitempty"`
	Fired      []string     `json:"fired,omitempty"`  // successors this node activated
	Passed     bool         `json:"passed,omitempty"` // skipped by its own when: condition
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	EndedAt    *time.Time   `json:"ended_at,omitempty"`
	Items      []any        `json:"items,omitempty"`
	ItemStates []*ItemState `json:"item_states,omitempty"`
	ApprovalID string       `json:"approval_id,omitempty"`
	ChildID    string       `json:"child_id,omitempty"`
}

// State is everything the engine knows about an execution. It is stored as the
// execution's context and must stay JSON-serialisable.
type State struct {
	Status    string                `json:"status"`
	Alert     map[string]any        `json:"alert,omitempty"`
	Inputs    map[string]any        `json:"inputs,omitempty"`
	Vars      map[string]any        `json:"vars,omitempty"`
	Nodes     map[string]*NodeState `json:"nodes,omitempty"`
	Output    any                   `json:"output,omitempty"`
	Error     string                `json:"error,omitempty"`
	DryRun    bool                  `json:"dry_run,omitempty"`
	Parent    *ParentRef            `json:"parent,omitempty"`
	StartedAt *time.Time            `json:"started_at,omitempty"`
	EndedAt   *time.Time            `json:"ended_at,omitempty"`
}

// Clone returns a deep copy (state is plain JSON, so a round trip is exact).
func (s *State) Clone() *State {
	if s == nil {
		return &State{}
	}
	b, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("engine: state is not serialisable: %v", err))
	}
	var out State
	_ = json.Unmarshal(b, &out)
	return &out
}

// ActiveNodes lists the nodes that are still in progress, in no particular order.
func (s *State) ActiveNodes() []string {
	var out []string
	for id, n := range s.Nodes {
		if !isTerminalNode(n.Status) {
			out = append(out, id)
		}
	}
	return out
}

// StateFromJSON decodes a stored execution context.
func StateFromJSON(m map[string]any) (*State, error) {
	if len(m) == 0 {
		return &State{}, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// JSON encodes the state for storage as an execution context.
func (s *State) JSON() map[string]any {
	b, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// Event is an input to Next. At is supplied by the caller; the engine never reads a clock.
type Event struct {
	Type      string         `json:"type"`
	At        time.Time      `json:"at"`
	NodeID    string         `json:"node_id,omitempty"`
	Attempt   int            `json:"attempt,omitempty"`
	Item      int            `json:"item,omitempty"` // foreach item, 1-based; 0 = not an item
	Status    string         `json:"status,omitempty"`
	Output    any            `json:"output,omitempty"`
	OutputRef string         `json:"output_ref,omitempty"`
	Error     *StepError     `json:"error,omitempty"`
	TimerKind string         `json:"timer_kind,omitempty"`
	Approved  bool           `json:"approved,omitempty"`
	Actor     string         `json:"actor,omitempty"`
	Comment   string         `json:"comment,omitempty"`
	ChildID   string         `json:"child_id,omitempty"`
	Inputs    map[string]any `json:"inputs,omitempty"`
	Alert     map[string]any `json:"alert,omitempty"`
	DryRun    bool           `json:"dry_run,omitempty"`
	Parent    *ParentRef     `json:"parent,omitempty"`
	Reason    string         `json:"reason,omitempty"`
}

// Dispatch asks a worker to run one attempt of an action.
type Dispatch struct {
	ExecutionID    string         `json:"execution_id"`
	NodeID         string         `json:"node_id"`
	Action         string         `json:"action"`
	Attempt        int            `json:"attempt"`
	Item           int            `json:"item,omitempty"`
	Params         map[string]any `json:"params"`
	TimeoutMS      int64          `json:"timeout_ms"`
	Priority       string         `json:"priority"`
	IdempotencyKey string         `json:"idempotency_key"`
	DryRun         bool           `json:"dry_run,omitempty"`
}

// Timer asks for an event at DueAt.
type Timer struct {
	ID          string    `json:"id"`
	ExecutionID string    `json:"execution_id"`
	NodeID      string    `json:"node_id,omitempty"`
	Kind        string    `json:"kind"`
	Attempt     int       `json:"attempt,omitempty"`
	Item        int       `json:"item,omitempty"`
	DueAt       time.Time `json:"due_at"`
}

// ApprovalRequest asks humans to decide.
type ApprovalRequest struct {
	ID          string    `json:"id"`
	ExecutionID string    `json:"execution_id"`
	NodeID      string    `json:"node_id"`
	Approvers   []string  `json:"approvers"`
	Message     string    `json:"message,omitempty"`
	DueAt       time.Time `json:"due_at"`
}

// ChildStart starts a subworkflow execution.
type ChildStart struct {
	ExecutionID string         `json:"execution_id"`
	Workflow    string         `json:"workflow"`
	Inputs      map[string]any `json:"inputs,omitempty"`
	Parent      ParentRef      `json:"parent"`
	DryRun      bool           `json:"dry_run,omitempty"`
}

// ParentNotice reports a child's outcome to its parent.
type ParentNotice struct {
	Parent  ParentRef  `json:"parent"`
	ChildID string     `json:"child_id"`
	Status  string     `json:"status"`
	Output  any        `json:"output,omitempty"`
	Error   *StepError `json:"error,omitempty"`
}

// Command is a side effect for the orchestrator. Key is unique per effect and used
// as the outbox key and as Nats-Msg-Id, so carrying a command out twice is harmless.
type Command struct {
	Kind     string           `json:"kind"`
	Key      string           `json:"key"`
	Dispatch *Dispatch        `json:"dispatch,omitempty"`
	Timer    *Timer           `json:"timer,omitempty"`
	Approval *ApprovalRequest `json:"approval,omitempty"`
	Child    *ChildStart      `json:"child,omitempty"`
	Parent   *ParentNotice    `json:"parent,omitempty"`
}

// Result is the outcome of Next. Changed is false when the event did not apply
// (a duplicate or a stale result); State is then equal to the input.
type Result struct {
	State    *State
	Commands []Command
	Changed  bool
	// Invalid is set when the event would have produced a status change that the state
	// machine forbids. That is a bug: the change is refused (Changed is false) rather
	// than stored, and the caller logs it.
	Invalid string
}

// ValidTransition is the execution state machine of spec section 7, for the status
// before and after one call of Next. One event can pass through several states (a
// start that runs only set nodes goes queued -> running -> succeeded), so the table
// is the closure of the spec's single steps:
//
//	queued  -> running | waiting | succeeded | failed | cancelled | timed_out
//	running -> running | waiting | succeeded | failed | cancelled | timed_out
//	waiting -> running | waiting | succeeded | failed | cancelled | timed_out
//	terminal states never change; nothing returns to queued.
//
// queued -> cancelled (a cancel before the first step) is the one addition to the
// spec's table: a queued execution can be cancelled from the API.
func ValidTransition(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case "", StatusQueued, StatusRunning, StatusWaiting:
	default:
		return false // terminal (or unknown) states are final
	}
	switch to {
	case StatusRunning, StatusWaiting, StatusSucceeded, StatusFailed, StatusCancelled, StatusTimedOut:
		return true
	}
	return false
}

// StepKey is the idempotency key format of spec rule 3.1.4: {execution_id}:{node_id}:{attempt},
// with the foreach item in brackets when there is one.
func StepKey(execID, nodeID string, item, attempt int) string {
	if item > 0 {
		return fmt.Sprintf("%s:%s[%d]:%d", execID, nodeID, item, attempt)
	}
	return fmt.Sprintf("%s:%s:%d", execID, nodeID, attempt)
}

// TimerID, ApprovalID and ChildID are deterministic, so recomputing a command after
// a crash yields the same document id and a second create fails harmlessly.
func TimerID(execID, kind, nodeID string, item, attempt int) string {
	return domain.DeterministicID("tmr", execID, kind, nodeID, item, attempt)
}

func ApprovalID(execID, nodeID string) string { return domain.DeterministicID("apr", execID, nodeID) }

func ChildID(execID, nodeID string) string {
	return domain.DeterministicID(domain.PrefixExecution, execID, "child", nodeID)
}
