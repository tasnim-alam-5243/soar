package processor

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/service"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Run consumes the EVENTS stream until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	osc := opensearch.Open(cfg)
	st := opensearch.NewStore(osc, cfg.IndexPrefix, opensearch.Options{})
	q, err := service.ConnectQueue(ctx, cfg, log, "soar-processor")
	if err != nil {
		return err
	}
	defer q.Close()
	cons, err := q.EventsConsumer(ctx)
	if err != nil {
		return err
	}
	triggers, err := NewTriggerIndex(st, log)
	if err != nil {
		return err
	}
	waits, err := NewWaitIndex(st, log)
	if err != nil {
		return err
	}
	if err := waits.Refresh(ctx); err != nil {
		log.Warn("could not load event waits yet; retrying in the background", "error", err.Error())
	}
	if err := triggers.Refresh(ctx); err != nil {
		log.Warn("could not load workflow triggers yet; retrying in the background", "error", err.Error())
	}
	p := &Processor{
		Cons: cons, Bulk: opensearch.NewBulk(osc, opensearch.BulkConfig{}), St: st, DLQ: q, Triggers: triggers, Waits: waits, Starts: q, UI: q, Log: log,
		Cfg: Config{Workers: cfg.ProcessorWorkers, Partitions: cfg.Partitions},
	}
	return service.RunRole(ctx, "processor", cfg, log, func(*http.ServeMux, *opensearch.Client) {},
		func(ctx context.Context) error { triggers.Run(ctx, 10*time.Second); return nil },
		func(ctx context.Context) error { waits.Run(ctx, 2*time.Second); return nil },
		func(ctx context.Context) error {
			return q.MoveMaxDeliveries(ctx, func(seq uint64) {
				obs.DLQMessages.WithLabelValues("events").Inc()
				log.Warn("poison message moved to the DLQ", "stream_seq", seq)
			})
		},
		p.Run,
	)
}
