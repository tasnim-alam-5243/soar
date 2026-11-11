package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/platform"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/service"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// Run serves the REST API and the health endpoints until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	_, st := platform.Store(cfg)
	reg, err := platform.Registry(cfg, st, log, nil)
	if err != nil {
		return err
	}
	secrets, err := platform.Secrets(cfg, st, log)
	if err != nil {
		return err
	}
	bl, err := platform.Blob(cfg)
	if err != nil {
		return err
	}
	s := New(st, log).WithSecrets(secrets).WithBlob(bl)
	s.partitions = cfg.Partitions

	var qc *queue.Client
	if c, err := service.ConnectQueue(ctx, cfg, log, "soar-api"); err != nil {
		log.Warn("API starts without NATS: running workflows, /dlq and /queues will answer 503", "error", err.Error())
	} else {
		qc = c
		defer qc.Close()
		s.WithQueue(qc).WithEngine(reg, qc, cfg.Partitions)
		stop, err := qc.SubscribeCore("ui.>", func(subject string, data []byte) {
			var ev UIEvent
			if json.Unmarshal(data, &ev) == nil {
				if ev.Topic == "" {
					ev.Topic = strings.TrimPrefix(subject, "ui.")
				}
				s.events.Publish(ev)
			}
		})
		if err == nil {
			defer stop()
		}
	}
	if s.reg == nil {
		s.reg = reg
	}

	if cfg.AuthDisabled {
		log.Warn("AUTHENTICATION IS DISABLED (SOAR_AUTH_DISABLED=true): every request acts as an admin. Never expose this API.")
	} else {
		a := &security.Authenticator{Principals: st.Principals, Audience: cfg.OIDCAudience}
		if cfg.OIDCIssuer != "" {
			a.OIDC = &security.OIDCVerifier{Issuer: cfg.OIDCIssuer, Audience: cfg.OIDCAudience, JWKSURL: cfg.OIDCJWKSURL}
		} else {
			log.Warn("SOAR_OIDC_ISSUER is not set: only API keys can sign in")
		}
		s.WithAuth(a)
		bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		for i := 0; i < 15; i++ {
			var disabled bool
			if disabled, err = EnsureBootstrapKey(bctx, st, cfg.BootstrapKey); err == nil {
				if disabled {
					log.Warn("SOAR_BOOTSTRAP_API_KEY is set but its principal is disabled; it stays disabled (remove the variable)")
				} else {
					log.Warn("SOAR_BOOTSTRAP_API_KEY grants admin rights: remove it and restart the api once real admins exist")
				}
				break
			}
			time.Sleep(2 * time.Second)
		}
		cancel()
		if err != nil {
			log.Error("could not install the bootstrap API key", "error", err.Error())
		}
	}
	return service.RunWith(ctx, "api", cfg, log, func(mux *http.ServeMux, _ *opensearch.Client) {
		s.Mount(mux)
		s.MountCORS(mux, cfg.CORSOrigins)
	})
}
