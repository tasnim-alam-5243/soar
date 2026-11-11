# SOAR platform

Implementation of the SOAR technical spec v0.9: ingestion from any SIEM or app, a durable workflow engine,
actions and integrations in a sandboxed runner, cases and observables, approvals and policy, security, a web
UI and the operational tooling around it.

One Go binary (`soar`), one image, role chosen by subcommand, plus the Python runner image and the web image.

## Status

| Step | What | State |
|---|---|---|
| 1-4 | Foundation, indices and migrations, storage layer, REST API | done (committed) |
| 5 | Queue-first ingestion, Wazuh 4.14 push and pull | done; load gates need the real stack |
| 6 | Engine: DSL, validator, CEL, pure state machine | done; deterministic, property and fuzz tests |
| 7 | Worker, built-in actions, Python runner (gRPC, sandbox) | done |
| 8 | Reliability: retries, idempotency, outbox sweeper, DLQ, leases | done; chaos-tested (processor, worker, orchestrator and NATS kills) |
| 9 | Security: OIDC + API keys, roles, encrypted secrets, audit, scrubbing, TLS/mTLS, policy | done; `make test-security` passes |
| 10 | Triggers and waits: cron, approvals, timers; Outlook (Graph) mailbox adapter | partly: syslog, Kafka and file/object adapters and wait-for-event nodes are not built |
| 11 | Cases and observables: alert -> case -> task, snapshots, evidence | done |
| 12 | Web UI: dashboard, alerts, cases, workflow editor, executions, approvals, sources, connections, admin | done |
| 13 | Integration SDK, DFIR-IRIS 2.4.27 and Outlook (Microsoft Graph) | done; endpoints marked *verify* |
| 14 | Prometheus metrics, alert rules | done; the 24 h soak is yours to run |
| 15 | Production Compose, backup/restore, runbooks | done except a Helm chart |
| 16 | AI (later) | hooks only (policy, actor model, dry runs, manifests, reserved `agent` node) |

## Quick start (Rocky Linux 10.x)

```bash
sudo ./scripts/rocky10-setup.sh      # once: Docker CE, compose plugin, Go, make, vm.max_map_count
# log out/in so the docker group applies, then:
cp deploy/compose/.env.example deploy/compose/.env   # replace the public development secrets
make up                              # builds soar, soar-runner and soar-web, starts the stack, waits for /readyz
open http://localhost:3000           # UI; Keycloak dev users: admin/admin, engineer/engineer, analyst/analyst, viewer/viewer
make test-all                        # Go, Python runner and UI tests (make runner-venv and make web-install first)
make test-integration                # needs the stack running
make down                            # stop (keep data) | make clean (delete data)
```

API from the command line with the bootstrap admin key (development default; change it in `.env`):

```bash
export SOAR_KEY=soar_dev-bootstrap-key-change-me-0123456789abcdef
curl -s -H "Authorization: Bearer $SOAR_KEY" localhost:8080/api/v1/me | jq .
```

`make help` lists every target. Production: `deploy/prod/` and [docs/runbooks.md](docs/runbooks.md).

## Ports (all bound to 127.0.0.1)

| Service | Port | Notes |
|---|---|---|
| web | 3000 | React UI; nginx proxies `/api` to the api |
| api | 8080 | `/api/v1/...` (OIDC or API key), `/healthz`, `/readyz`, `/metrics` |
| ingest | 8081 | `POST /ingest/{source}` (per-source token or HMAC) |
| processor / collector / orchestrator / worker / scheduler | 8082-8086 | health, `/metrics`, `/stats`, inside the compose network |
| OpenSearch / Dashboards | 9200 / 5601 | security plugin **off** in dev |
| NATS | 4222, 8222 | JetStream |
| S3 object storage (RustFS) | 9000 (S3), 9001 (console) | service `s3`; buckets created by `migrate`; keys `S3_ACCESS_KEY` / `S3_SECRET_KEY` |
| Keycloak | 8180 | realm `soar` imported from `deploy/keycloak/realm.json` (development users) |
| Prometheus | 9090 | `docker compose --profile monitoring up -d` |

## Configuration (environment, validated at start, bad config = exit 2)

