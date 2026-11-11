// Package platform builds the shared dependencies of the service roles from the
// configuration (store, object storage, action registry, secret store, runner), so
// every role wires them the same way.
package platform

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/yourorg/soar/integrations"
	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/blob"
	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/runner"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Store opens the OpenSearch store.
func Store(cfg config.Config) (*opensearch.Client, *store.Store) {
	osc := opensearch.Open(cfg)
	return osc, opensearch.NewStore(osc, cfg.IndexPrefix, opensearch.Options{OnConflict: func(index string) {
		obs.OSWriteConflicts.WithLabelValues(index).Inc()
	}})
}

// Blob returns the object store: S3 when SOAR_BLOB_ENDPOINT is set, the
// filesystem when SOAR_BLOB_DIR is set, otherwise nil (large payloads stay inline).
func Blob(cfg config.Config) (blob.Blob, error) {
	switch {
	case cfg.BlobEndpoint != "":
		return blob.NewS3(cfg.BlobEndpoint, cfg.BlobAccessKey, cfg.BlobSecretKey, cfg.BlobRegion, cfg.BlobSSE)
	case cfg.BlobDir != "":
		if err := os.MkdirAll(cfg.BlobDir, 0o750); err != nil {
			return nil, fmt.Errorf("blob dir: %w", err)
		}
		return blob.FS{Root: cfg.BlobDir}, nil
	}
	return nil, nil
}

// Packs returns the integration packages: from SOAR_INTEGRATIONS_DIR, or the built-in ones.
func Packs(cfg config.Config) fs.FS {
	if cfg.IntegrationsDir != "" {
		return os.DirFS(cfg.IntegrationsDir)
	}
	return integrations.Packs
}

// Registry builds the action registry. exec runs python actions (nil in roles that
// only need manifests).
func Registry(cfg config.Config, st *store.Store, log *slog.Logger, exec actions.Executor) (*actions.Registry, error) {
	return actions.Build(actions.Deps{Store: st, Log: log, HTTP: actions.HTTPOptions{AllowLoopback: cfg.HTTPAllowLoopback}}, Packs(cfg), exec)
}

// Secrets returns the encrypted secret store. Without SOAR_MASTER_KEY only env:
// references resolve and secrets cannot be written.
func Secrets(cfg config.Config, st *store.Store, log *slog.Logger) (*security.SecretStore, error) {
	return security.FromConfig(cfg.MasterKeyID, cfg.MasterKey, cfg.OldMasterKeys, st.Secrets, func(m string) { log.Warn(m) })
}

// Runner connects to the Python runner, or returns nil when none is configured.
func Runner(cfg config.Config) (*runner.Client, error) {
	if cfg.RunnerAddr == "" {
		return nil, nil
	}
	return runner.Dial(cfg.RunnerAddr, cfg.RunnerTLS)
}
