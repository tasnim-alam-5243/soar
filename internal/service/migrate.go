package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Migrate is the "soar migrate" command. In Step 1 it only waits for OpenSearch and exits 0;
// Step 2 adds index templates and migrations.
func Migrate(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	log.Info("starting", "config", cfg)
	osc := opensearch.New(cfg.OSURLs, cfg.OSUser, cfg.OSPassword)

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		err := osc.Ping(ctx)
		if err == nil {
			break
		}
		log.Warn("waiting for opensearch", "error", err.Error())
		select {
		case <-ctx.Done():
			return fmt.Errorf("opensearch not ready: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
	log.Info("migrate finished", "applied", 0, "note", "no migrations defined yet")
	return nil
}