| Variable | Required | Default |
|---|---|---|
| `SOAR_OS_URLS` | yes | comma-separated http(s) URLs |
| `SOAR_OS_USER` / `SOAR_OS_PASSWORD` | no | |
| `SOAR_NATS_URL` | yes | one or more `nats://` URLs, comma-separated |
| `SOAR_PARTITIONS` | no | 8 (1-256) |
| `SOAR_LOG_LEVEL` | no | info |
| `SOAR_HTTP_ADDR` | no | role default (table above) |
| `SOAR_INDEX_PREFIX` | no | `soar` (lowercase letters, digits, `-`, `_`) |
| `SOAR_OS_STATE_REPLICAS` / `SOAR_OS_TS_REPLICAS` | no | 0 (0-5); set 1 in production |
| `SOAR_RETENTION_FILE` | no | `/etc/soar/retention.yaml` (baked into the image from `config/retention.yaml`) |
| `SOAR_EVENTS_MAX_BYTES`, `SOAR_NATS_REPLICAS`, `SOAR_PROCESSOR_WORKERS` | no | 20 GiB, 1, 2 |
| `SOAR_BLOB_ENDPOINT` + `SOAR_BLOB_ACCESS_KEY` / `SOAR_BLOB_SECRET_KEY` | no | any S3-compatible store (path-style, SigV4); `SOAR_BLOB_DIR` for a filesystem store; neither: large payloads stay inline |
| `SOAR_BLOB_REGION`, `SOAR_BLOB_SSE` | no | us-east-1, false |
| `SOAR_MASTER_KEY_ID` / `SOAR_MASTER_KEY` | for secrets | 32 random bytes in base64; `SOAR_OLD_MASTER_KEYS=id:key,...` during rotation |
| `SOAR_OIDC_ISSUER` / `SOAR_OIDC_AUDIENCE` / `SOAR_OIDC_JWKS_URL` | api | Keycloak realm URL, `soar-api`, discovered when empty |
| `SOAR_BOOTSTRAP_API_KEY` | no | first admin API key (`soar_` + 32+ characters) |
| `SOAR_AUTH_DISABLED` | no | false; true makes every request an admin (local experiments only) |
| `SOAR_CORS_ORIGINS` | no | only when the UI is served from another origin |
| `SOAR_RUNNER_ADDR` | worker | `runner:50051`; without it python actions fail with `runner_unavailable` |
| `SOAR_WORKER_PRIORITIES` / `SOAR_WORKER_CLASSES` / `SOAR_WORKER_CONCURRENCY` | no | all / all / 16 |
| `SOAR_POLICY_FILE` | no | `/etc/soar/policy.yaml` (`config/policy.yaml`) |
| `SOAR_INTEGRATIONS_DIR` | no | built-in integration packages |
| `SOAR_HTTP_ALLOW_LOOPBACK` | no | false (SSRF guard of http.request) |

## Layout

Follows spec §4, plus `internal/cases` (case logic shared by the API and actions), `internal/platform`
(shared wiring of the roles), `internal/jsonschema`, `examples/workflows`, `deploy/{keycloak,web,prometheus,prod}`.
Import rule: `domain` imports nothing internal; nothing imports `cmd`.

## Step 1 exit gate

- [ ] `make up` finishes and all containers are healthy (`make ps`)
- [ ] `curl localhost:8080/readyz` returns 200 with `opensearch` and `nats` both `ok`
- [ ] `make test` and `make vet` pass; CI green on GitHub
- [ ] `docker stop soar-nats-1` makes `/readyz` return 503 within seconds; starting it restores 200
- [ ] `docker compose -f deploy/compose/docker-compose.yml run --rm -e SOAR_PARTITIONS=0 api` exits 2 and logs every problem

## Step 2: indices, retention and migrations

`soar migrate` creates, for each of the 22 indices in spec section 5 plus `soar-approvals`, an index template with `dynamic: strict`
mappings, the first index and its alias (`soar-alerts`, `soar-cases`, ...). Five time-series indices (`alerts`, `raw`,
`steprun`, `timeline`, `audit`) use rollover (30 GB or 1 day) plus an ISM delete policy generated from
`config/retention.yaml`. A ledger index (`soar-migrations`) stores a checksum per index, so repeating the command
changes nothing.

```bash
make migrate         # apply (the stack also runs this automatically before every service starts)
make migrate-check   # verify only; exit 1 on drift
```

Changing retention: edit `config/retention.yaml`, run `make migrate`, check with `make migrate-check`.
Changing a mapping: edit `internal/store/opensearch/schema/entities.go`; additive changes apply in place.
An incompatible change needs a `v2` index and `_reindex`.

