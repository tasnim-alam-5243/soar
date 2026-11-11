package obs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestMetricsRegistryServes(t *testing.T) {
	Executions.WithLabelValues("succeeded", "phishing_triage").Inc()
	TimerLag.WithLabelValues().Set(1.5)
	StepDuration.WithLabelValues("http.request", "succeeded").Observe(0.2)
	if _, err := Registry.Gather(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`soar_executions_total{status="succeeded",workflow="phishing_triage"} 1`,
		`soar_timer_lag_seconds 1.5`,
		`soar_step_duration_seconds_bucket{action="http.request",status="succeeded",le="0.25"} 1`,
		"go_goroutines",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %q", want)
		}
	}
}
