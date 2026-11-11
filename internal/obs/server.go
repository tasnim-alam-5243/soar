package obs

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Serve runs h on addr until ctx is cancelled, then shuts down gracefully (10 s).
func Serve(ctx context.Context, log *slog.Logger, addr string, h http.Handler) error {
	return ServeTLS(ctx, log, addr, h, nil, false)
}

// ServeTLS is Serve over HTTPS when tc is not nil. With requireClientCert every
// request except the /healthz and /readyz probes must present a client certificate
// that tc.ClientCAs verified (mTLS).
func ServeTLS(ctx context.Context, log *slog.Logger, addr string, h http.Handler, tc *tls.Config, requireClientCert bool) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if tc != nil {
		ln = tls.NewListener(ln, tc.Clone())
		if requireClientCert {
			h = RequireClientCert(h)
		}
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Info("http server listening", "addr", ln.Addr().String(), "tls", tc != nil, "client_certificates", tc != nil && requireClientCert)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		return err
	}
	log.Info("http server stopped")
	return nil
}

// RequireClientCert refuses requests without a verified client certificate, except
// the liveness and readiness probes (container healthchecks carry no certificate).
func RequireClientCert(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && (r.TLS == nil || len(r.TLS.VerifiedChains) == 0) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Client certificate required","status":401,"detail":"this endpoint requires a client certificate signed by the configured CA"}`))
			return
		}
		h.ServeHTTP(w, r)
	})
}
