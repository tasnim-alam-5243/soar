// Package service holds the plumbing every role shares in Step 1:
// dependency checks, the health server and one-shot startup tasks.
package service

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Run serves /healthz and /readyz for a role until ctx is cancelled.
func Run(ctx context.Context, role string, cfg config.Config, log *slog.Logger) error {
	return RunWith(ctx, role, cfg, log, nil)
}

// RunWith is Run plus a hook that adds the role's own routes. The hook receives
// the mux and the shared OpenSearch client.
func RunWith(ctx context.Context, role string, cfg config.Config, log *slog.Logger, mount func(mux *http.ServeMux, osc *opensearch.Client)) error {
	log.Info("starting", "config", cfg)

	osc := opensearch.Open(cfg)
	h := obs.NewHealth(log, 2*time.Second, checks(cfg, osc)...)
	mux := http.NewServeMux()
	h.Routes(mux)
	mux.Handle("GET /metrics", promhttp.HandlerFor(obs.Registry, promhttp.HandlerOpts{}))
	if mount != nil {
		mount(mux, osc)
	}

	return obs.ServeTLS(ctx, log, cfg.AddrFor(role), obs.Middleware(log, mux), cfg.ServerTLS, cfg.ClientCertReq)
}

func checks(cfg config.Config, osc *opensearch.Client) []obs.Check {
	return []obs.Check{
		{Name: "opensearch", Fn: osc.Ping},
		{Name: "nats", Fn: func(ctx context.Context) error { return queue.Ping(ctx, cfg.NATSURLs, true) }},
	}
}
