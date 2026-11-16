// Package scheduler runs the periodic jobs of the platform (spec section 2): due
// timers, the outbox and queued-execution sweepers, cron triggers, retention
// cleanup and TTL purges. Only the replica holding the leader lease runs them.
package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/orchestrator"
	"github.com/yourorg/soar/internal/platform"
	"github.com/yourorg/soar/internal/service"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// SeenTTL is how long an exact-duplicate key is remembered (spec: soar-seen, 24 h).
const SeenTTL = 24 * time.Hour

// Run blocks until ctx is cancelled or a fatal error occurs.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	osc, st := platform.Store(cfg)
	q, err := service.ConnectQueue(ctx, cfg, log, "soar-scheduler")
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
	orch := &orchestrator.Orchestrator{Q: q, St: st, Registry: reg, Workflows: orchestrator.NewWorkflowCache(st), Blob: bl, Log: log, Partitions: cfg.Partitions}
	jobs := &Jobs{St: st, Q: q, Orch: orch, Log: log, Partitions: cfg.Partitions}
	host, _ := os.Hostname()
	lease := &Lease{St: st, Name: "scheduler", Owner: host + "-" + domain.NewULID(time.Now())[:8], TTL: 15 * time.Second}
	cron := &Cron{St: st, Q: q, Log: log, Partitions: cfg.Partitions}
	ret := &Retention{OS: osc, St: st, Blob: bl, Prefix: cfg.IndexPrefix, File: cfg.RetentionFile, Log: log}

	var leading atomic.Bool
	var timerLag atomic.Int64
	leader := func(ctx context.Context) error {
		defer lease.Release(context.Background())
		return service.Every(ctx, 5*time.Second, func(ctx context.Context) {
			ok, err := lease.Acquire(ctx)
			if err != nil {
				log.Warn("leader lease check failed", "error", err.Error())
				ok = false
			}
			if ok != leading.Load() {
				log.Info("leadership changed", "leading", ok, "owner", lease.Owner)
				if ok {
					obs.LeaseTakeovers.WithLabelValues("scheduler").Inc()
				}
			}
			leading.Store(ok)
		})
	}
	whenLeading := func(every time.Duration, name string, fn func(context.Context) error) func(context.Context) error {
		return func(ctx context.Context) error {
			return service.Every(ctx, every, func(ctx context.Context) {
				if !leading.Load() {
					return
				}
				if err := fn(ctx); err != nil && ctx.Err() == nil {
					log.Warn("scheduler job failed", "job", name, "error", err.Error())
				}
			})
		}
	}
	return service.RunRole(ctx, "scheduler", cfg, log, func(mux *http.ServeMux, _ *opensearch.Client) {
		mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"leading": leading.Load(), "timer_lag_seconds": float64(timerLag.Load()) / 1e9})
		})
	},
		leader,
		whenLeading(time.Second, "timers", func(ctx context.Context) error {
			for {
				n, err := jobs.FireDueTimers(ctx, 500)
				if err != nil || n < 500 {
					if lag, lerr := jobs.OldestTimerLag(ctx); lerr == nil {
						timerLag.Store(int64(lag))
						obs.TimerLag.WithLabelValues().Set(lag.Seconds())
					}
					return err
				}
			}
		}),
		whenLeading(5*time.Second, "outbox-sweeper", func(ctx context.Context) error {
			n, err := jobs.SweepOutbox(ctx, 10*time.Second, 200)
			obs.OutboxUnpublished.WithLabelValues().Set(float64(n))
			return err
		}),
		whenLeading(15*time.Second, "queued-sweeper", func(ctx context.Context) error {
			_, err := jobs.SweepQueued(ctx, 30*time.Second, 200)
			return err
		}),
		whenLeading(30*time.Second, "cron", cron.Tick),
		whenLeading(15*time.Second, "queue-gauges", func(ctx context.Context) error {
			stats, err := q.Stats(ctx)
			if err != nil {
				return err
			}
			for _, s := range stats {
				var pending float64
				for _, c := range s.Consumers {
					pending += float64(c.Pending) + float64(c.AckPending)
				}
				obs.QueuePending.WithLabelValues(s.Name).Set(pending)
				if s.MaxBytes > 0 {
					obs.QueueBytesRatio.WithLabelValues(s.Name).Set(float64(s.Bytes) / float64(s.MaxBytes))
				}
			}
			return nil
		}),
		whenLeading(10*time.Minute, "ttl-purge", func(ctx context.Context) error {
			n, err := opensearch.PurgeExpired(ctx, osc, cfg.IndexPrefix, time.Now().UTC(), SeenTTL)
			if err == nil && n > 0 {
				log.Info("ttl purge", "deleted", n)
			}
			return err
		}),
		whenLeading(15*time.Minute, "retention", ret.Run),
	)
}
