package adapters

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/yourorg/soar/internal/domain"
)

func init() {
	RegisterStream("kafka", StreamSpec{New: newKafka, NeedsConnection: true})
}

// kafka consumes topics with a consumer group (Step 10). Offsets are committed only
// after the batch is queued (at least once); each record carries its identity
// (topic/partition/offset), so a batch re-read after a crash or rebalance is
// deduplicated by the queue.
//
// Connection (connector "kafka"): config.brokers (list or "a:9092,b:9092"), config.tls
// (bool), config.ca_file (private CA), config.sasl (plain, scram-sha-256,
// scram-sha-512) with secret_refs username and password.
// Source config: topics (list, required), group (default "soar-<source>"),
// start: latest | earliest (where a new group begins; default latest), max_batch: 500.
type kafkaStream struct {
	source   string
	topics   []string
	group    string
	start    string
	maxBatch int
	conn     *domain.Connection
	resolve  func(string) (string, error)
	log      *slog.Logger
}

func newKafka(src domain.Source, deps StreamDeps) (Stream, error) {
	k := &kafkaStream{
		source: src.Name, group: "soar-" + src.Name, start: "latest", maxBatch: 500,
		conn: deps.Conn, resolve: deps.Resolve, log: deps.Log,
	}
	if k.log == nil {
		k.log = slog.Default()
	}
	k.topics = stringList(src.Config["topics"])
	if len(k.topics) == 0 {
		return nil, fmt.Errorf("config.topics must list at least one topic")
	}
	if g, _ := src.Config["group"].(string); g != "" {
		k.group = g
	}
	if s, _ := src.Config["start"].(string); s != "" {
		if s != "latest" && s != "earliest" {
			return nil, fmt.Errorf("config.start must be latest or earliest, got %q", s)
		}
		k.start = s
	}
	if n := intOf(src.Config["max_batch"]); n > 0 {
		k.maxBatch = n
	}
	if k.conn != nil {
		if _, err := k.options(); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// options builds the client options from the connection.
func (k *kafkaStream) options() ([]kgo.Opt, error) {
	cfg := k.conn.Config
	brokers := stringList(cfg["brokers"])
	if len(brokers) == 0 {
		return nil, fmt.Errorf("connection %q: config.brokers must list at least one broker (host:port)", k.conn.Name)
	}
	reset := kgo.NewOffset().AtEnd()
	if k.start == "earliest" {
		reset = kgo.NewOffset().AtStart()
	}
	opts := []kgo.Opt{
		kgo.SeedBrokers(brokers...), kgo.ConsumerGroup(k.group), kgo.ConsumeTopics(k.topics...),
		kgo.DisableAutoCommit(), kgo.ConsumeResetOffset(reset), kgo.BlockRebalanceOnPoll(),
		kgo.ClientID("soar-collector"),
	}
	if useTLS, _ := cfg["tls"].(bool); useTLS {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if ca, _ := cfg["ca_file"].(string); ca != "" {
			pem, err := os.ReadFile(ca)
			if err != nil {
				return nil, fmt.Errorf("connection %q: ca_file: %w", k.conn.Name, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("connection %q: ca_file has no PEM certificate", k.conn.Name)
			}
			tc.RootCAs = pool
		}
		opts = append(opts, kgo.DialTLSConfig(tc))
	}
	if mech, _ := cfg["sasl"].(string); mech != "" {
		if k.resolve == nil {
			return opts, nil // validating only: credentials are resolved when the stream starts
		}
		user, err := k.secret("username")
		if err != nil {
			return nil, err
		}
		pass, err := k.secret("password")
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(mech) {
		case "plain":
			opts = append(opts, kgo.SASL(plain.Auth{User: user, Pass: pass}.AsMechanism()))
		case "scram-sha-256":
			opts = append(opts, kgo.SASL(scram.Auth{User: user, Pass: pass}.AsSha256Mechanism()))
		case "scram-sha-512":
			opts = append(opts, kgo.SASL(scram.Auth{User: user, Pass: pass}.AsSha512Mechanism()))
		default:
			return nil, fmt.Errorf("connection %q: sasl must be plain, scram-sha-256 or scram-sha-512", k.conn.Name)
		}
	}
	return opts, nil
}

func (k *kafkaStream) secret(name string) (string, error) {
	ref, _ := k.conn.SecretRefs[name].(string)
	if ref == "" {
		return "", fmt.Errorf("connection %q: secret_refs.%s is required for SASL", k.conn.Name, name)
	}
	return k.resolve(ref)
}

func (k *kafkaStream) Run(ctx context.Context, emit Emit) error {
	if k.conn == nil {
		return fmt.Errorf("kafka sources need a connection")
	}
	opts, err := k.options()
	if err != nil {
		return err
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return err
	}
	// with BlockRebalanceOnPoll a plain Close waits for AllowRebalance; leave the group cleanly
	defer cl.CloseAllowingRebalance()
	k.log.Info("kafka consumer started", "topics", k.topics, "group", k.group)
	for ctx.Err() == nil {
		fetches := cl.PollRecords(ctx, k.maxBatch)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return nil
		}
		failed := false
		fetches.EachError(func(topic string, partition int32, err error) {
			failed = true
			k.log.Warn("kafka fetch error", "topic", topic, "partition", partition, "error", err.Error())
		})
		var batch []Message
		var recs []*kgo.Record
		fetches.EachRecord(func(r *kgo.Record) {
			id := fmt.Sprintf("%s/%d/%d", r.Topic, r.Partition, r.Offset)
			batch = append(batch, Message{Body: r.Value, ID: id, Meta: map[string]any{
				"transport": "kafka", "topic": r.Topic, "partition": int(r.Partition), "offset": r.Offset,
				"key": string(r.Key), "timestamp": r.Timestamp.UTC().Format(time.RFC3339Nano),
			}})
			recs = append(recs, r)
		})
		if len(batch) == 0 {
			cl.AllowRebalance()
			if failed {
				sleepCtx(ctx, time.Second)
			}
			continue
		}
		// queue the batch before committing its offsets; keep trying while the queue refuses
		for attempt := 0; ; attempt++ {
			err := emit(ctx, batch)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return nil
			}
			k.log.Warn("kafka batch not queued yet; retrying (offsets stay uncommitted)", "records", len(batch), "error", err.Error())
			sleepCtx(ctx, min(200*time.Millisecond<<min(attempt, 5), 5*time.Second))
		}
		if err := cl.CommitRecords(ctx, recs...); err != nil && ctx.Err() == nil {
			k.log.Warn("kafka commit failed; the records may be read again (the queue deduplicates them)", "error", err.Error())
		}
		cl.AllowRebalance()
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// stringList reads a list of strings or a comma-separated string.
func stringList(v any) []string {
	var out []string
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case []string:
		for _, s := range t {
			if strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case string:
		for _, s := range strings.Split(t, ",") {
			if strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}
