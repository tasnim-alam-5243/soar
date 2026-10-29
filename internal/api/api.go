// Package api serves the REST API, auth, CRUD and search.
// In Step 1 it only starts the shared health server; real behaviour arrives in later steps.
package api

import (
	"context"
	"log/slog"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/service"
)

// Run blocks until ctx is cancelled or a fatal error occurs.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	return service.Run(ctx, "api", cfg, log)
}
