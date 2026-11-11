package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Execution flow streams (spec 8.2).
const (
	StepsStream   = "STEPS"
	ResultsStream = "RESULTS"
)

// Priorities and classes of step subjects: steps.<priority>.<class>.
var (
	Priorities = []string{"critical", "normal", "bulk"}
	Classes    = []string{"builtin", "python"}
)

// Envelope types.
const (
	MsgStart      = "start"
	MsgStep       = "step"
	MsgStepResult = "step_result"
	MsgTimer      = "timer"
	MsgCancel     = "cancel"
	MsgApproval   = "approval"
	MsgChildDone  = "child_done"
)

// Envelope is the message format of spec 8.2:
// {id, type, execution_id, node_id, attempt, idempotency_key, trace.traceparent, sent_at, payload | payload_ref}.
type Envelope struct {
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	ExecutionID    string          `json:"execution_id"`
	NodeID         string          `json:"node_id,omitempty"`
	Attempt        int             `json:"attempt,omitempty"`
	Item           int             `json:"item,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Partition      int             `json:"partition"`
	Trace          *Trace          `json:"trace,omitempty"`
	SentAt         time.Time       `json:"sent_at"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	PayloadRef     string          `json:"payload_ref,omitempty"`
}

// Trace carries W3C trace context between services.
type Trace struct {
	Traceparent string `json:"traceparent,omitempty"`
}

// NewEnvelope builds an envelope with payload p marshalled to JSON.
func NewEnvelope(typ, id, execID string, partition int, p any) (Envelope, error) {
	e := Envelope{ID: id, Type: typ, ExecutionID: execID, Partition: partition, SentAt: time.Now().UTC()}
	if p != nil {
		b, err := json.Marshal(p)
		if err != nil {
			return e, err
		}
		e.Payload = b
	}
	return e, nil
}

// Msg turns the envelope into a publishable message on subject; the envelope id is the Nats-Msg-Id.
func (e Envelope) Msg(subject string) (Msg, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return Msg{}, err
	}
	return Msg{Subject: subject, ID: e.ID, Data: b}, nil
}

// StepSubject is where steps of one priority and action class are queued.
func StepSubject(priority, class string) string { return Subject("steps", priority, class) }

// ResultSubject is the orchestrator inbox of one partition.
func ResultSubject(partition int) string { return Subject("results", strconv.Itoa(partition)) }

// Partition maps an execution to an orchestrator partition (spec 7: hash(execution_id) % N).
func Partition(executionID string, n int) int {
	if n <= 0 {
		n = 1
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(executionID))
	return int(h.Sum32() % uint32(n))
}

// FlowOptions size the STEPS and RESULTS streams.
type FlowOptions struct {
	Replicas int
	MaxBytes int64 // per stream; default 5 GiB
}

// EnsureFlowStreams creates or updates STEPS and RESULTS.
func (c *Client) EnsureFlowStreams(ctx context.Context, o FlowOptions) error {
	if o.Replicas < 1 {
		o.Replicas = 1
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 5 << 30
	}
	for _, cfg := range []jetstream.StreamConfig{
		{
			Name: StepsStream, Subjects: []string{"steps.>"}, Retention: jetstream.WorkQueuePolicy, Storage: jetstream.FileStorage,
			Replicas: o.Replicas, MaxAge: 24 * time.Hour, MaxBytes: o.MaxBytes, Discard: jetstream.DiscardNew,
			Duplicates: 5 * time.Minute, Compression: jetstream.S2Compression,
		},
		{
			Name: ResultsStream, Subjects: []string{"results.>"}, Retention: jetstream.WorkQueuePolicy, Storage: jetstream.FileStorage,
			Replicas: o.Replicas, MaxAge: 24 * time.Hour, MaxBytes: o.MaxBytes, Discard: jetstream.DiscardNew,
			Duplicates: 5 * time.Minute, Compression: jetstream.S2Compression,
		},
	} {
		if _, err := c.js.CreateStream(ctx, cfg); err != nil {
			if !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
				return fmt.Errorf("create stream %s: %w", cfg.Name, err)
			}
			if _, err := c.js.UpdateStream(ctx, cfg); err != nil {
				return fmt.Errorf("update stream %s: %w", cfg.Name, err)
			}
		}
	}
	return nil
}

