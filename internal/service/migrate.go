package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/store/opensearch"
	"github.com/yourorg/soar/internal/store/opensearch/migrate"
	"github.com/yourorg/soar/internal/store/opensearch/schema"
)

// Migrate is "soar migrate": create or update every index template, index and
// retention policy, then exit. It is safe to run any number of times.
func Migrate(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	return runMigrate(ctx, cfg, log, false)
}

// MigrateCheck is "soar migrate-check": compare the live cluster with the schema
// and change nothing. It exits non-zero when anything has drifted.
func MigrateCheck(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	return runMigrate(ctx, cfg, log, true)
}

func runMigrate(ctx context.Context, cfg config.Config, log *slog.Logger, checkOnly bool) error {
	log.Info("starting", "config", cfg, "check_only", checkOnly)

	retention, err := schema.LoadRetention(cfg.RetentionFile)
	if err != nil {
		return err
	}
	osc := opensearch.New(cfg.OSURLs, cfg.OSUser, cfg.OSPassword)
	if err := waitOpenSearch(ctx, osc, log); err != nil {
		return err
	}

	r := &migrate.Runner{
		C:   osc,
		Log: log,
		Opts: schema.Options{
			Prefix:             cfg.IndexPrefix,
			StateReplicas:      cfg.StateReplicas,
			TimeSeriesReplicas: cfg.TSReplicas,
			Retention:          retention,
		},
	}
	var results []migrate.Result
	if checkOnly {
		results, err = r.Check(ctx)
	} else {
		results, err = r.Apply(ctx)
	}
	if err != nil {
		return err
	}

	counts := map[string]int{}
	for _, res := range results {
		counts[res.Action]++
		attrs := []any{"entity", res.Entity, "index", res.Alias, "action", res.Action}
		if res.Detail != "" {
			attrs = append(attrs, "detail", res.Detail)
		}
		if res.Failed() {
			log.Error("index problem", attrs...)
		} else {
			log.Info("index ok", attrs...)
		}
	}
	failed := counts["DRIFT"] + counts["error"]
	log.Info("migrate finished", "indices", len(results), "created", counts["created"], "updated", counts["updated"],
		"unchanged", counts["unchanged"], "ok", counts["ok"], "failed", failed)
	if failed > 0 {
		return fmt.Errorf("%d of %d indices failed (see log lines above)", failed, len(results))
	}
	return nil
}

func waitOpenSearch(ctx context.Context, osc *opensearch.Client, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		err := osc.Ping(ctx)
		if err == nil {
			return nil
		}
		log.Warn("waiting for opensearch", "error", err.Error())
		select {
		case <-ctx.Done():
			return fmt.Errorf("opensearch not ready: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
}
