package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/service"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Run serves the REST API and the health endpoints until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	return service.RunWith(ctx, "api", cfg, log, func(mux *http.ServeMux, osc *opensearch.Client) {
		st := opensearch.NewStore(osc, cfg.IndexPrefix, opensearch.Options{})
		New(st, log).Mount(mux)
	})
}
