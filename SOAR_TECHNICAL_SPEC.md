# SOAR Platform: Technical Spec (v0.9)

Full platform: backend, connectors, frontend, deployment.
Stack: **Go** (core) · **Python** (sandboxed runner, connectors) · **OpenSearch** (state/data) · **NATS JetStream** (queue) · **React + TypeScript** (UI) · **Docker**.

> **Decisions:** **single-tenant only** (later path in §3.2) · **peak 1,000 alerts/sec** through a **guaranteed, efficient NATS JetStream queue** (§3.3, §8, §9) · OpenSearch is the system of record · ingestion is **adapter + parser + mapping** driven so any SIEM, case system, or app can be added mostly by configuration (§9) · **AI-ready extension points** are designed in now, AI itself is built later (§20) · retention 7 days for data and 3 days after closure for cases, config-driven so it can become 365 days (§5.2) · MinIO for large payloads and files (§5.1) · Keycloak for OIDC · first source: Wazuh 4.14 (push and pull); first integrations: DFIR-IRIS 2.4.27 and Outlook mail (§10.1) · all model types through a model gateway, with no data restrictions (§20).

---

## 1. What it does

Alerts come in (webhook, poll, email, schedule) → matched to **workflows (playbooks)** → steps run as **actions** (built-in or connector) → results update **executions**, **cases**, and **observables** → everything is **audited**. Analysts use a web UI to build workflows, work cases, and approve steps.

## 2. Architecture

```
 Sources ──► ingest (push) / collector (pull) ──► NATS: EVENTS ──► processor (dedup, bulk-write, match triggers) ──► executions
                                                  │
 UI (React) ──► api ──────────────────────────────┤
                                                  ▼
                         orchestrator ──► NATS: STEPS ──► worker ──► builtin actions
                              ▲                                   └─► Python runner (gRPC) ──► connectors ──► external apps
                              └────────── NATS: RESULTS ◄─────────┘
 scheduler: cron, polling, timers, recovery sweeper
 OpenSearch: workflows, executions, steps, alerts, cases, observables, audit, secrets
```

One Go binary, one image, role chosen by subcommand:

| Command | Role |
|---|---|
| `soar api` | REST API, auth, CRUD, search |
| `soar ingest` | HTTP push receiver: authenticate, parse, map, publish to NATS (never waits on OpenSearch) |
| `soar collector` | Pull and stream adapters: polling APIs, syslog, email, Kafka/queues, file and object stores; owns cursors |
| `soar processor` | Consume `EVENTS`: dedup, bulk-write alerts, extract observables, match triggers, create executions |
| `soar orchestrator` | Advance executions (pure engine + I/O) |
| `soar worker` | Run actions, write step results |
| `soar scheduler` | Cron, pollers, timers, recovery (leader-elected) |
| `soar migrate` | Index templates and migrations |

Plus: `runner` (Python, gRPC, sandboxed) and `web` (React, served by nginx).

## 3. Design rules, scope, and capacity

### 3.1 Rules that prevent bugs

1. **State in OpenSearch, work in NATS.** Never use OpenSearch as a queue.
2. **Optimistic concurrency** on every state write (`if_seq_no`, `if_primary_term`). Retry on conflict (bounded).
3. **Write state first, then publish commands** (transactional outbox, §8.4). Publish with `Nats-Msg-Id = idempotency key`.
4. **At-least-once + idempotency keys = effectively once.** Key format: `{execution_id}:{node_id}:{attempt}`.
5. **Engine is a pure function:** `Next(workflow, state, event) → (commands, newState)`. No I/O, no wall clock.
6. **Data access in one place** (`internal/store`) and NATS subjects built by one helper (`queue.Subject`), so later changes stay local.
7. **Secrets decrypted only inside the worker.** Never in context, logs, traces, or API responses.
8. **`dynamic: strict`** mappings on core indices.
9. **Queue-first ingestion.** `ingest` returns `202` once NATS has persisted the event; OpenSearch writes happen downstream in bulk.
10. **Extend by registry.** Adapters, parsers, actions, and (later) LLM providers register by name behind small interfaces. Adding one never requires changing core code.

### 3.2 Single tenant (now) and the later path

The platform is single-tenant. There is no `tenant_id`, no tenant resolver, and no tenant token in subjects or IDs. If multi-tenancy is ever needed:

1. Add a `tenant_id` keyword to mappings (new index version + `_reindex` with a default value).
2. Add a tenant segment in `queue.Subject(...)` (one helper, so it is a single change) and in dedup fingerprints and idempotency keys.
3. Add a tenant filter inside `internal/store` (the only place that talks to OpenSearch).
4. Add a tenant resolver (JWT claim or API-key tenant), tenant admin APIs, per-tenant limits, and per-tenant secret keys.

This is real work, but it stays local because data access and subject naming each live in one place.

### 3.3 Capacity plan: 1,000 alerts/sec peak through NATS

**Working assumptions (measure and correct in Steps 5 and 14):**

| Item | Assumption |
|---|---|
| Peak ingest | 1,000 alerts/s (design target) |
| Average ingest | 100 alerts/s (8.6 M/day) |
| Burst headroom | 2,000 alerts/s for 60 s (2x peak), absorbed by NATS |
| Retention | 7 days for all data now; 365 days planned (§5.2) |
| Average alert size | 2 KB (normalized + raw) |
| Alerts that trigger a workflow | 10-20% (10-20 executions/s average, 100-200/s at peak) |
| Steps per execution | about 5 (50-100 step messages/s average, 500-1,000/s at peak) |

**Storage sizing (alerts index only; step runs, executions, and MinIO objects are extra):** average rate × average size × 86,400 s × retention. At 100/s and 2 KB: about 17 GB/day, so about 120 GB of primary data for 7 days (about 240 GB with one replica). Peak-all-day upper bound for 7 days: about 1.2 TB. At the planned 365 days: about 6.3 TB primary (about 12.6 TB with a replica), so plan hot/warm tiers or snapshots before raising retention. Levers: dedup, `best_compression` codec, trimming `raw` for low-severity alerts, raw bodies in MinIO.

**Design choices for this rate:**

- **Queue-first:** `ingest` publishes to JetStream with `PublishAsync` (bounded in-flight window, about 1,000) and returns `202` after the ack. A batch endpoint (up to 500 alerts per request) is provided for senders.
- **`EVENTS` stream:** file storage, `WorkQueue` retention, replicas 1 (dev) or 3 (prod), `max_bytes` cap with `discard: new`, so a full queue produces `503` back-pressure instead of data loss (full settings in §8).
- **Processor:** N replicas pull batches of 256 and ack only after the OpenSearch bulk succeeds. Bulk size 500-1,000 docs or 250 ms. Deterministic `_id` = hash(fingerprint, window bucket) so dedup is an upsert and redelivery is harmless.
- **OpenSearch:** alerts index starts with 2 primary shards, rollover at 30 GB or 1 day, refresh 5 s. Orchestrator state writes are batched per pull (bulk, per-item `if_seq_no`).
- **Orchestrator partitions:** start with `SOAR_PARTITIONS=8`.

**Targets (validated in Steps 5 and 14):**

| Metric | Target |
|---|---|
| Peak ingest | 1,000/s for 30 min, zero loss (24 h soak with hourly peaks in Step 14) |
| Burst | 2,000/s for 60 s with no 5xx; queue drains within 5 min |
| Ingest latency | p99 < 100 ms to `202` |
| Event to alert searchable | p99 < 5 s |
| Event to first step dispatched (matched alerts) | p99 < 2 s |
| Step dispatch latency | p99 < 250 ms |

## 4. Repository layout

```
soar/
  cmd/soar/                 main.go, subcommand dispatch
  internal/
    config/ domain/ blob/ store/opensearch/ queue/ api/ policy/
    ingest/{adapters,parsers,mapping}/ collector/ processor/
    engine/ expr/ orchestrator/ worker/ actions/ runner/
    scheduler/ security/ audit/ obs/
    ai/                     # reserved, empty interfaces only (see §20)
  api/openapi.yaml          REST contract (source of truth)
  api/proto/runner.proto    Go ↔ Python contract
  integrations/             one folder per product: manifest, actions, sources, mappings
  runner/python/            sandboxed runner service
  web/                      React + TS + Vite + React Flow
  deploy/compose/ deploy/opensearch/ migrations/
  test/ (integration, chaos, load)  docs/  Makefile
```

Import rule: `domain` imports nothing internal; `store/queue/engine` import `domain`; services import the rest; nothing imports `cmd`.

## 5. Data model (OpenSearch)

Common fields on all docs: `id` (prefixed ULID: `exe_`, `wf_`, `alt_`, `cas_`, `obs_`, `sr_`), `created_at`, `updated_at`, `schema_version`. Index naming: `soar-<entity>-v1` with alias `soar-<entity>`.

