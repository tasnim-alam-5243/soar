// Package seed installs the built-in source packs (`soar seed`).
package seed

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/yourorg/soar/integrations"
	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/ingest/pack"
	"github.com/yourorg/soar/internal/service"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Run is "soar seed": install the built-in source packs, then exit. It is idempotent and
// never overwrites what operators changed. If SOAR_WAZUH_INDEXER_URL is set it also
// creates the wazuh_indexer connection and enables the wazuh-pull source.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	log.Info("starting", "config", cfg)
	osc := opensearch.Open(cfg)
	if err := service.WaitOpenSearch(ctx, osc, log); err != nil {
		return err
	}
	st := opensearch.NewStore(osc, cfg.IndexPrefix, opensearch.Options{Refresh: "true"})

	b, err := pack.Load(integrations.Packs, "wazuh")
	if err != nil {
		return err
	}
	rep, err := pack.Install(ctx, st, b)
	for _, l := range rep.Lines {
		log.Info("pack", "wazuh", l)
	}
	if err != nil {
		return err
	}

	url := os.Getenv("SOAR_WAZUH_INDEXER_URL")
	if url == "" {
		log.Info("wazuh pull stays disabled: set SOAR_WAZUH_INDEXER_URL (and user/password) to enable it")
		return nil
	}
	conn := domain.Connection{
		Meta: domain.Meta{ID: domain.DeterministicID("con", "wazuh_indexer")}, ConnectorID: "wazuh-indexer", Name: "wazuh_indexer",
		AuthType: "basic", SecretRefs: domain.JSON{"username": "env:SOAR_WAZUH_INDEXER_USER", "password": "env:SOAR_WAZUH_INDEXER_PASSWORD"},
		Config: domain.JSON{"base_url": url},
	}
	if ca := os.Getenv("SOAR_WAZUH_INDEXER_CA_FILE"); ca != "" {
		conn.Config["ca_file"] = ca
	}
	if os.Getenv("SOAR_WAZUH_INDEXER_INSECURE") == "true" {
		conn.Config["insecure_skip_verify"] = true // development only; the collector logs a warning
	}
	if _, err := st.Connections.Put(ctx, conn); err != nil { // the connection is configuration owned by the environment
		return err
	}
	_, _, err = st.Sources.Update(ctx, domain.DeterministicID(domain.PrefixSource, "wazuh-pull"), func(s *domain.Source) error {
		s.Enabled = true
		s.Auth = domain.JSON{"type": "none"} // pull sources have no inbound auth
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	log.Info("wazuh pull enabled", "indexer", url)
	return nil
}