### Step 2 exit gate

- [ ] `make migrate` twice: second run reports every index `unchanged`
- [ ] `make migrate-check` exits 0
- [ ] unknown field is rejected: `curl -s -XPUT localhost:9200/soar-workflows/_doc/t -H 'Content-Type: application/json' -d '{"x":1}'` returns `strict_dynamic_mapping_exception`
- [ ] `make test` and `make test-integration` pass

## Step 3: storage layer

All reads and writes of documents go through `internal/store` (interfaces) and
`internal/store/opensearch` (the only OpenSearch code). Services never build OpenSearch requests.

- `store.Repo[T]`: `Get`, `Create`, `Put`, `Save` (optimistic: fails with `ErrVersionConflict` if the
  document changed since it was read), `Update` (read-modify-write, bounded retries, then `ErrConflictExhausted`),
  `Delete`, `List` (filters, ranges, one sort key, cursor pagination).
- `store.BulkWriter`: many writes per request with a result per item (`OK`, `Conflict`, `Retryable`, `Permanent`),
  optional per-item version checks, scripted upserts. `opensearch.GroupWriter` merges concurrent callers into
  shared bulk requests (flush at 1,000 ops or 250 ms) and returns only after OpenSearch acknowledged.
- `internal/store/memory` is an in-process implementation for unit tests. `internal/store/storetest` is the
  behaviour contract; memory and OpenSearch must both pass it.
- Time-series indices (`alerts`, `steprun`, ...) are read through a search and cannot be deleted by ID:
  retention removes them. A just-written time-series document is searchable after the next refresh (up to 5 s).

## Step 4: REST API

`api/openapi.yaml` is the contract (served at `/api/v1/openapi.yaml`). Change it first, then the code.
`go test ./internal/api` fails when a route and the document disagree, when a response breaks its schema, or when
an operation is never exercised.

Authentication arrived in Step 9: send `Authorization: Bearer <token>` (see Step 9 below).

| Area | Endpoints (all under `/api/v1`) | Status |
|---|---|---|
| Workflows | `POST/GET /workflows`, `GET/PUT /workflows/{id}`, `POST /workflows/{id}/versions`, `/publish`, `POST /workflows/validate` | done (structural validation; CEL checks in Step 6) |
| Run / cancel / retry | `POST /workflows/{id}/run`, `POST /executions/{id}/cancel`, `POST /executions/{id}/retry` | done (Steps 6-8) |
| Executions | `GET /executions`, `GET /executions/{id}`, `GET /executions/{id}/steps` | done (read side) |
| Alerts | `GET/POST /alerts`, `GET /alerts/{id}` | done (POST is manual entry; ingestion is Step 5) |
| Cases | `POST/GET /cases`, `GET/PATCH /cases/{id}`, tasks, observables, timeline | done (alert linking in Step 11) |
| Observables | `GET /observables`, `GET /observables/{id}` | done |
| Connections | `POST/GET /connections`, `GET/DELETE /connections/{id}` | done (`/test` in Step 13) |
| Approvals, secrets, principals, roles, audit, connectors, dashboard, `/stream` | | done (Steps 7-12) |

Conventions: errors are `application/problem+json` with `request_id`; lists return `{"items": [...], "next_cursor": "..."}`
(`limit` 1-200); creates accept `Idempotency-Key`; single resources carry an `ETag`, and `If-Match` on `PUT`/`PATCH`/publish
turns the update into a compare-and-set (409 on mismatch).

Try it:

```bash
curl -s -H "Authorization: Bearer $SOAR_KEY" -XPOST localhost:8080/api/v1/cases -H 'Content-Type: application/json' \
  -d '{"title":"Phishing campaign","severity":4}' | jq .
curl -s -H "Authorization: Bearer $SOAR_KEY" 'localhost:8080/api/v1/cases?status=open' | jq .
curl -s localhost:8080/api/v1/openapi.yaml | head
```

### Steps 3-4 exit gates

- [ ] `make test` passes (includes the contract tests and the shared store suite on the in-memory store)
- [ ] `make test-integration` passes (same store suite on real OpenSearch, bulk, group writer, API on the real stack)
- [ ] the curl commands above work against `make up`

## Step 5: ingestion (queue first)

```
Wazuh --push--> ingest (:8081) --\
                                  +--> NATS EVENTS --> processor --> OpenSearch (alerts, observables, executions)
Wazuh indexer <--poll-- collector -/                      \--> DLQ (poison messages, unmappable records)
```

