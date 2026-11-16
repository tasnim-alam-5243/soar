package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/platform"
	"github.com/yourorg/soar/internal/policy"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/service"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Run consumes every partition's results until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	_, st := platform.Store(cfg)
	q, err := service.ConnectQueue(ctx, cfg, log, "soar-orchestrator")
	if err != nil {
		return err
	}
	defer q.Close()
	bl, err := platform.Blob(cfg)
	if err != nil {
		return err
	}
	reg, err := platform.Registry(cfg, st, log, nil)
	if err != nil {
		return err
	}
	pol, err := policy.Load(cfg.PolicyFile)
	if err != nil {
		return err
	}
	o := &Orchestrator{
		Q: q, St: st, Registry: reg, Workflows: NewWorkflowCache(st), Blob: bl, Log: log, Partitions: cfg.Partitions,
		Policy: pol,
	}
	var loops []func(context.Context) error
	for p := 0; p < cfg.Partitions; p++ {
		cons, err := q.ResultsConsumer(ctx, p)
		if err != nil {
			return err
		}
		loops = append(loops, func(ctx context.Context) error { o.RunPartition(ctx, cons, 64); return nil })
	}
	loops = append(loops, func(ctx context.Context) error {
		return q.MoveExhausted(ctx, queue.ResultsStream, "results", func(_ context.Context, dl queue.DeadLetter) {
			obs.DLQMessages.WithLabelValues("results").Inc()
			log.Error("orchestrator message dead-lettered; its execution may stall until the sweeper or deadline", "subject", dl.Subject, "seq", dl.Seq)
		})
	})
	return service.RunRole(ctx, "orchestrator", cfg, log, func(mux *http.ServeMux, _ *opensearch.Client) {
		mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]int64{
				"handled": o.Stats.Handled.Load(), "ignored": o.Stats.Ignored.Load(),
				"conflicts": o.Stats.Conflicts.Load(), "published": o.Stats.Published.Load(),
			})
		})
	}, loops...)
}

// Msg is the part of jetstream.Msg the loop uses.
type Msg interface {
	Data() []byte
	InProgress() error
	NakWithDelay(time.Duration) error
	Term() error
	DoubleAck(context.Context) error
}

// RunPartition pulls batches from one partition's consumer. Messages of one execution
// are applied in order; different executions run concurrently.
func (o *Orchestrator) RunPartition(ctx context.Context, cons jetstream.Consumer, batch int) {
	for ctx.Err() == nil {
		b, err := cons.Fetch(batch, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			if ctx.Err() == nil {
				o.Log.Warn("fetch failed", "error", err.Error())
				sleep(ctx, time.Second)
			}
			continue
		}
		var msgs []Msg
		for m := range b.Messages() {
			msgs = append(msgs, m)
		}
		o.HandleBatch(ctx, msgs)
	}
}

// HandleBatch groups messages by execution and settles every message.
func (o *Orchestrator) HandleBatch(ctx context.Context, msgs []Msg) {
	type item struct {
		m   Msg
		env queue.Envelope
	}
	groups := map[string][]item{}
	var order []string
	for _, m := range msgs {
		var env queue.Envelope
		if err := json.Unmarshal(m.Data(), &env); err != nil || env.ExecutionID == "" {
			o.Log.Error("invalid orchestrator message; terminating it")
			_ = m.Term()
			continue
		}
		if _, ok := groups[env.ExecutionID]; !ok {
			order = append(order, env.ExecutionID)
		}
		groups[env.ExecutionID] = append(groups[env.ExecutionID], item{m, env})
	}
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, id := range order {
		items := groups[id]
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			for _, it := range items {
				err := o.Handle(ctx, it.env)
				if errors.Is(err, ErrRetry) && store.Unavailable(err) && ctx.Err() == nil {
					err = o.holdWhileUnavailable(ctx, it.m, it.env, err)
				}
				switch {
				case err == nil:
					actx, cancel := context.WithTimeout(ctx, 5*time.Second)
					if aerr := it.m.DoubleAck(actx); aerr != nil {
						o.Log.Warn("ack not confirmed; a redelivery is harmless", "error", aerr.Error())
					}
					cancel()
				case errors.Is(err, ErrRetry):
					o.Log.Warn("will retry message", "execution_id", id, "type", it.env.Type, "error", err.Error())
					_ = it.m.NakWithDelay(2 * time.Second)
				default:
					o.Log.Error("message failed", "execution_id", id, "type", it.env.Type, "error", err.Error())
					_ = it.m.NakWithDelay(5 * time.Second)
				}
			}
		}()
	}
	wg.Wait()
}

// holdWhileUnavailable retries a message in place while OpenSearch is unavailable,
// keeping it alive with InProgress (AckWait is 30 s). Nak-ing it instead spent one of
// its 10 deliveries every 2 s, so an outage of ~20 s dead-lettered step results and
// left executions stuck. The partition's next fetch waits for this batch: the
// orchestrator pauses and JetStream keeps the rest.
func (o *Orchestrator) holdWhileUnavailable(ctx context.Context, m Msg, env queue.Envelope, err error) error {
	o.Stats.Held.Add(1)
	obs.StoreHolds.WithLabelValues("orchestrator").Inc()
	o.Log.Warn("OpenSearch is unavailable: holding the message until it answers again", "execution_id", env.ExecutionID, "type", env.Type, "error", err.Error())
	held := time.Now()
	stop := store.Heartbeat(10*time.Second, func() { _ = m.InProgress() }) // also during an attempt that hangs
	defer stop()
	wait := store.Waiter{Max: o.HoldMax}
	for errors.Is(err, ErrRetry) && store.Unavailable(err) && wait.Wait(ctx) == nil {
		err = o.Handle(ctx, env)
	}
	if err == nil {
		o.Log.Info("OpenSearch answers again: message applied", "execution_id", env.ExecutionID, "held_for", time.Since(held).Round(time.Second).String())
	}
	return err
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// PartitionName is used in logs and metrics.
func PartitionName(p int) string { return "p" + strconv.Itoa(p) }