// StepsConsumer is the durable consumer worker-{priority}-{class} (spec 8.3). Long
// steps keep their message with InProgress heartbeats, so AckWait is the heartbeat
// deadline rather than the step timeout.
func (c *Client) StepsConsumer(ctx context.Context, priority, class string, tune ...func(*jetstream.ConsumerConfig)) (jetstream.Consumer, error) {
	cfg := jetstream.ConsumerConfig{
		Durable: "worker-" + priority + "-" + class, AckPolicy: jetstream.AckExplicitPolicy,
		AckWait: 60 * time.Second, MaxDeliver: 3, MaxAckPending: 1000,
		FilterSubject: StepSubject(priority, class),
	}
	for _, t := range tune {
		t(&cfg)
	}
	return c.js.CreateOrUpdateConsumer(ctx, StepsStream, cfg)
}

// ResultsConsumer is the durable consumer orch-p{N} of one partition (spec 8.3).
// As with the processor, BackOff[0] acts as the first ack deadline.
func (c *Client) ResultsConsumer(ctx context.Context, partition int, tune ...func(*jetstream.ConsumerConfig)) (jetstream.Consumer, error) {
	cfg := jetstream.ConsumerConfig{
		Durable: "orch-p" + strconv.Itoa(partition), AckPolicy: jetstream.AckExplicitPolicy,
		AckWait: 30 * time.Second, MaxDeliver: 10, MaxAckPending: 512,
		BackOff:       []time.Duration{30 * time.Second, 5 * time.Second, 30 * time.Second},
		FilterSubject: ResultSubject(partition),
	}
	for _, t := range tune {
		t(&cfg)
	}
	return c.js.CreateOrUpdateConsumer(ctx, ResultsStream, cfg)
}

// DeadLetter describes a message moved to the DLQ by MoveExhausted.
type DeadLetter struct {
	Stream, Consumer, Subject string
	Data                      []byte
	Seq                       uint64
	Deliveries                int
}

// MoveExhausted is the DLQ mover of spec 8.5 for any stream: on the max-deliveries
// advisory of a consumer it copies the message to dlq.<origin>, calls onMove (the
// worker uses it to report the step as failed so the execution does not hang), and
// deletes the original so it cannot block the work queue. It returns when ctx ends.
func (c *Client) MoveExhausted(ctx context.Context, stream, origin string, onMove func(context.Context, DeadLetter)) error {
	subj := "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES." + stream + ".*"
	sub, err := c.nc.Subscribe(subj, func(m *nats.Msg) {
		var adv struct {
			Stream     string `json:"stream"`
			Consumer   string `json:"consumer"`
			StreamSeq  uint64 `json:"stream_seq"`
			Deliveries int    `json:"deliveries"`
		}
		if json.Unmarshal(m.Data, &adv) != nil || adv.StreamSeq == 0 {
			return
		}
		mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		st, err := c.js.Stream(mctx, stream)
		if err != nil {
			return
		}
		raw, err := st.GetMsg(mctx, adv.StreamSeq)
		if err != nil {
			return // already moved by another replica
		}
		extra := map[string]string{HdrDeliveries: strconv.Itoa(adv.Deliveries), HdrOriginSeq: strconv.FormatUint(adv.StreamSeq, 10)}
		id := "dlq-" + stream + "-" + strconv.FormatUint(adv.StreamSeq, 10)
		if err := c.SendToDLQ(mctx, origin, "max deliveries exceeded ("+adv.Consumer+")", raw.Subject, raw.Data, id, extra); err != nil {
			return
		}
		if onMove != nil {
			onMove(mctx, DeadLetter{Stream: stream, Consumer: adv.Consumer, Subject: raw.Subject, Data: raw.Data, Seq: adv.StreamSeq, Deliveries: adv.Deliveries})
		}
		_ = st.DeleteMsg(mctx, adv.StreamSeq)
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	return sub.Unsubscribe()
}

// Notify publishes a best-effort change notification on a core (non-JetStream)
// subject, for example ui.executions. Losing one is harmless: the UI refetches.
func (c *Client) Notify(subject string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = c.nc.Publish(subject, b)
}

// SubscribeCore listens on a core subject until the returned function is called.
func (c *Client) SubscribeCore(subject string, fn func(subject string, data []byte)) (func(), error) {
	sub, err := c.nc.Subscribe(subject, func(m *nats.Msg) { fn(m.Subject, m.Data) })
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}