* **ingest** authenticates, parses, filters, maps, publishes to JetStream and answers `202` only after NATS
  stored the event. It never waits for OpenSearch. A full queue gives `503` + `Retry-After`; a noisy source `429`.
* **collector** polls pull sources on a schedule and moves the cursor only after NATS acknowledged every event.
  A lease in `soar-cursors` makes sure one replica polls a source at a time.
* **processor** takes batches of 256, drops exact duplicates (`soar-seen`), groups repeats into one alert per
  fingerprint and 10-minute window (`occurrence_count`), upserts observables, starts matching workflows, and acks a
  message only after its writes succeeded. Poison messages go to the DLQ. While OpenSearch is unreachable or
  overloaded (connection errors, 429/502/503/504) it holds the fetched batch, keeps it alive with `InProgress`
  and pauses fetching, so an outage of any length costs no deliveries (see docs/runbooks.md "OpenSearch outage").
* Sources, mappings, filter sets and managed lists are data (`/api/v1/sources`, `/mappings`, `/filters`, `/lists`),
  each with a dry-run test endpoint. Mappings and filter sets are versioned and immutable.

### Wazuh quick start

```bash
make up                          # runs migrate (adds two index fields) and seed (installs the Wazuh pack)
curl -s -H "Authorization: Bearer $SOAR_KEY" localhost:8080/api/v1/sources | jq '.items[].name'          # wazuh-prod, wazuh-pull
```

**Push** (v1): copy `integrations/wazuh/push/custom-soar*` to the manager and add the `<integration>` block from
`integrations/wazuh/README.md`; `api_key` is `SOAR_SOURCE_TOKEN_WAZUH_PROD` (set it in `deploy/compose/.env`).
Try it without Wazuh:

```bash
curl -s -XPOST localhost:8081/ingest/wazuh-prod -H "Authorization: Bearer $SOAR_SOURCE_TOKEN_WAZUH_PROD" \
  -H 'Content-Type: application/json' -d @integrations/wazuh/testdata/05_sshd_auth_success.json
curl -s -H "Authorization: Bearer $SOAR_KEY" 'localhost:8080/api/v1/alerts?source_id=wazuh-prod' | jq '.items[0] | {title,severity,occurrence_count,observables}'
```

**Pull** (v1): create a read-only indexer user, set `SOAR_WAZUH_INDEXER_URL`, `_USER`, `_PASSWORD` (and `_CA_FILE`, or
`_INSECURE=true` for development only) in `.env`, then `make seed && make up`. The collector lists the last two daily
indices, never `wazuh-alerts-*`, and:

* pages with `search_after` on `@timestamp` then `id` (spec 10.1); if the indexer cannot sort on `id` it drops that key
  once, logs a warning, and relies on the overlap plus the duplicate guard;
* stores the cursor as `{ts, after}` and only ever moves it forward. Up to `max_pages_per_cycle` (20 x 500) pages per
  cycle; a capped cycle is followed at once by the next, so a backlog is read at full speed (~1,000/s measured);
* once caught up, re-reads the 60 s overlap before where the catch-up began, newest first, within `overlap_pages`
  (40 x 500 = the whole overlap up to ~330 alerts/s; above that its newest part). Records it published moments ago are
  skipped before mapping; anything else repeated is dropped by the duplicate guard. An alert that arrives by push and
  by pull is stored once (both sources share `system: wazuh-prod`).

`soar seed` never overwrites a source an operator may have edited. A stack seeded before this paging existed keeps the
old `wazuh-pull` config (it still works, less efficiently); to adopt the new one, `PATCH /api/v1/sources/{id}` with the
`config` from `integrations/wazuh/sources/wazuh-pull.yaml`, or delete the source and run `make seed`.

**Tune filtering without a deploy**: `PUT /api/v1/lists/wazuh_noisy_rule_ids {"items":["5501"]}`; check with
`POST /api/v1/filters/test`. Keep the Wazuh Integrator filter coarse (`<level>3</level>`): changing it needs a manager restart.

Other useful calls: `GET /queues/stats` (backlog and consumer lag), `GET /dlq`, `POST /dlq/{id}/replay`,
`POST /mappings/test` (shows the exact failing expression), `GET /sources/{id}/health`.

### Step 5 exit gate

