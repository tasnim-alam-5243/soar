package queue

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/yourorg/soar/internal/queue/queuetest"
)

func connect(t *testing.T, o StreamOptions) *Client {
	t.Helper()
	c, err := Connect([]string{queuetest.Start(t)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if o.EventsMaxBytes == 0 {
		o.EventsMaxBytes = 256 << 20 // JetStream reserves max_bytes against free disk
	}
	if err := c.EnsureStreams(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	return c
}

func eventsState(t *testing.T, c *Client) jetstream.StreamState {
	t.Helper()
	st, err := c.js.Stream(context.Background(), EventsStream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info.State
}

func TestSubjectSanitizesTokens(t *testing.T) {
	if got := Subject("events", "wazuh.prod *x>"); got != "events.wazuh_prod__x_" {
		t.Errorf("got %q", got)
	}
	if EventSubject("wazuh-prod") != "events.wazuh-prod" {
		t.Error("event subject")
	}
}

func TestEnsureStreamsIsRepeatableAndMatchesSpec(t *testing.T) {
	c := connect(t, StreamOptions{EventsMaxBytes: 1 << 30})
	if err := c.EnsureStreams(context.Background(), StreamOptions{EventsMaxBytes: 2 << 30}); err != nil {
		t.Fatalf("second call must update, not fail: %v", err)
	}
	st, _ := c.js.Stream(context.Background(), EventsStream)
	info, _ := st.Info(context.Background())
	cfg := info.Config
	if cfg.Retention != jetstream.WorkQueuePolicy || cfg.Discard != jetstream.DiscardNew || cfg.MaxBytes != 2<<30 ||
		cfg.Duplicates != 5*time.Minute || cfg.Storage != jetstream.FileStorage || cfg.MaxAge != 7*24*time.Hour {
		t.Errorf("EVENTS config differs from spec 8.2: %+v", cfg)
	}
}

func TestPublishIsDeduplicatedByMsgID(t *testing.T) {
	c := connect(t, StreamOptions{})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := c.Publish(ctx, Msg{Subject: EventSubject("s"), ID: "evt-1", Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}
	if n := eventsState(t, c).Msgs; n != 1 {
		t.Fatalf("same Nats-Msg-Id published three times must store one message, got %d", n)
	}
}

func TestPublishManyAcksEveryMessage(t *testing.T) {
	c := connect(t, StreamOptions{})
	ms := make([]Msg, 2500) // more than MaxPending: exercises the bounded in-flight window
	for i := range ms {
		ms[i] = Msg{Subject: EventSubject("s"), ID: fmt.Sprint("id-", i), Data: []byte("payload")}
	}
	for i, err := range c.PublishMany(context.Background(), ms) {
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if n := eventsState(t, c).Msgs; n != 2500 {
		t.Fatalf("stored %d, want 2500", n)
	}
}

func TestFullStreamRejectsWithErrQueueFullAndLosesNothingAccepted(t *testing.T) {
	c := connect(t, StreamOptions{EventsMaxBytes: 64 << 10})
	ctx := context.Background()
	payload := make([]byte, 1024)
	accepted, full := 0, 0
	for i := 0; i < 200; i++ {
		err := c.Publish(ctx, Msg{Subject: EventSubject("s"), ID: fmt.Sprint("m", i), Data: payload})
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrQueueFull):
			full++
		default:
			t.Fatalf("unexpected error class: %v", err)
		}
	}
	if full == 0 || accepted == 0 {
		t.Fatalf("expected both accepted and rejected publishes, got accepted=%d full=%d", accepted, full)
	}
	if n := int(eventsState(t, c).Msgs); n != accepted {
		t.Fatalf("every accepted message must be stored: accepted=%d stored=%d", accepted, n)
	}
}

func TestWorkQueueRemovesAckedMessagesInBatches(t *testing.T) {
	c := connect(t, StreamOptions{})
	ctx := context.Background()
	ms := make([]Msg, 600)
	for i := range ms {
		ms[i] = Msg{Subject: EventSubject("s"), ID: fmt.Sprint("b", i), Data: []byte("x")}
	}
	for _, err := range c.PublishMany(ctx, ms) {
		if err != nil {
			t.Fatal(err)
		}
	}
	cons, err := c.EventsConsumer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for got < 600 {
		batch, err := cons.Fetch(256, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for m := range batch.Messages() {
			if err := m.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
			n++
		}
		if n == 0 {
			t.Fatalf("fetch returned nothing after %d messages", got)
		}
		got += n
	}
	if n := eventsState(t, c).Msgs; n != 0 {
		t.Errorf("acked work-queue messages must be gone, %d remain", n)
	}
}

func TestPoisonMessageMovesToDLQAndReplays(t *testing.T) {
	c := connect(t, StreamOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	moved := make(chan uint64, 1)
	go func() { _ = c.MoveMaxDeliveries(ctx, func(seq uint64) { moved <- seq }) }()
	time.Sleep(200 * time.Millisecond) // let the advisory subscription register

	cons, err := c.EventsConsumer(ctx, func(cfg *jetstream.ConsumerConfig) {
		cfg.AckWait, cfg.MaxDeliver, cfg.BackOff = 100*time.Millisecond, 3, []time.Duration{50 * time.Millisecond, 50 * time.Millisecond}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(ctx, Msg{Subject: EventSubject("s"), ID: "poison", Data: []byte(`{"bad":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(ctx, Msg{Subject: EventSubject("s"), ID: "good", Data: []byte(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}

	var goodAcked atomic.Bool
	deadline := time.Now().Add(15 * time.Second)
	for len(moved) == 0 && time.Now().Before(deadline) {
		batch, err := cons.Fetch(10, jetstream.FetchMaxWait(200*time.Millisecond))
		if err != nil {
			continue
		}
		for m := range batch.Messages() {
			if string(m.Data()) == `{"ok":true}` {
				_ = m.DoubleAck(ctx)
				goodAcked.Store(true)
			} else {
				_ = m.Nak() // the processor "fails" on this message every time
			}
		}
	}
	select {
	case <-moved:
	default:
		t.Fatal("poison message never reached the DLQ")
	}
	if !goodAcked.Load() {
		t.Error("a poison message must not block the good one behind it")
	}
	if n := eventsState(t, c).Msgs; n != 0 {
		t.Errorf("poison message must be removed from EVENTS, %d remain", n)
	}
	entries, err := c.DLQList(ctx, 10, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("dlq: %v %+v", err, entries)
	}
	e := entries[0]
	if e.OriginSubject != "events.s" || e.Reason == "" || e.Deliveries != 3 {
		t.Errorf("dlq entry lacks context: %+v", e)
	}

	if err := c.DLQReplay(ctx, e.Seq); err != nil {
		t.Fatal(err)
	}
	if n := eventsState(t, c).Msgs; n != 1 {
		t.Errorf("replay must republish to the original subject, EVENTS has %d", n)
	}
	if left, _ := c.DLQList(ctx, 10, 0); len(left) != 0 {
		t.Errorf("replayed entry must leave the DLQ: %+v", left)
	}
	if err := c.DLQReplay(ctx, e.Seq); !errors.Is(err, ErrNotInDLQ) {
		t.Errorf("second replay: want ErrNotInDLQ, got %v", err)
	}
}

func TestTermRemovesMessageFromWorkQueue(t *testing.T) {
	c := connect(t, StreamOptions{})
	ctx := context.Background()
	if err := c.Publish(ctx, Msg{Subject: EventSubject("s"), ID: "t1", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	cons, _ := c.EventsConsumer(ctx)
	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for m := range batch.Messages() {
		if err := m.Term(); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := eventsState(t, c).Msgs; n != 0 {
		t.Logf("Term leaves the message in the work queue (%d)", n)
		t.Fatal("processor design assumes Term removes the message; it must delete it itself otherwise")
	}
}

func TestStatsReportsConsumerLag(t *testing.T) {
	c := connect(t, StreamOptions{})
	ctx := context.Background()
	if _, err := c.EventsConsumer(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_ = c.Publish(ctx, Msg{Subject: EventSubject("s"), ID: fmt.Sprint("x", i), Data: []byte("x")})
	}
	stats, err := c.Stats(ctx)
	if err != nil || len(stats) != 2 {
		t.Fatalf("stats: %v %+v", err, stats)
	}
	ev := stats[0]
	if ev.Name != EventsStream || ev.Messages != 5 || len(ev.Consumers) != 1 || ev.Consumers[0].Pending != 5 {
		t.Errorf("events stats: %+v", ev)
	}
}

func TestFirstDeliveryGetsTheFullAckDeadline(t *testing.T) {
	c := connect(t, StreamOptions{})
	ctx := context.Background()
	cons, err := c.EventsConsumer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := cons.Info(ctx)
	if len(info.Config.BackOff) == 0 || info.Config.BackOff[0] < info.Config.AckWait {
		t.Fatalf("BackOff[0]=%v must not undercut AckWait=%v: JetStream uses it as the first ack deadline", info.Config.BackOff, info.Config.AckWait)
	}
	if err := c.Publish(ctx, Msg{Subject: EventSubject("s"), ID: "slow", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	first, err := cons.Fetch(1, jetstream.FetchMaxWait(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var held jetstream.Msg
	for m := range first.Messages() {
		held = m
	}
	if held == nil {
		t.Fatal("no message")
	}
	time.Sleep(2500 * time.Millisecond) // slow processing, longer than the spec's literal 1 s
	again, _ := cons.Fetch(1, jetstream.FetchMaxWait(500*time.Millisecond))
	n := 0
	for range again.Messages() {
		n++
	}
	if n != 0 {
		t.Fatal("a message still being processed must not be redelivered after 2.5 s")
	}
	if err := held.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
}