| Index | Type | Key fields |
|---|---|---|
| `soar-workflows` | state | name, version, status (draft/published/archived), definition, triggers[], checksum |
| `soar-executions` | state | workflow_id, workflow_version, status, trigger, context, pending_nodes[], outbox[], lease, started_at, ended_at |
| `soar-steprun` | time-series | execution_id, node_id, attempt, status, input_ref, output_ref, error, duration_ms, idempotency_key, parent_step_id, actor_type |
| `soar-alerts` | time-series | kind, source_id, external_id, fingerprint, severity(1-5), title, summary, observables[], mitre, verdict, ext, raw_ref, occurrence_count, case_id |
| `soar-cases` | state | title, summary, status, severity, assignee, sla, alert_ids[], tags[], verdict, external_refs[] |
| `soar-case-tasks` | state | case_id, title, status, assignee, due_at |
| `soar-observables` | state | type, value, first_seen, last_seen, reputation, case_ids[] |
| `soar-timeline` | time-series | subject_id, event_type, actor, data |
| `soar-audit` | time-series | actor, actor_type, action, resource, before_hash, after_hash, ip, request_id |
| `soar-secrets` | state | name, ciphertext, key_id, nonce, scope |
| `soar-principals` | state | type, name, roles[], key_hash, disabled |
| `soar-idempotency` | state | key, status, result_ref, expires_at |
| `soar-timers` | state | execution_id, node_id, due_at, kind, fired |
| `soar-sources` | state | name, adapter, parser, mapping_id, auth, schedule, filters, dedup, rate_limit, enabled, health |
| `soar-mappings` | state | name, version, kind, definition (CEL field map), checksum |
| `soar-filters` | state | name, version, mode, default, rules[] (CEL or script), checksum |
| `soar-lists` | state | name, items[] (managed lists used by filters, for example noisy rule IDs) |
| `soar-cursors` | state | source_id, cursor, updated_at (polling and stream positions) |
| `soar-raw` | time-series | source_id, received_at, body_ref (MinIO), parse_error (7-day retention, enables replay) |
| `soar-case-alerts` | state | case_id, alert snapshot (copied when an alert is linked to a case, so it outlives the alert's own retention) |
| `soar-seen` | state | key = hash(system, external_id), first_seen (TTL 24 h; exact-duplicate guard for push + pull overlap) |
| `soar-connections` | state | connector_id, name, auth_type, secret_refs, config |

Payloads over 64 KB are stored in MinIO and referenced (`*_ref`). Execution documents stay small.

### 5.1 Payload and file storage (MinIO, S3 API)

| Bucket | Content | Lifecycle |
|---|---|---|
| `soar-raw` | Raw inbound bodies over 64 KB (smaller ones travel inline in the NATS message) | Expire after 7 days |
| `soar-payloads` | Step inputs and outputs over 64 KB | Expire after 7 days |
| `soar-evidence` | Case files and email attachments | Deleted with the case (7 days after closure) by the scheduler; versioning on |
| `soar-exports` | Generated reports and exports | Expire after 7 days |

- Reference format: `blob://<bucket>/<yyyy>/<mm>/<dd>/<ulid>` plus SHA-256 and size stored beside the reference.
- **Write order:** upload the object first, then write the document or publish the message that references it. An orphaned object is harmless and is removed by lifecycle expiry or an orphan sweeper; a dangling reference is never created.
- Access only through `internal/blob` (S3 API, so MinIO can be swapped for another S3-compatible store). Downloads for the UI use short-lived presigned URLs. Server-side encryption on; credentials come from config or a secret.

```go
type Blob interface {
    Put(ctx context.Context, bucket, key string, r io.Reader, size int64, meta map[string]string) (Ref, error)
    Get(ctx context.Context, ref Ref) (io.ReadCloser, error)
    Delete(ctx context.Context, ref Ref) error
    PresignGet(ctx context.Context, ref Ref, ttl time.Duration) (string, error)
}
```

- MinIO is licensed AGPL; the interface keeps this a deployment choice.

### 5.2 Retention (7 days now, 365 days later)

Retention is **configuration, not code**. `soar migrate` and the scheduler read one file and generate OpenSearch ISM policies, MinIO lifecycle rules, and cleanup jobs from it. Moving from 7 to 365 days is a config change plus a capacity check.

```yaml
# config/retention.yaml
default: 7d
classes:
  alerts: 7d          # from received_at
  raw: 7d             # MinIO lifecycle + soar-raw index
  steprun: 7d
  timeline: 7d
  executions: 7d      # from ended_at; running/waiting executions are never deleted
  cases: 3d           # from closed_at; open cases are never deleted
  case_evidence: 3d   # follows its case (tasks, alert snapshots too)
  observables: 7d     # from last_seen; kept while linked to a live case
  audit: 7d           # planned: 365d
  idempotency: ttl    # by expires_at
```

| Data | Mechanism |
|---|---|
| Alerts, raw metadata, step runs, timeline, audit | Rollover indices + generated ISM delete policy |
| Executions, cases, case tasks, case alert snapshots, observables | Scheduler cleanup job: throttled `delete_by_query` on ended or closed items; open or running items are never deleted |
| Raw bodies and large payloads (MinIO) | MinIO lifecycle rules |
| Case evidence (MinIO) | Scheduler deletes objects when the case expires |
| Idempotency and `soar-seen` records | TTL cleanup |
| **Configuration objects** (workflows, sources, mappings, connections, secrets, principals, models) | Not subject to retention |

**Rules:**

- Linking an alert to a case copies it into `soar-case-alerts`, so a case does not lose its alerts when the alert itself expires.
- For cases and executions the clock starts when they end or close, so work in progress is never deleted. Cases (with their tasks, alert snapshots, and evidence) expire 3 days after closure; executions expire 7 days after they end.
- Expiry of a local case does not delete the matching case in DFIR-IRIS (§10.1); IRIS keeps its own copy under its own retention.
- Audit is set to 7 days for now by decision; audit logs normally need longer retention for accountability, which is why `audit: 365d` is the planned value.

## 6. Workflow DSL (YAML/JSON, expressions in CEL)

```yaml
name: phishing_triage
version: 1
triggers:
  - type: alert
    match: 'alert.source == "email_gateway" && alert.severity >= 3'
inputs:
  - name: url
    from: 'alert.observables.filter(o, o.type == "url")[0].value'
nodes:
  - id: check_url
    action: http.request
    params: { method: GET, url: '"https://intel.example/u?q=" + inputs.url' }
    retry: { max_attempts: 3, backoff: exponential, initial: 2s, max: 30s }
    timeout: 20s
  - id: decide
    type: branch
    cases:
      - when: 'steps.check_url.output.body.score > 70'
        next: [block]
    default: [close]
  - id: block
    action: connector.firewall.block_url
    idempotency_key: '"block:" + inputs.url'
    params: { url: 'inputs.url' }
  - id: close
    action: case.close
edges:
  - check_url -> decide
```

**Node types:** `action`, `branch`, `parallel`, `foreach` (concurrency cap), `wait`, `approval`, `set`, `subworkflow`, `end`, and a reserved `agent` (rejected by the validator until AI is enabled, §20).

**CEL variables:** `alert`, `inputs`, `vars`, `steps.<id>.output|status`, `secrets.<name>` (worker only), `env`.

**Validation at save time:** unique node IDs; all targets exist; acyclic (loops only via `foreach`); all nodes reachable; CEL compiles and type-checks; action params match JSON schema; limits respected. Published versions are immutable; executions pin their version.

## 7. Execution engine

State machine (any other transition is a bug):

| From | Event | To |
|---|---|---|
| — | TriggerMatched | queued |
| queued | Claimed | running |
| running | StepSucceeded (more nodes) | running |
| running | StepSucceeded (none left) | succeeded |
| running | StepFailed (retries exhausted, no error path) | failed |
| running | WaitStarted | waiting |
| waiting | TimerFired / ApprovalGranted / EventReceived | running |
| running/waiting | CancelRequested | cancelled |
| running/waiting | DeadlineExceeded | timed_out |

**Orchestrator loop:** pull message → load execution (with seq/term) → check/acquire lease → `Engine.Next` → write state **and outbox entries** in one concurrency-checked write → publish outbox commands → mark published → ack (§8.4).
**Partitioning:** `hash(execution_id) % N` → `results.<partition>`.
**Defaults:** step timeout 60s (max 1h); 3 attempts (max 10); exponential backoff with jitter; execution deadline 24h. Retry only network, timeout, 5xx, 429.

## 8. Queue management (NATS JetStream): guaranteed and efficient

### 8.1 What is guaranteed

| Property | Mechanism |
|---|---|
| No accepted event is lost | `202` is returned only after JetStream acknowledges a durable write (file storage, R3 quorum in production) |
| No step is lost | Transactional outbox: state and pending commands are saved in one write; a sweeper republishes anything unconfirmed (§8.4) |
| No duplicate side effects | At-least-once delivery + publish dedup (`Nats-Msg-Id`) + idempotency store + deterministic IDs |
| Failures are visible | Bounded redelivery, then DLQ with reason; replay via API |
| Overload is safe | Bounded streams (`discard: new`) return `503` to senders; consumers capped by `MaxAckPending` |
| Ordering | Not global. Per-execution ordering via partitioned subjects; the engine tolerates reordering and duplicates |

**Not guaranteed:** exactly-once delivery (not achievable in general; effectively-once comes from idempotency). If JetStream storage is destroyed on every replica, events accepted but not yet processed are lost; R3 replication and backups reduce that risk.

### 8.2 Streams

| Stream | Subjects | Retention | Limits | Purpose |
|---|---|---|---|---|
| `EVENTS` | `events.<source>` | WorkQueue | max_age 7 d, max_bytes 20 GB, discard new, dedup window 5 min | Ingested alerts |
| `STEPS` | `steps.<priority>.<action_class>` | WorkQueue | max_age 24 h, discard new | Steps ready to run; priority is `critical`, `normal`, or `bulk` |
| `RESULTS` | `results.<partition>` | WorkQueue | max_age 24 h, discard new | Step outcomes and timer events for the orchestrator |
| `DLQ` | `dlq.<origin>` | Limits | max_age 30 d | Poison and exhausted messages |

All streams: file storage, replicas 1 in dev and 3 in production, S2 compression, messages kept small (payloads over 64 KB go to storage and travel by reference).

**Durability note:** JetStream fsyncs periodically by default, so a single-replica node can lose recent acked messages on power loss. Production uses R3 (quorum commit). Single-node setups set `sync_interval: always`. Measure the throughput cost in the Step 5 load test.

```go
js, _ := jetstream.New(nc)
js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
    Name:        "EVENTS",
    Subjects:    []string{"events.>"},
    Retention:   jetstream.WorkQueuePolicy,
    Storage:     jetstream.FileStorage,
    Replicas:    3,
    MaxAge:      7 * 24 * time.Hour,
    MaxBytes:    20 << 30,
    Discard:     jetstream.DiscardNew,   // full queue => publish error => HTTP 503
    Duplicates:  5 * time.Minute,        // Nats-Msg-Id dedup window
    Compression: jetstream.S2Compression,
})
```

**Message envelope:** `{id, type, execution_id, node_id, attempt, idempotency_key, trace.traceparent, sent_at, payload | payload_ref}`.

### 8.3 Consumers

Durable pull consumers with explicit acks. Filter subjects must not overlap within a WorkQueue stream.

| Consumer | Stream / filter | Fetch batch | AckWait | MaxDeliver | BackOff |
|---|---|---|---|---|---|
| `processor` | `EVENTS` / `events.>` | 256 | 60 s | 5 | 1 s, 5 s, 30 s, 2 min |
| `orch-p{N}` (one per partition) | `RESULTS` / `results.{N}` | 64 | 30 s | 10 | 1 s, 5 s, 30 s |
| `worker-{priority}-{class}` | `STEPS` / `steps.{priority}.{class}` | worker concurrency | step timeout + 15 s | 3 | 5 s, 30 s |

```go
cons, _ := js.CreateOrUpdateConsumer(ctx, "EVENTS", jetstream.ConsumerConfig{
    Durable:       "processor",
    AckPolicy:     jetstream.AckExplicitPolicy,
    AckWait:       60 * time.Second,
    MaxDeliver:    5,
    BackOff:       []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute},
    MaxAckPending: 2048,
    FilterSubject: "events.>",
})
batch, _ := cons.Fetch(256, jetstream.FetchMaxWait(500*time.Millisecond))
```

**Ack rules:**

- Ack only after the durable effect is complete (bulk write succeeded, result published). For results and state-changing acks use `DoubleAck`/`AckSync` so a lost ack cannot silently cause reprocessing without notice.
- Long actions send `InProgress()` every `AckWait/3` to extend the deadline.
- Transient failure (OpenSearch 429/5xx): `NakWithDelay`.
- Permanent invalid message: `Term()` and route to DLQ.

### 8.4 Transactional outbox (state and queue stay consistent)

Writing OpenSearch and publishing to NATS cannot be one transaction, so the orchestrator uses an outbox stored inside the execution document.

1. Orchestrator computes new state and commands (`Engine.Next`).
2. One write (with `if_seq_no`/`if_primary_term`) saves the new state **and** `outbox[]` entries: `{key, subject, payload_ref, published: false}`.
3. Publish each entry with `Nats-Msg-Id = key`; on JetStream ack, mark it `published: true` (batched, asynchronous).
4. Ack the inbound message.
5. **Sweeper:** every few seconds, finds executions with `published:false` entries older than 10 s and republishes them. Duplicates are dropped by `Nats-Msg-Id` (inside the dedup window) or by the idempotency store at the worker.

A crash at any point leaves either nothing to do or an entry the sweeper completes, so no step is lost.

### 8.5 Retries, poison messages, and the DLQ

Two separate retry layers, never mixed:

| Layer | Trigger | Owner | Mechanism |
|---|---|---|---|
| Business retry | Action returned a retryable error (timeout, 5xx, 429) | Engine | New attempt (`attempt+1`, new idempotency key) scheduled via a timer with backoff |
| Delivery retry | Worker crash or lost ack | JetStream | Redelivery after `AckWait`, `BackOff`, up to `MaxDeliver` |

**DLQ flow:** a mover subscribes to the JetStream max-deliveries advisory for each consumer, copies the original message (by stream sequence) to `dlq.<origin>` with reason, delivery count and trace ID, then removes the original so it does not block the work queue. Verify this exact behavior in a test, since WorkQueue streams keep unacked messages. A metric and alert fire on any DLQ arrival. `POST /dlq/{id}/replay` republishes to the original subject.

### 8.6 Timers and waits

Timers are durable in OpenSearch, not in NATS: a `soar-timers` document with `due_at`. The scheduler (leader-elected) polls due timers every second in batches, publishes a `TimerFired` event to `results.<partition>` with `Nats-Msg-Id = timer id`, then marks the timer `fired`. If the scheduler crashes between publish and mark, the republish is deduplicated. Approvals and wait-for-event nodes use the same mechanism for their timeouts.

### 8.7 Back-pressure and flow control

- Stream full: `discard: new` makes publish fail; `ingest` returns `503` with `Retry-After`. Nothing is dropped silently.
- Consumers: `MaxAckPending` caps in-flight messages; workers use a semaphore sized to CPU and memory limits.
- OpenSearch slow: processor pauses fetching (`NakWithDelay` on rejected bulk items) and queue depth absorbs the burst.
- Priority: `critical` steps use their own subjects and dedicated worker capacity so bulk enrichment cannot starve urgent response actions.
- Rate limits at ingest protect the queue from a single noisy source.

### 8.8 Efficiency

- Batch everywhere: `Fetch(256)` on pull, `PublishAsync` with a bounded in-flight window (about 1,000) on publish, bulk writes (500-1,000 docs or 250 ms) to OpenSearch.
- One consumer per orchestrator partition, so one execution has one writer in normal operation and conflict retries stay rare.
- Subjects are specific enough to avoid wildcard fan-out; no overlapping filters.
- Small messages and S2 compression; large payloads by reference.
- Long-lived connections, reused clients, no per-message sync ack except where the rules above require it.
- Starting sizes for 1,000/s (tune from load tests): 2-4 processor replicas, 8 orchestrator partitions, workers scaled per action class.

### 8.9 Monitoring and alerts

| Signal | Alert when |
|---|---|
| Stream bytes / `max_bytes` | above 70% |
| Consumer pending messages (lag) | rising for 5 min or above 50,000 on `EVENTS` |
| Oldest unacked message age | above 2 x `AckWait` |
| Redelivery rate | above 1% of deliveries |
| DLQ depth / arrivals | any arrival |
| Publish error rate at ingest | above 0.1% |
| Outbox entries unpublished > 10 s | any, for more than 1 min |
| Timer lag (`now - due_at` of oldest unfired) | above 5 s |

### 8.10 Queue tests (exit criteria for Steps 5 and 8)

1. **Zero loss:** send N unique `event_id`s at 1,000/s; assert each appears exactly once in alerts (duplicates become `occurrence_count`, not extra documents).
2. **Kill processor mid-batch:** no loss, no double-created executions.
3. **Kill a worker mid-step:** step redelivered; side effect happens once (idempotency).
4. **Kill orchestrator between state write and publish:** sweeper completes the outbox; no lost step.
5. **NATS node failure (R3):** publishing resumes; no acked event is lost.
6. **OpenSearch outage:** ingest keeps accepting until the stream limit, then `503`; after recovery the backlog drains with no loss.
7. **Poison message:** reaches DLQ after `MaxDeliver`, does not block the queue; replay works.
8. **Burst 2,000/s for 60 s:** no 5xx; backlog drains within 5 min.

## 9. Ingestion framework (flexible, pluggable, queue-first)

**Goal:** connect any SIEM, case-management tool, or application by webhook, API, or other transport. Adding a source should be configuration (a source + a mapping), not code. Code is needed only for a brand-new transport.

### 9.1 Pipeline

`Transport adapter → raw envelope → parser → mapper → normalized event → NATS EVENTS → processor → OpenSearch + executions`

Three concerns, kept separate:

| Concern | Question | Pluggable by |
|---|---|---|
| Transport (adapter) | How does data arrive? | Go adapter registered by name |
| Parser | What format is it? | Go parser registered by name |
| Mapping | What does it mean? | Declarative YAML using CEL; no code |

The **raw envelope** is `{source_id, kind, received_at, transport, meta (headers, topic, ...), body}`. Raw bodies are kept for 7 days (metadata in `soar-raw`, large bodies in MinIO) so a source can be replayed after a mapping is fixed.

### 9.2 Event kinds

Ingestion is not limited to alerts. Every event has a `kind`, and triggers can subscribe by kind.

| Kind | Examples |
|---|---|
| `alert` | SIEM or EDR detections |
| `case_event` | Ticket created, updated, or closed in an external case system |
| `indicator` | Threat-intel IOCs |
| `asset` | CMDB or EDR inventory changes |
| `custom` | Anything else, mapped to a generic schema with `ext` fields |

### 9.3 Transport adapters

| Adapter | Direction | Role | Phase |
|---|---|---|---|
| `webhook` | push | `ingest` | v1 |
| `poll_http` (REST API with cursor, paging, OAuth2/key auth) | pull | `collector` | v1 |
| `syslog` (UDP/TCP/TLS; RFC 5424/3164) | push | `collector` | v1.1 |
| `email` (IMAP, Microsoft Graph, Gmail API) | pull | `collector` | v1.1 |
| `kafka` / AMQP / SQS / Pub/Sub / Event Hubs | stream | `collector` | v1.1 |
| `file_object` (S3, GCS, Azure Blob, SFTP) | pull | `collector` | later |
| `sql_poll` (query with cursor column) | pull | `collector` | later |
| `sse` / `websocket` | stream | `collector` | later |
| `manual` (UI/API upload, test events) | push | `api` | v1 |

Log shippers (Fluent Bit, Vector, Beats, Splunk HTTP forwarders) need no special adapter: they use `webhook` or `syslog`.

```go
// Push adapters receive data and hand it to the sink.
type Adapter interface {
    Kind() string
    Start(ctx context.Context, src SourceConfig, sink RawSink) error
    Stop(ctx context.Context) error
}
// Pull adapters are driven by the collector with a persisted cursor.
type Poller interface {
    Poll(ctx context.Context, src SourceConfig, cur Cursor) (items []RawItem, next Cursor, err error)
}
// RawSink applies back-pressure: it returns an error when the queue is full.
type RawSink interface { Accept(ctx context.Context, ev RawEvent) error }

adapters.Register("webhook", newWebhook)   // registry pattern
```

### 9.4 Parsers

`json`, `ndjson`, `xml`, `csv`, `cef`, `leef`, `syslog`, `keyvalue`, and `regex` (named groups). Interface: `Parse(raw []byte, meta Meta) ([]map[string]any, error)`. One raw body can yield many records (batch payloads, NDJSON).

### 9.5 Source configuration

```yaml
# Push: SIEM sends webhooks
id: siem-prod
system: siem-prod        # sources sharing a system share external IDs (push + pull dedup)
adapter: webhook
parser: json
mapping: siem_alert@1
auth: { type: hmac, header: X-Signature, algo: sha256, secret_ref: siem_prod_hmac }
filters: [siem-default@1]   # filter sets, see §9.5.1
dedup: { fields: [raw.rule_name, raw.host], window: 10m }
rate_limit: { rps: 200, burst: 500 }
```

```yaml
# Pull: poll a REST API
id: case-system-events
system: case-system
adapter: poll_http
schedule: every 30s
request:
  method: GET
  url: 'https://cases.example/api/v2/events'
  query: { since: 'cursor.value', limit: '200' }
  auth: { type: oauth2_client_credentials, connection: case_system_prod }
items_path: 'body.items'
pagination: { type: cursor, next: 'body.next_cursor' }
cursor: { field: 'item.updated_at', initial: 'now-1h' }
parser: json
mapping: case_event@1
```

**Inbound auth options:** shared token, HMAC (configurable header and algorithm), Basic, bearer JWT validation, mTLS, IP allowlist, timestamp-based replay protection. **Outbound auth for pollers:** API key, Basic, Bearer, OAuth2 (client credentials and refresh), via `soar-connections` so secrets are never in source configs.

### 9.5.1 Filtering (flexible and reusable)

Filter sets are versioned objects (`soar-filters`) that sources reference as `filters: [name@version, ...]`. They run in two stages: `pre_map` (on `raw`, cheapest, drops early) and `post_map` (on the normalized event).

- **Rule shape:** `{name, stage, action, when | script}`. Actions: `keep`, `drop`, `tag`, `set_severity`, `route` (send to a specific workflow). Set `mode: first_match` or `all`, and a `default: keep | drop`.
- **What a rule can match:** any field, through CEL: equality, `in` lists, regex (`matches`), numeric ranges, list membership, time of day, and managed lists (`in_list(value, "list_name")`, stored in `soar-lists` so IDs and names can be edited without a deploy).
- **Script filters:** for logic CEL cannot express, a Python script runs in the sandboxed runner. Scripts receive **batches** (up to 256 events per call) and return a decision per event, so they stay affordable at peak rate. They run only on events that passed the cheap CEL rules. A script that errors or times out **keeps** the event by default (fail-open) and increments an error metric; this is configurable.
- **Push-down for pull sources:** a `poll_http` source can include an OpenSearch query-DSL fragment (`query_filter`) so filtering happens on the server, followed by the same CEL stage.
- **Push sources filter inside SOAR:** the sending system can stay broadly configured, and filter changes take effect without touching it.
- **Observability and tuning:** `soar_filter_events_total{source,rule,action}`; optional sampling of dropped raw events (default off) for tuning.
- **Testing:** `POST /filters/test` runs a sample payload and returns a rule-by-rule trace. `POST /filters/simulate` replays the last N hours of stored raw events (within the 7-day window) and returns counts per rule, with no side effects.

```yaml
id: wazuh-default
version: 1
mode: first_match
default: keep
rules:
  - { name: drop-noisy-rule-ids, stage: pre_map, action: drop, when: 'in_list(raw.rule.id, "wazuh_noisy_rule_ids")' }
  - { name: drop-ignored-decoders, stage: pre_map, action: drop, when: 'in_list(get_path(raw, "decoder.name"), "ignored_decoders")' }
  - { name: ransomware-or-bruteforce, stage: pre_map, action: tag, when: 'raw.rule.description.matches("(?i)ransom|brute.?force")' }
  - { name: extra-logic, stage: pre_map, action: drop, script: filters/wazuh_extra.py }
  - { name: min-level, stage: pre_map, action: drop, when: 'int(raw.rule.level) < 3' }
```

### 9.6 Declarative mapping (CEL)

```yaml
id: siem_alert           # field names below are illustrative; validate against real payloads
version: 1
kind: alert
fields:
  external_id: 'raw.id'
  title:       'raw.rule_name'
  description: 'coalesce(raw.description, "")'
  severity:    'severity_map(raw.severity, {"critical":5,"high":4,"medium":3,"low":2,"informational":1})'
  occurred_at: 'parse_time(raw.timestamp)'
  tags:        '["siem"]'
observables:
  - { type: ip,   value: 'raw.src_ip', role: source }
  - { type: host, value: 'raw.host' }
  - { type: hash, from:  'extract_hashes(raw.message)' }
mitre: 'coalesce(raw.mitre, {})'
dedup_key: ['raw.rule_name', 'raw.host']
ext: 'raw'              # keep vendor-specific fields under ext
```

- Helper functions available in CEL: `coalesce`, `severity_map`, `parse_time`, `lower`, `regex_extract`, `extract_ips|domains|urls|hashes|emails`, `get_path`.
- Mappings are versioned and immutable once published; sources reference `name@version`.
- Escape hatch for odd formats: a `transform` step run in the sandboxed Python runner (slower, used sparingly).
- **Source packs:** ready-made source + mapping YAML per product under `integrations/<name>/sources|mappings/` (Splunk, Elastic, Sentinel, QRadar, Wazuh, CrowdStrike, Jira, ServiceNow, TheHive, and so on). Each pack must be verified against real sample payloads before it is marked supported.
- **Test before enabling:** `POST /mappings/test` and `POST /sources/{id}/test` take a sample payload and return the normalized event or the exact failing expression.

### 9.7 Normalized alert schema (internal)

| Field | Notes |
|---|---|
| `id`, `event_id`, `kind`, `source_id`, `external_id`, `external_url` | Identity and link back to the source system |
| `title`, `description`, `summary` | `summary` is reserved for human or AI-written summaries |
| `severity` (1-5), `original_severity` | Normalized plus the raw vendor value |
| `status`, `category`, `tags[]` | |
| `observables[]` | `{type, value, role}` |
| `entities` | Hosts, users, accounts |
| `mitre` | `{technique_ids[], techniques[], tactics[]}` (Wazuh supplies tactics as one list for all techniques, so they are not paired) |
| `occurred_at`, `received_at`, `last_seen` | |
| `fingerprint`, `occurrence_count` | Dedup |
| `verdict` | Analyst or automation disposition (true positive, false positive, benign, unknown); useful for metrics and, later, AI evaluation |
| `ext` | Flattened vendor-specific fields |
| `raw_ref`, `schema_version` | |

The schema is kept close to OCSF/ECS naming so exports and later mappings are straightforward.

### 9.8 Two-way integration (case management and SIEM sync)

- **Outbound:** integration actions create, update, comment on, and close external tickets or alerts (idempotent, with `supports_dry_run`).
- **Inbound:** `case_event` sources map external status changes onto local cases and alerts.
- Cases and alerts store `external_refs[]`: `{system, id, url, last_synced_at, sync_version}`.
- **Loop prevention:** every outbound change carries an `origin` marker; inbound events that match a recent outbound change (same `sync_version`) are ignored.

### 9.9 Ingest and collector behavior

**`ingest` (HTTP push):**

1. Authenticate (per-source scheme), body ≤ 1 MB, batch ≤ 500 records, global and per-source rate limits.
2. Parse and map. Records that fail parse or mapping are **not dropped**: raw is stored in `soar-raw` with `parse_error`, a copy goes to `dlq.ingest`, and a per-source error metric increments. Fix the mapping, then replay with `POST /sources/{id}/replay`.
3. Apply the source's filter sets (§9.5.1). Drops are counted per rule, never silent.
4. Assign `event_id` (ULID), `received_at`, fingerprint.
5. `PublishAsync` to `events.<source>` with `Nats-Msg-Id = event_id` (or the client's `Idempotency-Key`); wait for the JetStream ack.
6. Return `202 {event_ids}`. Stream full or unhealthy: `503`/`429` with `Retry-After`.

**`collector` (pull and stream):** runs adapters on a schedule or continuously; stores the cursor in `soar-cursors` only **after** JetStream acks the batch (at-least-once; dedup removes repeats); applies the same parse, map, and publish steps; backs off on source errors; one lease per source so only one collector instance polls it.

**`processor` (scalable consumer group):**

1. Pull a batch (256) from `EVENTS`.
2. **Exact-duplicate guard:** for events with an `external_id`, bulk-create `soar-seen` keys (`hash(system, external_id)`, `op_type=create`). A conflict means the same event already arrived (for example by push and by pull); it is dropped and counted as `duplicate`.
3. Extract observables (IP, domain, URL, hash, email).
4. Group and persist: bulk `update` with upsert on the deterministic `_id` (fingerprint + 10-minute window bucket); a script increments `occurrence_count` and `last_seen`.
5. Bulk upsert observables.
6. For new (non-grouped) events, match triggers (by `kind` and CEL condition) against an in-memory compiled index refreshed on workflow publish, and bulk-create executions with deterministic IDs (hash of event and workflow).
7. Ack the batch only after bulk success. Failed items retry individually; poison items go to `DLQ`.

### 9.10 Operations

- Sources and mappings are cached and hot-reloaded on change (core NATS notification `config.changed`).
- Per-source health: last event time, rate, error rate, lag. Alert when a source goes silent beyond its expected interval.
- Per-source metrics: `soar_source_events_total{source,result}` (`accepted|filtered|parse_error|rejected`), `soar_source_lag_seconds`.

## 10. Actions and integrations

**Action manifest:**

```yaml
id: http.request
version: 1
description: Send an HTTP request; returns status, headers, body   # human and LLM readable
side_effect: read         # read | write | destructive
risk: low                 # low | medium | high
supports_dry_run: true
class: builtin            # builtin | python
params_schema: {...}      # JSON Schema
output_schema: {...}
secrets: [auth_token]
retryable_errors: [network, timeout, http_5xx, http_429]
at_most_once: false
limits: { cpu: 0.5, memory_mb: 128, timeout_s: 60, egress: ["*"] }
```

```go
type Action interface {
    Manifest() Manifest
    Execute(ctx context.Context, in Input) (Output, error)
}
type Error struct{ Code string; Retryable bool; Message string }
```

**Built-in (Go):** `http.request`, `transform.map`, `delay.sleep`, `case.create|update|close`, `observable.add`, `notify.log`.

**Integration package:** `integrations/<name>/manifest.yaml` (name, auth type, actions, schemas, optional `sources:` and `mappings:`) + `actions/*.py`. One package can provide actions, inbound sources, and mappings for the same product (for example a SIEM). The `description`, `side_effect`, `risk`, and `supports_dry_run` fields are required for every action; they power approvals and, later, AI tool use (§20). The platform handles auth (API key, Basic, Bearer, OAuth2 with refresh), pagination, rate limiting, retries, and timeouts; connector code only maps params to the remote API and returns normalized output. Later: OpenAPI import generates actions automatically.

**Python runner (gRPC):**

```proto
service Runner { rpc Execute(ExecuteRequest) returns (stream ExecuteEvent); }
message ExecuteRequest { string action_id=1; string idempotency_key=2; bytes params_json=3;
  map<string,string> secrets=4; Limits limits=5; string traceparent=6; }
message Limits { double cpu=1; uint32 memory_mb=2; uint32 timeout_s=3; }
message ExecuteEvent { oneof event { string log=1; Result result=2; Failure failure=3; } }
message Result  { bytes output_json=1; }
message Failure { string code=1; bool retryable=2; string message=3; }
```

Runner container: non-root, read-only FS, no capabilities, seccomp, CPU/memory limits, egress allowlist. Secrets passed per call, memory only.

**Worker algorithm:** pull step → check idempotency store (done? republish result, ack) → mark `in_progress` → resolve secrets and CEL params → execute with limits → write step run + idempotency result → publish result → ack.

### 10.1 First sources and integrations (v1 scope)

Items marked *verify* must be confirmed against the target product version and real sample payloads before they are marked supported.

#### Wazuh 4.14 (SIEM source: push and pull, built in Step 5)

- **Push (v1):** Wazuh's Integrator supports custom integrations whose name starts with `custom-`, with `hook_url`, `level`, `rule_id`, `group` filters and `alert_format` json (confirmed in Wazuh docs). A small script `custom-soar` in `/var/ossec/integrations/` posts the alert JSON to `POST /ingest/wazuh-prod` with a source token.

```xml
<integration>
  <name>custom-soar</name>
  <hook_url>https://soar.example/ingest/wazuh-prod</hook_url>
  <level>3</level>   <!-- coarse; fine filtering happens in SOAR (§9.5.1) -->
  <alert_format>json</alert_format>
  <api_key>SOURCE_TOKEN</api_key>
</integration>
```

- **Pull (v1):** `poll_http` against the Wazuh indexer (single node, one endpoint, TLS CA on the connection). Confirmed from your indexer sample:
  - Documents live in daily indices named by the **UTC** date of `@timestamp` (for example `wazuh-alerts-4.x-2026.03.02` holds an alert with `@timestamp` 2026-03-02T19:15:44Z, which is 2026-03-03 01:15 at +0600).
  - Each document has the same fields as the manager alert plus `@timestamp` (UTC, ends in `Z`) and `input.type`. `timestamp` and `id` are identical to the manager's `alerts.json`, so push and pull produce the same `external_id` and deduplicate cleanly.
  - Your cluster reports 141 shards, so the poller must not search every index each time. The adapter builds the index list from the last N UTC days (`index_window`, default 2) instead of using `wazuh-alerts-*`.
  - Cursor: `@timestamp` with a 60-second overlap (duplicates are dropped by the exact-duplicate guard, §9.9). Sort is `@timestamp` then `id`; at connection test the collector reads the field mapping and, if `id` is not sortable, falls back to overlap plus the guard only.
  - Use a **dedicated read-only indexer user** (not `admin`) and verify TLS with the CA certificate; disabling certificate checks (`curl -k`) is allowed only as an explicit dev setting that logs a warning.

```yaml
id: wazuh-pull
system: wazuh-prod
adapter: poll_http
parser: json
schedule: every 15s
connection: wazuh_indexer          # basic auth user + CA certificate
request:
  method: POST
  path: '/{index}/_search'          # {index} built from index_window
  index_window: { prefix: 'wazuh-alerts-4.x-', days: 2 }
  body:
    size: 500
    sort: [{ '@timestamp': asc }, { id: asc }]
    query: { bool: { filter: [ { range: { '@timestamp': { gt: 'cursor.value' } } } ] } }
items_path: 'body.hits.hits'
item_path: '_source'
cursor: { field: 'item.@timestamp', overlap: 60s, initial: 'now-1h' }
mapping: wazuh_alert@1
filters: [wazuh-default@1]
```

If a poll returns a full page (500), the collector polls again immediately until caught up, capped per cycle so one source cannot starve others. At the 100/s average a 15-second poll returns about 1,500 alerts (three pages).

- **Filtering by anything:** rule level, rule ID, rule name or description, rule groups, decoder name, agent, location, `data.*` fields, full log text, or a script, all through filter sets (§9.5.1). Keep the Wazuh Integrator filter coarse: changing integration settings in Wazuh requires a manager restart (confirmed in Wazuh docs), while SOAR filter changes apply immediately. Pull sources can also push the filter down to the indexer as `query_filter`. In your samples, level alone would hide the successful root password login (rule 5715, level 3, from 10.10.192.1) and most samples are level 3 or 5 (one is 7), which is why filters match on rule, decoder, and data fields and the default minimum level is 3, not higher.
- **Push and pull together, no duplicates:** both sources declare the same `system: wazuh-prod`. The Wazuh alert `id` is the `external_id`, so the processor's exact-duplicate guard (§9.9) drops the second copy of any alert that arrives both ways.
- **Severity map from `rule.level`:** 0-3 → 1, 4-6 → 2, 7-9 → 3, 10-12 → 4, 13-16 → 5 (Wazuh documents levels 0-16 for filters). Adjustable.
- **Dedup key:** `rule.id` + `agent.id` + (`data.srcip` or `syscheck.path`) + `data.dstuser`, 10-minute window.

```yaml
id: wazuh_alert
version: 1
kind: alert
fields:
  external_id:       'raw.id'                                  # e.g. "1793256511.10907"
  title:             'raw.rule.description'
  description:       'coalesce(get_path(raw, "full_log"), "")'
  severity:          'level_map(raw.rule.level, [[0,3,1],[4,6,2],[7,9,3],[10,12,4],[13,16,5]])'
  original_severity: 'string(raw.rule.level)'
  occurred_at:       'parse_time(raw.timestamp, "2006-01-02T15:04:05.000-0700")'   # offset has no colon
  tags:              'coalesce(get_path(raw, "rule.groups"), [])'
observables:           # null or empty values are skipped
  - { type: host,      value: 'raw.agent.name' }
  - { type: ip,        value: 'get_path(raw, "data.srcip")',   role: source }
  - { type: user,      value: 'get_path(raw, "data.srcuser")', role: source }
  - { type: user,      value: 'get_path(raw, "data.dstuser")', role: target }
  - { type: file_path, value: 'get_path(raw, "syscheck.path")' }
  - { type: hash,      value: 'get_path(raw, "syscheck.sha256_after")', skip_if: 'is_empty_hash(value)' }
  - { type: hash,      value: 'get_path(raw, "syscheck.sha1_after")',   skip_if: 'is_empty_hash(value)' }
  - { type: hash,      value: 'get_path(raw, "syscheck.md5_after")',    skip_if: 'is_empty_hash(value)' }
mitre:
  technique_ids: 'coalesce(get_path(raw, "rule.mitre.id"), [])'
  techniques:    'coalesce(get_path(raw, "rule.mitre.technique"), [])'
  tactics:       'coalesce(get_path(raw, "rule.mitre.tactic"), [])'
dedup_key: ['raw.rule.id', 'raw.agent.id', 'coalesce(get_path(raw, "data.srcip"), get_path(raw, "syscheck.path"), "")', 'coalesce(get_path(raw, "data.dstuser"), "")']
ext: 'raw'
```

**Confirmed against your Wazuh 4.14 samples** (five manager alerts and one indexer hit; levels seen: 3, 5, and 7):

| Sample | `rule.id` | Level → severity | Observables produced |
|---|---|---|---|
| FIM file added (`/usr/bin/make-dummy-cert`) | 554 | 5 → 2 | host, file path, SHA-256, SHA-1, MD5 |
| FIM file added (`/usr/bin/npx-22`) | 554 | 5 → 2 | host, file path (hashes skipped, see below) |
| systemd service failure | 40704 | 5 → 2 | host |
| PAM login session opened | 5501 | 3 → 1 | host, users; MITRE T1078 |
| sshd authentication success | 5715 | 3 → 1 | host, source IP 10.10.192.1, user; MITRE T1078, T1021 |
| FIM file modified (`/etc/ld.so.cache`), indexer hit | 550 | 7 → 3 | host, file path, SHA-256, SHA-1, MD5; MITRE T1565.001 |

**Parsing notes from the samples:**

- `timestamp` looks like `2026-10-29T12:48:31.597+0600`: the offset has no colon, so it is not strict RFC 3339. `parse_time` takes an explicit layout. `predecoder.timestamp` has no year or timezone and is not used.
- `rule.mitre` and `data` are often absent; always use `get_path` and `coalesce`. `rule.id` is a string, `rule.level` is a number.
- `rule.mitre.id`, `technique`, and `tactic` are separate lists; tactics are not paired with techniques. IDs can be sub-techniques (for example `T1565.001`).
- Modified file events carry `changed_attributes` and `*_before` values only for attributes that changed; `*_after` hashes are present.
- Agent `000` is the Wazuh manager itself, and `agent.ip` is absent in these samples, so nothing depends on it.
- Compliance arrays (`pci_dss`, `gdpr`, `hipaa`, `nist_800_53`, `tsc`, `gpg13`) are kept under `ext`, not promoted to top-level fields.
- The hashes in the `npx-22` sample are those of empty input (and it has size 41 and mode `rwxrwxrwx`, so it looks like a symlink). `is_empty_hash` skips them so they never reach threat-intel lookups.
- Filter fields confirmed: `rule.id`, `rule.description`, `rule.groups`, `rule.level`, `decoder.name`, `decoder.parent`, `location`, `agent.*`, `data.*`, `syscheck.*`, `full_log`, `predecoder.program_name`.
- **Golden fixtures:** these samples (five manager alerts plus the indexer hit) are stored under `integrations/wazuh/testdata/` with their expected normalized output, and CI runs the mapping and the default filter set against them.

- **Actions (Wazuh manager REST API with JWT auth, *verify*):** get agent, list agents, restart agent, run active response (for example block an IP).

#### DFIR-IRIS 2.4.27 (case management: actions plus sync)

- **Auth:** each IRIS user has an API key sent as `Authorization: Bearer <key>` (confirmed in IRIS docs).
- **API generation:** IRIS 2.4.x uses the API described by the "v2.0.4" API reference, which applies to IRIS 2.4.0 and later (confirmed); case-scoped requests need a `cid` parameter. A partial `/api/v2` exists in 2.4.x but is incomplete (for example, case creation under `/api/v2` was reported missing in 2.4.22). **IRIS v3 drops the legacy endpoints**, and the IRIS docs publish a legacy-to-`/api/v2` mapping. Therefore the integration is written against an internal `CaseSystem` interface with two client implementations selected by `api_version: legacy_2_4 | v2`; only `legacy_2_4` is built now. Endpoint paths live in a per-version table in the manifest, not in action code. *Verify* each endpoint against 2.4.27.
- **Alerts:** IRIS has an Alerts API (since v2.1.0). It requires a service account with `alert_read` and `alert_write` permissions (confirmed).
- **Actions:** `iris.case.create|update|close`, `iris.alert.create|update`, `iris.ioc.add|list`, `iris.asset.add`, `iris.note.add`, `iris.timeline.event.add`, `iris.task.create`, `iris.alert.merge_into_case`. All carry `description`, `side_effect`, `risk`, and idempotency keys (the IRIS object id is stored in `external_refs`).
- **One service identity, many assignees:** the connection holds one service-account API key (with `alert_read`, `alert_write`, and the case permissions it needs). Per-user API keys are not used. Case and alert assignment fields take a list of IRIS usernames, resolved to IRIS user IDs and cached at connection test, so one workflow can assign to many IRIS users.
- **Gotcha:** IRIS IOC types and TLP levels are numeric IDs specific to each instance. The integration resolves names to IDs at connection test and caches them.
- **Case sync:** local case ↔ IRIS case through `external_refs`; observables become IOCs; alerts become IRIS alerts or case evidence. Inbound updates come from a `poll_http` source with a cursor on the update time (IRIS has a webhook module in 2.3.4+, which can be added as a push source later; *verify*).

#### Outlook mail (Microsoft Graph)

**Model: ingest from one shared mailbox, act on many mailboxes.**

- **Auth:** OAuth2 client credentials (app registration) with application permissions `Mail.Read`, `Mail.ReadWrite`, and `Mail.Send` (the first two confirmed in Graph docs). For now the app may reach all mailboxes. Restricting it to a defined group in Exchange Online (Application Access Policy or RBAC for Applications; *verify* which your tenant uses) is optional and can be turned on later with no code change. Tenant-wide `Mail.ReadWrite` and `Mail.Send` are powerful, so protect the app's credential (a certificate is preferred over a client secret) and rely on the approvals below for write and destructive actions.
- **Inbound source `outlook_mail`:** one source = one mailbox and folder (the shared phishing-report mailbox). More sources can be added later.
  - **Phase 1, polling:** Graph message delta, `GET /users/{id}/mailFolders/{id}/messages/delta`. The full `@odata.deltaLink` is the cursor (stored in `soar-cursors`); the mapper handles `@removed` entries and read/unread-only changes.
  - **Phase 2, change notifications:** Graph subscriptions (webhook). They expire quickly (about 4,230 minutes without resource data, 1 day with resource data), so a renewal job and lifecycle-notification handling are required. Polling stays as the safety net.
  - An `email` parser extracts headers, text/HTML body, links, and attachment metadata. Attachments go to MinIO (`soar-evidence`) with a hash. Reported emails become `alert` events with URL, domain, sender, and hash observables.
- **Reach into many mailboxes:** every action takes a `mailbox` parameter (UPN or ID) checked against the connection's `mailbox_scope`. Today: `allow: all`, `deny: []` (nothing is blocked). The deny list is supported by the code and config but empty, so entries can be added later with no code change. Actions: `outlook.mail.get`, `search`, `get_attachment`, `mark_read`, `move`, `reply`, `send`, `delete` (to Deleted Items), `purge` (permanent; *verify* Graph support).
- **Campaign response pattern:** search across a list of mailboxes → `foreach` with a concurrency cap (default 4) → move or delete. `dry_run` lists what would change. Graph throttling (`429` with `Retry-After`) is handled by the shared retry layer. A per-run mailbox cap (default 500) is configurable.
- **Sending to many mailboxes:** `outlook.mail.send` sends from a chosen mailbox (the shared mailbox by default) to `to`, `cc`, and `bcc` lists, split into batches as needed. It is `at_most_once` with an idempotency record to prevent double sends, and a recipient threshold (default 50, adjustable) above which policy requires approval.
- **Risk levels:** `get`, `search`, `get_attachment` are `read`; `mark_read`, `move`, `reply`, `send` are `write`; `delete` and `purge` are `destructive` and require approval by default; approvers are analysts and above (`analyst`, `engineer`, `admin`).

## 11. REST API (`/api/v1`)

Conventions: JSON; RFC 3339 UTC; `Authorization: Bearer <JWT|API key>`; `X-Request-ID`; cursor pagination (`limit` ≤ 200); `Idempotency-Key` on creates; `If-Match` on updates (409 on mismatch); errors as RFC 9457 problem details (`type, title, status, detail, request_id, errors[]`).

| Resource | Endpoints |
|---|---|
| Workflows | `POST/GET /workflows`, `GET /workflows/{id}`, `POST /workflows/{id}/versions`, `POST /workflows/{id}/publish`, `POST /workflows/validate` |
| Executions | `POST /workflows/{id}/run`, `GET /executions`, `GET /executions/{id}`, `GET /executions/{id}/steps`, `POST /executions/{id}/cancel|retry` |
| Approvals | `GET /approvals`, `POST /approvals/{id}/decision` |
| Alerts | `GET /alerts`, `GET /alerts/{id}`, `POST /alerts` |
| Cases | `POST/GET /cases`, `GET/PATCH /cases/{id}`, `POST /cases/{id}/tasks`, `POST /cases/{id}/observables`, `GET /cases/{id}/timeline` |
| Observables | `GET /observables`, `GET /observables/{id}` |
| Connectors | `GET /connectors`, `GET /connectors/{id}/actions` |
| Connections | `POST/GET/DELETE /connections`, `POST /connections/{id}/test` |
| Secrets | `PUT /secrets/{name}`, `GET /secrets` (metadata only), `DELETE /secrets/{name}` |
| Admin | `/principals`, `/roles`, `GET /audit` |
| Ingest | `POST /ingest/{source_id}` (one alert or a batch of up to 500; separate listener) |
| Sources | `POST/GET /sources`, `GET/PATCH /sources/{id}`, `POST /sources/{id}/test`, `GET /sources/{id}/health`, `POST /sources/{id}/replay`, `POST /mappings/test`, `GET/POST /mappings` |
| Filters | `POST/GET /filters`, `GET/PATCH /filters/{id}`, `POST /filters/test`, `POST /filters/simulate`, `GET/PUT /lists/{name}` |
| Queue | `GET /dlq`, `POST /dlq/{id}/replay`, `GET /queues/stats` |
| Ops | `/healthz`, `/readyz`, `/metrics` |

Realtime UI updates: `GET /stream` (Server-Sent Events) for execution and case changes.

## 12. Frontend

**Stack:** React 18 + TypeScript + Vite · React Flow (workflow canvas) · TanStack Query (server state) · Zustand (UI state) · React Router · Tailwind + shadcn/ui · Monaco (CEL/YAML editor) · generated API client from `openapi.yaml`.

| Page | Purpose |
|---|---|
| Dashboard | Alert volume, open cases, MTTR, automation rate, queue health |
| Alerts | Search, filter, dedup counts, link/create case |
| Cases | List, detail (observables, tasks, timeline, linked alerts, SLA) |
| Workflows | List, versions; **visual editor** (canvas ↔ YAML in sync), validate, test run (dry run) |
| Executions | List, live run view (node states on the canvas), step inputs/outputs, retry/cancel |
| Approvals | Pending human decisions |
| Sources | Configure sources, test mappings with sample payloads, view health, replay failed events |
| Connections | Configure and test integration credentials |
| Admin | Users, roles, API keys, audit log |

**Editor contract:** the canvas reads/writes the same DSL JSON the API validates; the server is the only validator (call `POST /workflows/validate` on change, show path-addressed errors inline).

## 13. Security

- **AuthN:** OIDC/JWT for users (Keycloak in dev and production; realm exported to `deploy/keycloak/realm.json`); hashed API keys (shown once); per-source ingest tokens/HMAC.
- **AuthZ:** roles `admin, engineer, analyst, viewer, service`; permissions as `resource:action` checked in middleware and store.
- **Secrets:** envelope encryption (AES-256-GCM data keys, master key via env/KMS/Vault, `key_id` for rotation); output scrubbing.
- **Hardening:** SSRF block (loopback, link-local, metadata IPs) on HTTP action; size/depth/CEL-cost limits; TLS/mTLS; audit every mutation and secret read; `govulncheck`, `pip-audit`, image scans in CI.

## 14. Observability

JSON logs (`slog`) with `request_id, execution_id, node_id, trace_id`. One OpenTelemetry trace per execution, `traceparent` in message envelopes. Prometheus metrics: `soar_ingest_events_total`, `soar_ingest_latency_seconds`, `soar_executions_total{status}`, `soar_execution_duration_seconds`, `soar_step_duration_seconds{action}`, `soar_step_retries_total`, `soar_queue_pending{stream}`, `soar_dlq_messages_total`, `soar_os_write_conflicts_total`, `soar_lease_takeovers_total`, `soar_source_events_total{source,result}`, `soar_source_lag_seconds`.
Targets are in §3.3. Also alert on `soar_queue_pending{stream="EVENTS"}` growth.

## 15. Reliability matrix

| Failure | Behavior |
|---|---|
| Worker dies mid-step | Redelivery after `AckWait`; idempotency prevents duplicate side effect |
| Orchestrator dies | Lease expires; another claims, reloads, republishes undispatched commands |
| OpenSearch conflict | Reload and retry (bounded) |
| OpenSearch down | Workers/orchestrator stop acking; ingest returns 503; JetStream retains |
| NATS down | Ingest 503; services reconnect with backoff |
| Poison message | `MaxDeliver` → DLQ + alert; replay via API |
| Actions that can't be idempotent | Declare `at_most_once: true`; no auto-retry |

Recovery sweeper (scheduler): finds `running` executions with expired leases, and outbox entries unpublished for more than 10 s, and republishes them.

## 16. Docker (dev)

```yaml
# deploy/compose/docker-compose.yml (outline)
services:
  opensearch:   { image: opensearchproject/opensearch:2.x }   # single node, 1-2 GB heap
  dashboards:   { image: opensearchproject/opensearch-dashboards }
  nats:         { image: nats:2, command: ["-js", "-sd", "/data"] }
  keycloak:     { image: quay.io/keycloak/keycloak }          # OIDC; realm imported from deploy/keycloak/realm.json
  minio:        { image: minio/minio, command: ["server", "/data"] }   # S3 API, buckets in §5.1
  migrate:      { image: soar:dev, command: ["soar","migrate"] }
  api:          { image: soar:dev, command: ["soar","api"],          ports: ["8080:8080"] }
  ingest:       { image: soar:dev, command: ["soar","ingest"],       ports: ["8081:8081"] }
  processor:    { image: soar:dev, command: ["soar","processor"] }
  collector:    { image: soar:dev, command: ["soar","collector"] }
  orchestrator: { image: soar:dev, command: ["soar","orchestrator"] }
  worker:       { image: soar:dev, command: ["soar","worker"] }
  scheduler:    { image: soar:dev, command: ["soar","scheduler"] }
  runner:       { image: soar-runner:dev }                    # no external network
  web:          { image: soar-web:dev, ports: ["3000:80"] }
```

Multi-stage Go build, static binary, non-root, distroless final image. Health: `/healthz` (alive), `/readyz` (deps reachable).

**Env vars:** `SOAR_OS_URLS`, `SOAR_OS_USER`, `SOAR_OS_PASSWORD`, `SOAR_NATS_URL`, `SOAR_MASTER_KEY_ID`, `SOAR_MASTER_KEY`, `SOAR_OIDC_ISSUER`, `SOAR_OIDC_AUDIENCE`, `SOAR_PARTITIONS`, `SOAR_LOG_LEVEL`, `SOAR_BLOB_ENDPOINT`, `SOAR_BLOB_ACCESS_KEY`, `SOAR_BLOB_SECRET_KEY`. Validate at startup; refuse to start on bad config.

**OpenSearch notes:** state indices 1 primary + 1 replica, refresh 1s; time-series use rollover at 20–40 GB/shard, refresh 5s; engine reads use `get` by ID (real-time), not search; retention is config-driven, 7 days for all data now (§5.2).

## 17. Build order (approve each gate before moving on)

| # | Step | Exit gate |
|---|---|---|
| 1 | Foundation: repo, Compose, Makefile, config, logging, CI, health | `make up` healthy; CI green |
| 2 | Domain types + index templates + migrations | Repeatable migrations; strict mappings |
| 3 | Storage layer (optimistic concurrency, bulk writer, repository interfaces) | Integration tests pass against real OpenSearch |
| 4 | API server + OpenAPI + error model | Contract tests pass |
| 5 | Ingestion framework (webhook and polling adapters, JSON parser, CEL mapping engine, source/mapping APIs with test endpoint) + processor, queue-first, with the **Wazuh 4.14** source pack (push and pull) as the first real source | 1,000/s for 30 min and 2,000/s burst for 60 s, zero loss (every `event_id` appears exactly once in alerts), p99 < 100 ms; queue tests in §8.10 pass |
| 6 | Engine core (DSL, validator, CEL, state machine) | Deterministic + property tests pass |
| 7 | Worker, built-in actions, Python runner, sandbox | Limits enforced |
| 8 | Reliability (retries, idempotency, DLQ, leases, sweeper) | Chaos (NATS node kill, processor/worker kill mid-batch): no lost events, no double side effects |
| 9 | Security (authN/Z, secrets, audit) | Security suite passes |
| 10 | Triggers and waits (cron, approvals, wait-for-event) + more adapters (syslog, email, Kafka, file/object) | Survives full restart |
| 11 | Cases and observables | Alert → case → task end to end |
| 12 | **Frontend**: shell + auth → alerts/cases → workflow editor → executions → admin | Analyst can run a playbook from the UI |
| 13 | **Integration SDK**: manifest (actions + sources + mappings), auth handlers, test harness, first two integrations (DFIR-IRIS, Outlook mail; §10.1) | A new SIEM source added with config only, in under 1 hour |
| 14 | Observability + load/soak | 24 h soak with daily traffic shape and hourly 1,000/s peaks; baselines documented |
| 15 | Packaging (prod Compose/Helm, backup/restore, runbooks) | Clean install < 15 min |
| 16 | AI extension (later; phases A1-A4 in §20) | Each phase has its own gate |

## 18. Conventions

- **Go:** `gofumpt`, `golangci-lint`, `context` first param, errors wrapped with `%w`, no global state, table-driven tests, `testcontainers-go` for integration.
- **API:** change `openapi.yaml` first; regenerate server stubs and the TS client; contract tests in CI.
- **Commits:** Conventional Commits; one step = one milestone branch; merge only when the exit gate is demonstrated.
- **Definition of done:** tests pass, lint clean, logs/metrics added, docs updated, exit gate shown.

## 19. Step 1 checklist (start here)

1. `go mod init github.com/<you>/soar`; create the layout in §4.
2. `cmd/soar/main.go` with subcommands; each logs on start and serves `/healthz` and `/readyz`.
3. Makefile: `up, down, logs, test, lint, build, migrate`.
4. Compose with `opensearch`, `nats`, `dashboards`, and the Go roles.
5. Config loader with validation (fail fast).
6. `slog` JSON logging with request/correlation IDs.
7. GitHub Actions: lint, unit tests, integration tests, image build.
8. Verify: `make up` then `curl localhost:8080/readyz` returns 200 with OpenSearch and NATS reachable.

## 20. AI-ready architecture (hooks now, AI later)

**Principle:** AI is not a special subsystem. An agent is another kind of workflow step that uses the same tools, approvals, audit, idempotency, and tracing as everything else. Nothing AI-specific is built now; the items below are cheap decisions that avoid a rewrite later.

### 20.1 Add now (cheap)

| Hook | What to do now | Why it helps AI later |
|---|---|---|
| Tool-ready action manifests | Required `description`, `side_effect`, `risk`, JSON Schema params/outputs, `supports_dry_run` (§10) | Every action becomes a safe, self-describing agent tool and can be exposed over MCP |
| Dry run | Workflows and write actions support `dry_run: true` | Safe testing, evals, and "propose, don't act" agent modes |
| Actor model | `actor_type` (`user`, `workflow`, `agent`, `system`) on audit and step runs; `parent_step_id` on step runs | Agent tool calls appear as child steps with full audit and tracing |
| Policy engine | `internal/policy`: `Decide(actor, action, args, risk) → allow | deny | require_approval`, CEL rules, RBAC-backed | Same guardrails for humans, workflows, and agents; approvals for write/destructive tools |
| Reserved DSL node | `type: agent` is reserved; the validator rejects it with "not enabled" | Playbooks written later need no engine redesign |
| Text fields | `summary` on alerts and cases; `verdict` on alerts and cases; `external_url` | Context for models; ground-truth labels for evaluation |
| Recorded outputs | Step outputs are stored (`output_ref`); replay uses stored results | Non-deterministic agent decisions are never silently re-run |
| Structured outputs | Actions declare `output_schema` and the worker validates it | Reliable tool results and validated model output |
| Reserved packages and indices | `internal/ai/` (empty interfaces) and reserved names `soar-agent-runs`, `soar-memory` | Clear home for the later work |
| Untrusted-data labeling | Alert and case free text is marked as external (`ext`, `raw`) | Basis for prompt-injection defenses |

### 20.2 Planned design (built later)

- **Agent node:** `type: agent` with `goal`, `tools` (allowlist), `max_steps`, `max_cost`, `approval.required_for: [write, destructive]`, `output_schema`. It runs as one long-running step (`ai.agent.run`) on a worker that calls the Python runner over a streaming gRPC `RunAgent` RPC. The engine itself is unchanged: it dispatches a step and waits for the result.
- **Tool calls:** each agent tool call goes through the normal action path (policy check, secrets, idempotency, audit) and is recorded as a child step run.
- **Model gateway** (`internal/ai/models`): supports every model type through capability-based interfaces and a registry of providers.

| Model type | Interface | Provider examples |
|---|---|---|
| Chat / LLM (tools, structured output) | `Chatter` | Hosted APIs (Anthropic, OpenAI, Google, Azure OpenAI, AWS Bedrock, Vertex AI) and local servers (Ollama, vLLM, llama.cpp, LM Studio) through an `openai_compatible` adapter |
| Embeddings | `Embedder` | Hosted or local |
| Reranking | `Reranker` | Hosted or local |
| Vision / multimodal | `Chatter` with image input | Hosted or local |
| Classical ML (classifiers, anomaly, NER) | `Classifier` | ONNX, Triton, KServe, or any HTTP service, hosted in the Python runner |
| Custom HTTP model | any | `http_custom` adapter with declarative request/response mapping (same CEL approach as ingestion) |

```yaml
# soar-models registry entry
id: local-chat
provider: openai_compatible
endpoint: http://vllm:8000/v1
model: <model-name>
capabilities: [chat, tools, structured_output]
limits: { context_tokens: 32000, rpm: 120 }
cost: { input_per_mtok: 0, output_per_mtok: 0 }
data_policy: { allowed_data_classes: [public, internal, sensitive] }   # default: everything allowed
auth: { secret_ref: local_chat_key }
```

  Routing policy picks a model per task by capability, data class, cost, and latency, with fallback. **No data restrictions by default:** any data (alerts, cases, observables, emails, attachments) may be sent to any model type. The `data_policy` field and an optional redaction hook exist so restrictions can be added later per model without code changes. The gateway always records which model received which data classes (audit), and secrets and credentials are never sent to a model (a technical safeguard, not a data restriction). Timeouts, retries, budgets, cost accounting, and response caching are handled in the gateway, not in agents or workflows.

```go
type LLM interface {
    Complete(ctx context.Context, req Request) (Response, error) // messages, tools, schema, limits
}
llm.Register("anthropic", newAnthropic) // other providers register the same way
```

- **Memory and retrieval:** OpenSearch k-NN vectors on alert and case text for similar-incident search; `soar-memory` holds case summaries, analyst feedback, and playbook notes.
- **MCP server (`soar mcp`):** exposes actions as tools and alerts, cases, and observables as resources to external agents and IDE assistants, under the same auth and policy.
- **Evaluation:** golden sets of alerts with expected verdicts; replay through `dry_run`; track accuracy, cost, latency, and approval overrides.
- **Observability:** prompts, tool calls, and responses recorded (redacted) in `soar-agent-runs`; one trace per agent run.

### 20.3 Safety rules for AI features

1. Agents run under a scoped service principal (`actor_type=agent`); they never receive secrets, and the model never sees credentials.
2. Alert and ticket text is untrusted input: kept separate from instructions, never allowed to widen the tool allowlist, and model output is validated against a schema before use.
3. Read tools are free; write tools need policy approval; destructive tools need a human approval by default.
4. Hard limits per run: steps, tokens, cost, wall-clock time.
5. Agents have no direct network access; everything goes through actions.

### 20.4 Phases

| Phase | Scope | Needs |
|---|---|---|
| A1 | LLM actions inside normal workflows (`ai.llm.complete`: summarize, classify, extract IOCs) | LLM gateway, secrets, output schemas |
| A2 | Analyst copilot: chat over a case with read-only tools; similar-case retrieval | Vector fields, MCP/tool layer |
| A3 | Agent node with approvals for write tools | Policy engine, `RunAgent` RPC, agent-run records |
| A4 | Autonomous triage and playbook drafting, measured by evals | Eval harness, verdict history |

## 21. Resolved decisions and open questions

### 21.1 Resolved

| # | Topic | Decision |
|---|---|---|
| 1 | Alert rate | Peak 1,000/s, average 100/s; test at 1,000/s and a 2,000/s burst |
| 2 | Retention | 7 days for data (alerts, raw, step runs, executions, audit); cases and their evidence 3 days after closure (confirmed); 365 days later, controlled by `config/retention.yaml` |
| 3 | Large payloads | MinIO (S3 API), §5.1 |
| 4 | OIDC | Keycloak (dev and production) |
| 5 | First integrations | DFIR-IRIS 2.4.27 and Outlook mail (§10.1) |
| 6 | NATS in production | 3-node JetStream cluster, replicas 3 |
| 7 | First SIEM | Wazuh 4.14, single node, both push (Integrator) and pull (indexer) |
| 8 | AI models | All model types through the model gateway; no data restrictions (§20.2) |
| 9 | IRIS users | One service identity; assignment to many IRIS users is enough |
| 10 | Outlook | Ingest from one shared mailbox; act on and send to all mailboxes (deny list supported but empty); delete/purge and sends to more than 50 recipients require approval |
| 11 | Wazuh filtering | Flexible filter sets: CEL on any field, managed lists, scripts, push-down to the indexer (§9.5.1) |
| 12 | Wazuh samples | Five manager alerts and one indexer hit received; mapping, filter fields, pull cursor, and fixtures finalized (§10.1) |
| 13 | Approvers | Analysts and above can approve destructive actions and large sends |


