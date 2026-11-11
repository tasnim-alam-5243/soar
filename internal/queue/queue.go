package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Stream and consumer names (spec section 8).
const (
	EventsStream      = "EVENTS"
	DLQStream         = "DLQ"
	ProcessorConsumer = "processor"

	// MaxPending bounds unacknowledged async publishes per connection (spec 3.3: about 1,000).
	MaxPending = 1000
)

var (
	// ErrQueueFull means the stream hit its byte limit (discard: new). Callers answer 503 + Retry-After.
	ErrQueueFull = errors.New("queue: stream is full")
	// ErrUnavailable means NATS could not persist the message right now.
	ErrUnavailable = errors.New("queue: unavailable")
)

// Subject is the only place subject names are built (spec rule 3.1), so a later
// change such as adding a tenant segment stays local.
func Subject(parts ...string) string {
	clean := make([]string, len(parts))
	for i, p := range parts {
		clean[i] = tokenRe.ReplaceAllString(p, "_")
	}
	return strings.Join(clean, ".")
}

var tokenRe = regexp.MustCompile(`[\s.*>]`)

// EventSubject is where normalized events of one source are published.
func EventSubject(sourceName string) string { return Subject("events", sourceName) }

// StreamOptions are per-environment stream settings.
type StreamOptions struct {
	Replicas       int   // 1 in dev, 3 in production
	EventsMaxBytes int64 // default 20 GiB
}

// Client is a NATS connection with JetStream.
type Client struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// Connect opens a connection that reconnects forever.
func Connect(urls []string, name string, extra ...nats.Option) (*Client, error) {
	nc, err := nats.Connect(strings.Join(urls, ","),
		append([]nats.Option{nats.Name(name), nats.MaxReconnects(-1), nats.ReconnectWait(500 * time.Millisecond), nats.Timeout(5 * time.Second)}, extra...)...)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncMaxPending(MaxPending))
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return &Client{nc: nc, js: js}, nil
}

// Close drains pending acks and closes the connection.
func (c *Client) Close() { _ = c.nc.Drain() }

// NC and JS expose the underlying handles for code that needs them (tests, advisories).
func (c *Client) NC() *nats.Conn          { return c.nc }
func (c *Client) JS() jetstream.JetStream { return c.js }

// EnsureStreams creates or updates EVENTS and DLQ. Safe to call from every role at start.
func (c *Client) EnsureStreams(ctx context.Context, o StreamOptions) error {
	if o.Replicas < 1 {
		o.Replicas = 1
	}
	if o.EventsMaxBytes <= 0 {
		o.EventsMaxBytes = 20 << 30
	}
	cfgs := []jetstream.StreamConfig{
		{
			Name: EventsStream, Subjects: []string{"events.>"}, Retention: jetstream.WorkQueuePolicy,
			Storage: jetstream.FileStorage, Replicas: o.Replicas, MaxAge: 7 * 24 * time.Hour,
			MaxBytes: o.EventsMaxBytes, Discard: jetstream.DiscardNew, Duplicates: 5 * time.Minute,
			Compression: jetstream.S2Compression,
		},
		{
			Name: DLQStream, Subjects: []string{"dlq.>"}, Retention: jetstream.LimitsPolicy,
			Storage: jetstream.FileStorage, Replicas: o.Replicas, MaxAge: 30 * 24 * time.Hour,
			Duplicates: 2 * time.Minute, Compression: jetstream.S2Compression,
		},
	}
	for _, cfg := range cfgs {
		if _, err := c.js.CreateStream(ctx, cfg); err != nil {
			if !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
				var api *jetstream.APIError
				if errors.As(err, &api) && api.ErrorCode == 10047 {
					return fmt.Errorf("create stream %s: NATS has less free storage than the stream's max_bytes (%d); lower SOAR_EVENTS_MAX_BYTES or give NATS more disk: %w", cfg.Name, cfg.MaxBytes, err)
				}
				return fmt.Errorf("create stream %s: %w", cfg.Name, err)
			}
			if _, err := c.js.UpdateStream(ctx, cfg); err != nil {
				return fmt.Errorf("update stream %s: %w", cfg.Name, err)
			}
		}
	}
	return nil
}

// Msg is one message to publish. ID becomes Nats-Msg-Id (deduplicated for 5 minutes).
type Msg struct {
	Subject string
	ID      string
	Data    []byte
	Header  nats.Header
}

// Publish stores one message and returns after JetStream acknowledged the write.
func (c *Client) Publish(ctx context.Context, m Msg) error {
	return c.PublishMany(ctx, []Msg{m})[0]
}

