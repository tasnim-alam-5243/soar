package collector

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/ingest"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/service"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Run polls the enabled pull sources until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	osc := opensearch.Open(cfg)
	st := opensearch.NewStore(osc, cfg.IndexPrefix, opensearch.Options{})
	q, err := service.ConnectQueue(ctx, cfg, log, "soar-collector")
	if err != nil {
		return err
	}
	defer q.Close()

	host, _ := os.Hostname()
	ss, err := security.FromConfig(cfg.MasterKeyID, cfg.MasterKey, cfg.OldMasterKeys, st.Secrets, func(m string) { log.Warn(m) })
	if err != nil {
		return err
	}
	reg := ingest.NewRegistry(st, log, ss.ResolveFunc())
	reg.Serves = ingest.IsPull
	col := &Collector{Reg: reg, Q: q, St: st, Log: log, Secrets: ss.ResolveFunc(), Owner: host + "-" + domain.NewULID(time.Now())[:8]}
	return service.RunRole(ctx, "collector", cfg, log, func(*http.ServeMux, *opensearch.Client) {},
		func(ctx context.Context) error { return reg.Run(ctx, 10*time.Second) },
		func(ctx context.Context) error { return col.Run(ctx) },
		func(ctx context.Context) error {
			return service.Every(ctx, 15*time.Second, func(ctx context.Context) { reg.ReportHealth(ctx, nil) })
		},
	)
}
