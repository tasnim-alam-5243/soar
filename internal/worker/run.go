package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/audit"
	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/platform"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/service"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Run executes steps until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	_, st := platform.Store(cfg)
	q, err := service.ConnectQueue(ctx, cfg, log, "soar-worker")
	if err != nil {
		return err
	}
	defer q.Close()
	bl, err := platform.Blob(cfg)
	if err != nil {
		return err
	}
	rc, err := platform.Runner(cfg)
	if err != nil {
		return err
	}
	var exec actions.Executor
	if rc != nil {
		defer rc.Close()
		exec = rc
	} else {
		log.Warn("SOAR_RUNNER_ADDR is not set: python (integration) actions will fail with runner_unavailable")
	}
	reg, err := platform.Registry(cfg, st, log, exec)
	if err != nil {
		return err
	}
	secrets, err := platform.Secrets(cfg, st, log)
	if err != nil {
		return err
	}
	n := cfg.WorkerConcurrency
	w := &Worker{Q: q, St: st, Registry: reg, Secrets: secrets, Blob: bl, Log: log, Audit: &audit.Auditor{Sink: st.Audit, Log: log}, Cfg: Config{
		Priorities: cfg.WorkerPriorities, Classes: cfg.WorkerClasses,
		Concurrency: map[string]int{"normal": n, "critical": max(n/2, 2), "bulk": max(n/4, 1)},
	}}
	consumers := map[[2]string]jetstream.Consumer{}
	for _, pc := range w.Consumers() {
		cons, err := q.StepsConsumer(ctx, pc[0], pc[1])
		if err != nil {
			return err
		}
		consumers[pc] = cons
	}
	var extra []obs.Check
	if rc != nil {
		extra = append(extra, obs.Check{Name: "runner", Fn: rc.Ping})
	}
	_ = extra
	return service.RunRole(ctx, "worker", cfg, log, func(mux *http.ServeMux, _ *opensearch.Client) {
		mux.HandleFunc("GET /stats", func(rw http.ResponseWriter, _ *http.Request) {
			rw.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(rw).Encode(map[string]int64{
				"executed": w.Stats.Executed.Load(), "succeeded": w.Stats.Succeeded.Load(),
				"failed": w.Stats.Failed.Load(), "replayed": w.Stats.Replayed.Load(), "retried": w.Stats.Retried.Load(),
			})
		})
	},
		func(ctx context.Context) error { return w.Run(ctx, consumers) },
		func(ctx context.Context) error {
			return q.MoveExhausted(ctx, queue.StepsStream, "steps", w.OnDeadLetter)
		},
	)
}