```bash
make test                        # unit tests incl. embedded JetStream, golden Wazuh fixtures, processor, collector
make test-integration            # the real stack, incl. a 3,000-event zero-loss check and an alert-triggered workflow run
                                 # (env: SOAR_API_KEY, SOAR_INDEX_PREFIX, SOAR_INGEST_URL/SOAR_API_URL, SOAR_IT_HTTP_URL;
                                 #  the defaults match `make up`)
make loadtest RATE=1000 DURATION=30m     # the 30-minute peak test; prints PASS/FAIL per gate
make loadtest-burst              # 2,000/s for 60 s, queue must drain within 5 minutes
```

- [ ] zero loss: stored occurrences == accepted events, each event guarded exactly once
- [ ] no 5xx, p99 to `202` below 100 ms (client side)
- [ ] queue drains after the burst (`drained ... after the last send` under 5 minutes)

### Measured: what limits throughput

First run on a Rocky Linux VM with `sync_interval: always`: 2,000/s requested, **1,112/s achieved**, p50 116 ms, p99 202 ms
(workers / throughput = 128 / 1,112 = 115 ms: a serial resource at ~1,100/s), zero loss, queue drained. The cause is NATS
syncing to disk on every write: each event costs two fsyncs (publish, and the processor's acknowledgement, which deletes
it from the work queue). An isolated measurement on a faster disk showed the same effect: publish 65,000/s without per-write
fsync versus 4,800/s with it, p50 1.3 ms versus 26 ms.

```bash
make nats-relaxed                        # fsync every 200 ms (deploy/nats/nats-relaxed.conf), recreates the nats container
make loadtest-burst                      # compare with the first run
make nats-always                         # back to fsync on every write
```

Trade-off to decide on purpose: with batched fsync an event acknowledged less than 200 ms before a power loss, kernel panic or
VM crash can be lost; restarting NATS or its container loses nothing. Production runs a 3-node cluster with replicas 3 (a message
is acknowledged only after a quorum has it), which is the real protection. If you need per-write fsync on one node, use a faster
disk (NVMe or a controller with a battery-backed cache) and keep the lower throughput until then.

After NATS is no longer the limit the next one is OpenSearch/processor: watch `GET /api/v1/queues/stats` during a run (EVENTS
`pending` should stay small), and raise `SOAR_PROCESSOR_WORKERS` (try 4) in `deploy/compose/.env`, then `make up`.
`make loadtest` prints a hint when the target rate is not reached.

Notes: the six Wazuh fixtures were built from the spec's field descriptions, replace them with real samples (see
`integrations/wazuh/README.md`). `occurrence_count` can overshoot by the events of one batch after a processor crash
between its two writes; events are never lost and never duplicated as documents. Replay of stored raw events
(`/sources/{id}/replay`, `/filters/simulate`) needs full raw capture and comes with the DLQ tooling in Step 8.


## Step 6: execution engine

`internal/engine` is the pure state machine of spec §7: `Next(workflow, state, event) -> (commands, newState)`.
No I/O, no clock (the event carries its time), deterministic jitter, so a replay always takes the same path.

* **DSL** (YAML/JSON, CEL): node types `action`, `branch`, `parallel`, `foreach` (concurrency cap, up to 1,000
  items), `wait` (`duration` or `until`), `approval` (approvers, timeout, `on_timeout`, `on_reject`), `set`,
  `subworkflow`, `end`; `agent` is reserved and rejected. Common fields: `when` (skip but keep the flow),
  `next`, `on_error`, `continue_on_error`; actions add `params`, `retry`, `timeout`, `idempotency_key`,
  `priority`. Workflow `inputs` (computed from the alert), `outputs`, `deadline` (default 24 h). See
  [docs/workflow-dsl.md](docs/workflow-dsl.md) and [examples/workflows](examples/workflows).
* **Params:** a string that compiles as CEL against `alert, inputs, vars, steps, env, item, index, secrets`
  is an expression; anything else is a literal (`method: GET`). Quote a literal that happens to be valid CEL: `'"1 + 2"'`.
* **Joins:** a node runs when all its predecessors finished and at least one activated it; others are skipped
  (dead-path elimination), so joins after branches and fan-outs need no special syntax.
* **Validation at save time** (`POST /workflows/validate`, publish): unique ids, existing targets, no cycles,
  reachability, CEL compiles, action exists and literal params match its JSON schema, limits.
