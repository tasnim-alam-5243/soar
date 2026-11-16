package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/store/opensearch"
)

// RunRole serves a role's HTTP endpoints (health plus whatever mount adds) and runs its
// background loops. If any loop returns an error the whole role shuts down and the
// error is returned, so the container restarts instead of limping along half-working.
func RunRole(ctx context.Context, role string, cfg config.Config, log *slog.Logger,
	mount func(mux *http.ServeMux, osc *opensearch.Client), background ...func(context.Context) error,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	fail := func(err error) {
		mu.Lock()
		if first == nil {
			first = err
		}
		mu.Unlock()
		cancel()
	}
	for i, fn := range background {
		wg.Add(1)
		go func(i int, fn func(context.Context) error) {
			defer wg.Done()
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				fail(fmt.Errorf("%s background task %d: %w", role, i, err))
			}
		}(i, fn)
	}
	err := RunWith(ctx, role, cfg, log, mount)
	cancel()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if first != nil {
		return first
	}
	return err
}

// ConnectQueue connects to NATS (waiting for it to come up) and makes sure the streams exist.
func ConnectQueue(ctx context.Context, cfg config.Config, log *slog.Logger, name string) (*queue.Client, error) {
	deadline := time.Now().Add(2 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		q, err := queue.Connect(cfg.NATSURLs, name, NATSOptions(cfg)...)
		if err == nil {
			ectx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err = q.EnsureStreams(ectx, queue.StreamOptions{Replicas: cfg.NATSReplicas, EventsMaxBytes: cfg.EventsMaxBytes})
			if err == nil {
				err = q.EnsureFlowStreams(ectx, queue.FlowOptions{Replicas: cfg.NATSReplicas})
			}
			cancel()
			if err == nil {
				return q, nil
			}
			q.Close()
		}
		lastErr = err
		log.Warn("waiting for NATS", "error", err.Error())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("nats not ready: %w", lastErr)
}

// Every runs fn now and then every interval until ctx ends.
func Every(ctx context.Context, interval time.Duration, fn func(context.Context)) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// NATSOptions returns the client options the configuration asks for (TLS with a
// private CA or a client certificate).
func NATSOptions(cfg config.Config) []nats.Option {
	if cfg.NATSTLS == nil {
		return nil
	}
	return []nats.Option{nats.Secure(cfg.NATSTLS.Clone())}
}
