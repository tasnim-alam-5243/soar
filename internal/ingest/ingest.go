// Package ingest turns vendor data into normalized events: parsers, a CEL mapping
// engine, filter sets and the HTTP push receiver. Nothing here waits on OpenSearch
// on the request path; events go to NATS first (queue-first ingestion, spec 3.3).
package ingest

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/service"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Run serves POST /ingest/{source} and the health endpoints until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	osc := opensearch.Open(cfg)
	st := opensearch.NewStore(osc, cfg.IndexPrefix, opensearch.Options{})
	q, err := service.ConnectQueue(ctx, cfg, log, "soar-ingest")
	if err != nil {
		return err
	}
	defer q.Close()

	ss, err := security.FromConfig(cfg.MasterKeyID, cfg.MasterKey, cfg.OldMasterKeys, st.Secrets, func(m string) { log.Warn(m) })
	if err != nil {
		return err
	}
	reg := NewRegistry(st, log, ss.ResolveFunc())
	reg.Serves = IsPush
	rc := &Receiver{Reg: reg, Q: q, St: st, Log: log}
	return service.RunRole(ctx, "ingest", cfg, log,
		func(mux *http.ServeMux, _ *opensearch.Client) { rc.Mount(mux) },
		func(ctx context.Context) error { return reg.Run(ctx, 10*time.Second) },
		func(ctx context.Context) error {
			return service.Every(ctx, 15*time.Second, func(ctx context.Context) { reg.ReportHealth(ctx, nil) })
		},
	)
}
