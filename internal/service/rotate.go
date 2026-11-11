package service

import (
	"context"
	"log/slog"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// RotateSecrets is "soar rotate-secrets": re-wrap every secret's data key with the active
// master key (SOAR_MASTER_KEY_ID), using SOAR_OLD_MASTER_KEYS to open the old ones.
func RotateSecrets(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	osc := opensearch.Open(cfg)
	if err := WaitOpenSearch(ctx, osc, log); err != nil {
		return err
	}
	st := opensearch.NewStore(osc, cfg.IndexPrefix, opensearch.Options{})
	ss, err := security.FromConfig(cfg.MasterKeyID, cfg.MasterKey, cfg.OldMasterKeys, st.Secrets, func(m string) { log.Warn(m) })
	if err != nil {
		return err
	}
	n, err := ss.RotateAll(ctx)
	if err != nil {
		return err
	}
	log.Info("secrets re-wrapped with the active master key", "key_id", cfg.MasterKeyID, "changed", n)
	return nil
}
