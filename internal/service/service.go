// Package service holds the plumbing every role shares in Step 1:
// dependency checks, the health server and one-shot startup tasks.
package service

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Run serves /healthz and /readyz for a role until ctx is cancelled.
func Run(ctx context.Context, role string, cfg config.Config, log *slog.Logger) error {
	log.Info("starting", "config", cfg)

	h := obs.NewHealth(log, 2*time.Second, checks(cfg)...)
	mux := http.NewServeMux()
	h.Routes(mux)

	return obs.Serve(ctx, log, cfg.AddrFor(role), obs.Middleware(log, mux))
}

func checks(cfg config.Config) []obs.Check {
	osc := opensearch.New(cfg.OSURLs, cfg.OSUser, cfg.OSPassword)
	return []obs.Check{
		{Name: "opensearch", Fn: osc.Ping},
		{Name: "nats", Fn: func(ctx context.Context) error { return queue.Ping(ctx, cfg.NATSURLs, true) }},
	}
}