* **Secrets** in expressions (`"Bearer " + secrets.token`) become placeholders; only the worker fills them in.
* **Triggers** may be written inside the definition (as in `examples/workflows`) or as the workflow's `triggers`.
  The API stores both in `triggers` (merged, no duplicates) and drops them from the stored definition; the
  processor and the cron scheduler also read triggers left inside definitions stored before that.
* **State machine:** `engine.ValidTransition` is the §7 table (terminal states are final, nothing returns to
  `queued`; a queued execution may be cancelled). `Next` refuses an illegal change instead of storing it
  (`Result.Invalid`, logged by the orchestrator as an engine bug).

Exit gate: `go test ./internal/engine` (unit tests per node type, 600 random workflows with shuffled and
duplicated events checking termination, legal transitions, single dispatch per attempt and deterministic replay,
and a fuzzer).

## Step 7: worker, actions, Python runner

* **Actions** register by name with a manifest (`description`, `side_effect`, `risk`, `supports_dry_run`, schemas,
  `retryable_errors`, `at_most_once`, `limits`). Built-ins: `http.request` (SSRF guard: loopback, link-local and
  metadata addresses blocked; private CAs through `ca_pem`), `transform.map`, `delay.sleep`, `notify.log`,
  `case.create|update|close`, `observable.add`, `alert.update`. `GET /connectors` and `GET /connectors/{id}/actions` list them.
* **Worker** (spec §10): idempotency record (`done` -> republish the stored result), secrets and connection
  resolved in the worker only, timeout, heartbeats (`InProgress`), output schema check, step run, result, ack.
  `critical`, `normal` and `bulk` priorities have separate capacity.
* **Runner** (`runner/python`, gRPC contract `api/proto/runner.proto`): every call runs in a child process with
  rlimits (memory, CPU, files), an empty environment, a wall-clock kill and secret values scrubbed from logs and
  errors. The manifest's `cpu` (a share of one core) is a CPU-time budget of `cpu x timeout` seconds; a busy action
  that exceeds it fails with `cpu_limit` (not retried) before its timeout. Integration code uses `soar_sdk` (`ActionContext`, `HTTP` with egress allowlist, `ActionError`).
  The container is non-root, read-only, without capabilities. `make proto` regenerates the stubs.

Exit gate: `go test ./internal/runner` (the Go client against the real Python runner) and `make test-python`
(timeout kill, CPU budget, memory limit, secret scrubbing, empty environment, path-escape refusal).

## Step 8: reliability

* **Outbox** (spec §8.4): state and pending commands are saved in one versioned write, then carried out
  (publish steps, create timers/approvals/child executions) and dropped. The scheduler sweeps entries older than 10 s.
* **Two retry layers:** business retries are engine timers with backoff; delivery retries are JetStream redeliveries.
* **OpenSearch outages are not delivery failures:** the processor, worker and orchestrator hold in-flight messages
  (heartbeats keep them) and retry in place until OpenSearch answers; only connection errors and 429/502/503/504
  count. Verified with a ~40 s cut-off during 1,000 events/s: zero loss, no redeliveries, running workflows finished.
* **DLQ:** max-delivery advisories move messages to `dlq.<origin>`; a dead-lettered step is reported to its
  execution as `delivery_failed` so its error path runs instead of waiting for the deadline.
* **Scheduler** (leader lease in `soar-cursors`): due timers every second, outbox and queued sweepers, cron,
  TTL purge, retention cleanup (ended executions, closed cases with their tasks, snapshots and evidence objects).

* **Who advances an execution (the "lease"):** the orchestrator holding a message of the execution's partition;
  JetStream's ack deadline is the lease. If it dies, the message is redelivered to another replica after the
  deadline, which reloads the state (optimistic concurrency keeps two writers safe) and the sweeper republishes any
  unpublished outbox entry. The `lease` field of the execution document is reserved and not written.
* **Exact counting after a processor crash:** the alert increment is the one write that is not idempotent, so the
  duplicate guard marks the event `applied` in the same bulk request; a redelivered event skips the increment and
  repeats only the idempotent writes (observables, execution creation).

Exit gate: `go test ./internal/e2e -run Flow` runs JetStream, orchestrator, worker and scheduler together:
branch + case, retries through timers, error paths, a step redelivered three times running its side effect once,
the outbox sweeper finishing after a publish failure, policy approvals, a poison step dead-lettered, subworkflow,
foreach and cancel.

