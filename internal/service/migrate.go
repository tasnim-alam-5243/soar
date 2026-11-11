package service

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/blob"
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
	osc := opensearch.Open(cfg)
	if err := WaitOpenSearch(ctx, osc, log); err != nil {
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
	if !checkOnly && cfg.BlobEndpoint != "" {
		if err := ensureBuckets(ctx, cfg, retention, log); err != nil {
			return err
		}
	}
	return nil
}

// ensureBuckets creates the object-storage buckets of spec 5.1 with expiry lifecycle
// rules generated from retention.yaml. Evidence has no expiry rule: the scheduler
// deletes it together with its case.
func ensureBuckets(ctx context.Context, cfg config.Config, ret schema.Retention, log *slog.Logger) error {
	s3, err := blob.NewS3(cfg.BlobEndpoint, cfg.BlobAccessKey, cfg.BlobSecretKey, cfg.BlobRegion, cfg.BlobSSE)
	if err != nil {
		return err
	}
	days := func(class string) int {
		v := ret.For(class)
		if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil && strings.HasSuffix(v, "d") {
			return n
		}
		return 7
	}
	for bucket, d := range map[string]int{blob.BucketRaw: days("raw"), blob.BucketPayloads: days("steprun"), blob.BucketExports: days("default"), blob.BucketEvidence: 0} {
		var lastErr error
		for i := 0; i < 30; i++ { // the object store may still be starting
			if lastErr = s3.EnsureBucket(ctx, bucket, d); lastErr == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if lastErr != nil {
			return fmt.Errorf("bucket %s: %w", bucket, lastErr)
		}
		log.Info("bucket ok", "bucket", bucket, "expire_days", d)
	}
	return nil
}

// WaitOpenSearch blocks until OpenSearch answers (up to two minutes).
func WaitOpenSearch(ctx context.Context, osc *opensearch.Client, log *slog.Logger) error {
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
