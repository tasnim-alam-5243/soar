package obs

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Check is one readiness dependency.
type Check struct {
	Name string
	Fn   func(context.Context) error
}

// Health serves /healthz (process alive) and /readyz (dependencies reachable).
type Health struct {
	checks  []Check
	timeout time.Duration
	log     *slog.Logger
}

// NewHealth builds a Health. Each check gets timeout to finish; checks run in parallel.
func NewHealth(log *slog.Logger, timeout time.Duration, checks ...Check) *Health {
	return &Health{checks: checks, timeout: timeout, log: log}
}

// Routes registers /healthz and /readyz on mux.
func (h *Health) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", h.ready)
}

func (h *Health) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	errs := make([]error, len(h.checks))
	var wg sync.WaitGroup
	for i, c := range h.checks {
		wg.Add(1)
		go func(i int, c Check) {
			defer wg.Done()
			errs[i] = c.Fn(ctx)
		}(i, c)
	}
	wg.Wait()

	status, code := "ok", http.StatusOK
	results := make(map[string]string, len(h.checks))
	for i, c := range h.checks {
		if errs[i] != nil {
			results[c.Name] = "fail"
			status, code = "unavailable", http.StatusServiceUnavailable
			h.log.WarnContext(r.Context(), "readiness check failed", "check", c.Name, "error", errs[i].Error())
			continue
		}
		results[c.Name] = "ok"
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": results})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