Chaos runs (2026-11-11, real processes killed with `kill -9`, OpenSearch + one JetStream node with fsync on every write):

| Spec 8.10 | Run | Result |
|---|---|---|
| #2 processor killed mid-batch | 1,000 events/s for 60 s, processor killed twice; 300 alert-triggered executions | 60,000 accepted = 60,000 occurrences stored, each event guarded once; one execution per alert. (The first run counted 160 events twice: fixed, see above.) |
| #3, #4 worker / orchestrator killed mid-step | 60 executions (sleep, `case.create`, log) while the worker was killed 12 times and the orchestrator 5 times | all succeeded (55 needed a redelivered step), exactly one case each |
| #5 NATS node killed | 500/s for 40 s, NATS killed for 5 s | 13 requests answered 503 during the outage (never a false 202); all 19,987 accepted events stored once; every role reconnected |
| #7 poison message | unmappable record | dead-lettered without blocking the queue; `POST /dlq/{seq}/replay` republishes it |
| #6 OpenSearch outage | see above | zero loss, no redeliveries |

The p99 ingest latency in these runs was ~225 ms because the test node fsyncs every write; see "Measured: what
limits throughput" and `make nats-relaxed`.

## Step 9: security

* **Authentication:** OIDC access tokens (Keycloak; RS256/ES256 via JWKS, issuer, audience, expiry) or API keys
  (`soar_...`, SHA-256 hashed, shown once). `SOAR_BOOTSTRAP_API_KEY` installs a first admin key.
* **Roles:** `admin, engineer, analyst, viewer, service` with `resource:action` permissions (`GET /roles`);
  Keycloak realm roles map directly. Approvers: analysts and above, or named users.
* **Secrets:** AES-256-GCM envelope encryption per secret, master key id for rotation (`soar rotate-secrets`),
  values write-only through `PUT /secrets/{name}`.
* **Audit** (`internal/audit`): every successful API mutation with the real actor, and every secret the worker
  decrypts for a step plus every completed write or destructive action, as actor `workflow:<name>` (actor_type
  `workflow`, request_id = the step's idempotency key). `GET /audit`.
* **Secrets never leave the worker:** values a step used are replaced with `***` in its output, error and log lines
  (also their base64 form, e.g. an echoed Basic authorization header).
* **Where secrets may come from:** `env:NAME` references reach only variables meant as integration secrets
  (`SOAR_SECRET_*` anywhere; `SOAR_WAZUH_INDEXER_*` for connections; `SOAR_SOURCE_TOKEN_*`/`SOAR_SOURCE_SECRET_*`
  for ingest auth), checked when a connection or source is saved and again at runtime, so a connection can never
  read `SOAR_MASTER_KEY` or a platform password. A stored secret's `scope` limits who may use it: `host:<name>` (only
  connections whose URL is on that host; use it for credentials that matter), `connection:<name>`, a connector id,
  `workflows`, `ingest` or `source:<name>`; scoped secrets resolve only for a named use (worker, collector, ingest).
  Unscoped secrets can be bound by anyone allowed to create connections. Workflow parameters use stored secrets only.
* **Worker to runner:** the channel carries decrypted secrets, so it is TLS (mTLS with `SOAR_RUNNER_CERT_FILE`)
  unless the runner is on the same host or `SOAR_RUNNER_INSECURE=true` says the network is private (the dev stack).
* **Bootstrap key:** created once; restarting the api never re-enables a disabled bootstrap key or resets its roles.
* **Approvers** match by role, principal id (`oidc:<sub>`, `prn_...`) or OIDC user name (`alice`, `user:alice`);
  API keys never match by name, and an OIDC email counts as a name only when verified.
* **Limits:** request bodies 1 MiB (evidence 25 MB), JSON nesting 64 levels (API and ingest), CEL cost limits.
* **TLS / mTLS** (all optional): `SOAR_OS_CA_FILE`, `SOAR_OS_CERT_FILE` + `SOAR_OS_KEY_FILE` (OpenSearch),
  `SOAR_NATS_CA_FILE`, `SOAR_NATS_CERT_FILE` + `SOAR_NATS_KEY_FILE` (NATS), `SOAR_TLS_CERT_FILE` + `SOAR_TLS_KEY_FILE`
  (serve the role's port over HTTPS) and `SOAR_TLS_CLIENT_CA_FILE` (require client certificates, except on
  `/healthz` and `/readyz`). Bad files stop the process at start. `soar healthcheck` follows the role's TLS
  setting and pins its own certificate. `SOAR_OS_INSECURE=true` skips verification (development only).