// PublishMany publishes all messages concurrently (bounded by MaxPending) and returns
// one error per message, nil meaning acknowledged and durable.
func (c *Client) PublishMany(ctx context.Context, ms []Msg) []error {
	errs := make([]error, len(ms))
	futs := make([]jetstream.PubAckFuture, len(ms))
	for i, m := range ms {
		nm := &nats.Msg{Subject: m.Subject, Data: m.Data, Header: m.Header}
		opts := []jetstream.PublishOpt{jetstream.WithStallWait(5 * time.Second)}
		if m.ID != "" {
			opts = append(opts, jetstream.WithMsgID(m.ID))
		}
		f, err := c.js.PublishMsgAsync(nm, opts...)
		if err != nil {
			errs[i] = classify(err)
			continue
		}
		futs[i] = f
	}
	for i, f := range futs {
		if f == nil {
			continue
		}
		select {
		case <-f.Ok():
		case err := <-f.Err():
			errs[i] = classify(err)
		case <-ctx.Done():
			errs[i] = ctx.Err()
		}
	}
	return errs
}

func classify(err error) error {
	if strings.Contains(err.Error(), "maximum bytes exceeded") {
		return fmt.Errorf("%w: %w", ErrQueueFull, err)
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// EventsConsumer returns the durable pull consumer of the processor group (spec 8.3).
// EventsConsumer returns the durable pull consumer of the processor group (spec 8.3).
//
// Note on BackOff: when BackOff is set, JetStream ignores AckWait and uses BackOff[0] as the
// deadline of the FIRST delivery. The spec's "1 s, 5 s, 30 s, 2 min" would therefore give
// every message a 1-second ack deadline and cause redeliveries of messages that are still
// being processed. So the first entry is the real processing deadline (60 s) and the
// later entries delay redelivery after a crash or lost ack. Transient failures use an
// explicit NakWithDelay and are not affected.
//
// tune lets tests shorten the timings; production passes none.
func (c *Client) EventsConsumer(ctx context.Context, tune ...func(*jetstream.ConsumerConfig)) (jetstream.Consumer, error) {
	cfg := jetstream.ConsumerConfig{
		Durable: ProcessorConsumer, AckPolicy: jetstream.AckExplicitPolicy, AckWait: 60 * time.Second,
		MaxDeliver: 5, BackOff: []time.Duration{60 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute},
		MaxAckPending: 2048, FilterSubject: "events.>",
	}
	for _, t := range tune {
		t(&cfg)
	}
	return c.js.CreateOrUpdateConsumer(ctx, EventsStream, cfg)
}

// Header names used on DLQ messages.
const (
	HdrOriginSubject = "Soar-Origin-Subject"
	HdrReason        = "Soar-Reason"
	HdrDeliveries    = "Soar-Deliveries"
	HdrOriginSeq     = "Soar-Origin-Seq"
	HdrTrace         = "Soar-Trace"
)

// SendToDLQ copies a message to dlq.<origin> with the reason it failed.
func (c *Client) SendToDLQ(ctx context.Context, origin, reason, subject string, data []byte, id string, extra map[string]string) error {
	h := nats.Header{}
	h.Set(HdrOriginSubject, subject)
	h.Set(HdrReason, reason)
	for k, v := range extra {
		h.Set(k, v)
	}
	return c.Publish(ctx, Msg{Subject: Subject("dlq", origin), ID: id, Data: data, Header: h})
}

// DLQEntry is one dead-lettered message.
type DLQEntry struct {
	Seq           uint64    `json:"seq"`
	Subject       string    `json:"subject"`
	OriginSubject string    `json:"origin_subject"`
	Reason        string    `json:"reason"`
	Deliveries    int       `json:"deliveries,omitempty"`
	StoredAt      time.Time `json:"stored_at"`
	Size          int       `json:"size"`
}

// DLQList returns up to limit entries, newest first, starting below beforeSeq (0 = from the end).
func (c *Client) DLQList(ctx context.Context, limit int, beforeSeq uint64) ([]DLQEntry, error) {
	st, err := c.js.Stream(ctx, DLQStream)
	if err != nil {
		return nil, err
	}
	info, err := st.Info(ctx)
	if err != nil {
		return nil, err
	}
	seq := info.State.LastSeq
	if beforeSeq > 0 && beforeSeq-1 < seq {
		seq = beforeSeq - 1
	}
	var out []DLQEntry
	for ; seq >= info.State.FirstSeq && seq > 0 && len(out) < limit; seq-- {
		m, err := st.GetMsg(ctx, seq)
		if err != nil {
			continue // deleted (already replayed)
		}
		e := DLQEntry{
			Seq: m.Sequence, Subject: m.Subject, OriginSubject: m.Header.Get(HdrOriginSubject),
			Reason: m.Header.Get(HdrReason), StoredAt: m.Time, Size: len(m.Data),
		}
		e.Deliveries, _ = strconv.Atoi(m.Header.Get(HdrDeliveries))
		out = append(out, e)
	}
	return out, nil
}

// ErrNotInDLQ is returned by DLQReplay for an unknown sequence.
var ErrNotInDLQ = errors.New("queue: no such dead-letter message")

// DLQReplay republishes a dead letter to its original subject and removes it from the DLQ.
func (c *Client) DLQReplay(ctx context.Context, seq uint64) error {
	st, err := c.js.Stream(ctx, DLQStream)
	if err != nil {
		return err
	}
	m, err := st.GetMsg(ctx, seq)
	if err != nil {
		return ErrNotInDLQ
	}
	origin := m.Header.Get(HdrOriginSubject)
	if origin == "" {
		return fmt.Errorf("dead letter %d has no origin subject", seq)
	}
	id := fmt.Sprintf("replay-%d-%d", seq, time.Now().UnixNano())
	if err := c.Publish(ctx, Msg{Subject: origin, ID: id, Data: m.Data}); err != nil {
		return err
	}
	return st.DeleteMsg(ctx, seq)
}

// ConsumerStats and StreamStats feed GET /queues/stats and the monitoring alerts in spec 8.9.
type ConsumerStats struct {
	Name        string `json:"name"`
	Pending     uint64 `json:"pending"`
	AckPending  int    `json:"ack_pending"`
	Redelivered int    `json:"redelivered"`
}

type StreamStats struct {
	Name      string          `json:"name"`
	Messages  uint64          `json:"messages"`
	Bytes     uint64          `json:"bytes"`
	MaxBytes  int64           `json:"max_bytes"`
	Consumers []ConsumerStats `json:"consumers,omitempty"`
}

// Stats reports every stream and its consumers.
func (c *Client) Stats(ctx context.Context) ([]StreamStats, error) {
	var out []StreamStats
	for _, name := range []string{EventsStream, StepsStream, ResultsStream, DLQStream} {
		st, err := c.js.Stream(ctx, name)
		if errors.Is(err, jetstream.ErrStreamNotFound) && (name == StepsStream || name == ResultsStream) {
			continue // created when the orchestrator and worker first start
		}
		if err != nil {
			return nil, err
		}
		info, err := st.Info(ctx)
		if err != nil {
			return nil, err
		}
		s := StreamStats{Name: name, Messages: info.State.Msgs, Bytes: info.State.Bytes, MaxBytes: info.Config.MaxBytes}
		lister := st.ListConsumers(ctx)
		for ci := range lister.Info() {
			s.Consumers = append(s.Consumers, ConsumerStats{Name: ci.Name, Pending: ci.NumPending, AckPending: ci.NumAckPending, Redelivered: ci.NumRedelivered})
		}
		out = append(out, s)
	}
	return out, nil
}

// MoveMaxDeliveries is a DLQ mover (spec 8.5): it listens for the "max deliveries"
// advisory of the processor consumer, copies the poison message to dlq.events with
// its reason, and deletes the original so it cannot block the work queue.
// It returns when ctx ends.
func (c *Client) MoveMaxDeliveries(ctx context.Context, onMove func(seq uint64)) error {
	subj := "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES." + EventsStream + "." + ProcessorConsumer
	sub, err := c.nc.Subscribe(subj, func(m *nats.Msg) {
		var adv struct {
			StreamSeq  uint64 `json:"stream_seq"`
			Deliveries int    `json:"deliveries"`
		}
		if json.Unmarshal(m.Data, &adv) != nil || adv.StreamSeq == 0 {
			return
		}
		mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		st, err := c.js.Stream(mctx, EventsStream)
		if err != nil {
			return
		}
		raw, err := st.GetMsg(mctx, adv.StreamSeq)
		if err != nil {
			return // already moved
		}
		extra := map[string]string{HdrDeliveries: strconv.Itoa(adv.Deliveries), HdrOriginSeq: strconv.FormatUint(adv.StreamSeq, 10)}
		if err := c.SendToDLQ(mctx, "events", "max deliveries exceeded", raw.Subject, raw.Data, "dlq-"+EventsStream+"-"+strconv.FormatUint(adv.StreamSeq, 10), extra); err != nil {
			return // advisory is not re-sent; the poison message stays and is retried by the next advisory
		}
		if err := st.DeleteMsg(mctx, adv.StreamSeq); err == nil && onMove != nil {
			onMove(adv.StreamSeq)
		}
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	return sub.Unsubscribe()
}
