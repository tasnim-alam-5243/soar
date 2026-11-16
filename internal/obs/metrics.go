package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry holds every metric of the process (spec section 14). Each role serves it
// at /metrics on its health port.
var Registry = prometheus.NewRegistry()

func init() {
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	Registry.MustRegister(c)
	return c
}

func gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	Registry.MustRegister(g)
	return g
}

func histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	Registry.MustRegister(h)
	return h
}

var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// Metrics of spec section 14.
var (
	IngestEvents       = counter("soar_ingest_events_total", "Events accepted by ingest and the collector", "source")
	IngestLatency      = histogram("soar_ingest_latency_seconds", "Time from request to 202 at ingest", latencyBuckets, "source")
	Executions         = counter("soar_executions_total", "Executions that reached a final status", "status", "workflow")
	ExecutionDuration  = histogram("soar_execution_duration_seconds", "Execution run time", []float64{0.1, 0.5, 1, 5, 15, 60, 300, 1800, 3600, 21600, 86400}, "workflow")
	StepDuration       = histogram("soar_step_duration_seconds", "Action run time in the worker", latencyBuckets, "action", "status")
	StepRetries        = counter("soar_step_retries_total", "Step deliveries the worker could not finish and nak-ed", "action")
	StepReplays        = counter("soar_step_replays_total", "Redelivered steps answered from the idempotency store", "action")
	QueuePending       = gauge("soar_queue_pending", "Messages waiting or unacknowledged per stream", "stream")
	QueueBytesRatio    = gauge("soar_queue_fill_ratio", "Stream bytes divided by max_bytes", "stream")
	DLQMessages        = counter("soar_dlq_messages_total", "Messages moved to the dead-letter queue", "origin")
	OSWriteConflicts   = counter("soar_os_write_conflicts_total", "OpenSearch optimistic-concurrency conflicts", "index")
	LeaseTakeovers     = counter("soar_lease_takeovers_total", "Leader or source leases taken over from another replica", "lease")
	SourceEvents       = counter("soar_source_events_total", "Events per source and outcome", "source", "result")
	SourceLag          = gauge("soar_source_lag_seconds", "Age of the newest event a pull source has delivered", "source")
	OutboxUnpublished  = gauge("soar_outbox_unpublished", "Executions with outbox entries older than the sweeper threshold")
	TimerLag           = gauge("soar_timer_lag_seconds", "now - due_at of the oldest unfired timer")
	OrchestratorEvents = counter("soar_orchestrator_events_total", "Messages applied by the orchestrator", "type", "result")
	PolicyDecisions    = counter("soar_policy_decisions_total", "Policy decisions before dispatch", "effect", "action")
	StoreUnavailable   = gauge("soar_store_unavailable", "1 while the role holds work because OpenSearch is unavailable", "role")
	StoreHolds         = counter("soar_store_unavailable_holds_total", "Batches or messages held in place because OpenSearch was unavailable", "role")
	AuditFailures      = counter("soar_audit_failures_total", "Audit records that could not be written", "actor_type")
	SecretsScrubbed    = counter("soar_secrets_scrubbed_total", "Step outputs, errors or log lines in which the worker replaced a secret value", "action")
)
