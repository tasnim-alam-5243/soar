package obs

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Serve runs h on addr until ctx is cancelled, then shuts down gracefully (10 s).
func Serve(ctx context.Context, log *slog.Logger, addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Info("http server listening", "addr", ln.Addr().String())

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