* **Security suite** (Step 9 gate): `make test-security` (authorization matrix over every route and role, API keys,
  write-only secrets, audit, scrubbing, SSRF, TLS/mTLS, size and depth limits, runner sandbox). CI also runs
  `govulncheck`, `pip-audit` (`make vulncheck`) and a Trivy scan of the images.
* **Policy** (`config/policy.yaml`, spec §20.1): destructive actions need approval unless an approval node
  directly precedes them; Outlook sends over 50 recipients need approval; CEL rules can allow or deny.

## Step 10: triggers and waits

Cron triggers (`{type: schedule, cron: "*/15 * * * *"}` or `@every 15m`, UTC; a missed slot runs once),
approval and wait nodes, durable timers, and the `graph_mail` pull adapter for Microsoft 365 mailboxes.
**Not built yet:** syslog, Kafka/AMQP, file/object and SQL adapters, and wait-for-event nodes.

## Step 11: cases and observables

`POST /alerts/{id}/case` (open a case from an alert, or link it to `case_id`), `POST|GET /cases/{id}/alerts`
(alerts are copied into `soar-case-alerts` so they outlive their own retention), `PATCH /cases/{id}/tasks/{task_id}`,
`POST /cases/{id}/evidence?filename=` (raw body up to 25 MB into `soar-evidence`, SHA-256 recorded) and
`GET /cases/{id}/evidence/{eid}` (presigned link, 5 minutes). Closing a case starts its 3-day retention clock.

## Step 12: web UI

`web/` (React 18, TypeScript, Vite, React Flow, TanStack Query, Zustand, Tailwind with shadcn-style components,
Monaco, client types generated from `api/openapi.yaml`). Pages: dashboard (`GET /dashboard`), alerts, cases,
workflow list and editor (canvas and YAML edit the same definition; the server validates on every change),
executions with a live canvas, approvals, sources (test with a sample payload), connections and secrets, admin
(principals, roles, audit, queues/DLQ). Live updates through `GET /stream` (server-sent events).
`make web-dev` runs it on :3000 against a local API; `make web-test` type-checks and runs the unit tests.

## Step 13: integrations

`integrations/<name>/manifest.yaml` declares actions (and optional sources and mappings); actions are Python
modules run by the runner. Each package has a `<name>.connection.test` action, run by
`POST /connections/{id}/test` through the worker so secrets stay there.

* **DFIR-IRIS 2.4.27** (`dfir_iris`): cases, alerts, IOCs, assets, notes, timeline events, tasks, alert merge.
  Endpoint paths per API generation in `integrations/dfir_iris/actions/endpoints.yaml` (`legacy_2_4`); IOC
  types, TLP levels, asset types and users resolved by name. *Verify every endpoint against your instance.*
* **Microsoft 365 mail** (`outlook`): get, search (one or many mailboxes, per-run cap), get_attachment,
  mark_read, move, reply, send (at-most-once, batches, approval above 50 recipients), delete and purge
  (destructive, approval by default). `mailbox_scope` allow/deny lists on the connection. Certificate or
  client-secret credentials. Source `outlook-phish` (adapter `graph_mail`, disabled until configured) maps
  reported emails to alerts with sender, URL and IP observables. Phase 1 polls by `receivedDateTime` with
  overlap instead of delta links (the duplicate guard drops repeats); change notifications are not built.
* **Wazuh** (`wazuh`): agent get/list/restart and active response (endpoints *verify*).

## Step 14: observability

Every role serves Prometheus metrics at `/metrics` (spec §14 names plus queue fill, timer lag, outbox, policy
decisions). `deploy/prometheus/alerts.yml` encodes the alerts of spec §8.9. JSON logs carry
`request_id`, `execution_id`, `node_id`. OpenTelemetry tracing is not wired yet.

## Step 15: packaging

`deploy/prod/docker-compose.yml` (3-node NATS with replicas 3, required secrets, external OpenSearch, S3 and
Keycloak, runner egress through an allowlisting proxy), `scripts/backup.sh` / `scripts/restore.sh`
(OpenSearch snapshot, NATS stream backup, evidence mirror) and [docs/runbooks.md](docs/runbooks.md).
**Not built:** a Helm chart.
