# Runbooks

Operational procedures for the SOAR platform. Commands assume the repository root and the
development Compose file; for production use `-f deploy/prod/docker-compose.yml --env-file deploy/prod/.env`.

## Install (target: under 15 minutes)

1. Host: Docker CE with the compose plugin, `vm.max_map_count=262144` (`sudo ./scripts/rocky10-setup.sh` on Rocky Linux 10).
2. `cp deploy/compose/.env.example deploy/compose/.env` and replace every public default:
   `SOAR_MASTER_KEY` (`openssl rand -base64 32`), `SOAR_BOOTSTRAP_API_KEY` (`soar_` + 32 random characters),
   `S3_SECRET_KEY`, `KEYCLOAK_ADMIN_PASSWORD`, `SOAR_SOURCE_TOKEN_WAZUH_PROD`.
3. `make up` builds the three images (soar, soar-runner, soar-web), runs `migrate` (indices, ISM policies,
   object-storage buckets with lifecycle rules on the `s3` service) and `seed` (Wazuh and Outlook source packs),
   and waits for `/readyz`.
4. Sign in at http://localhost:3000 (Keycloak dev users: admin/admin, engineer/engineer, analyst/analyst,
   viewer/viewer), or with the bootstrap API key.
5. Create real admins in Keycloak (realm role `admin`), then remove `SOAR_BOOTSTRAP_API_KEY` and restart the api.

## Upgrade

1. `scripts/backup.sh` (below).
2. Pull the new version, `make image`.
3. `docker compose ... run --rm migrate`: additive mapping changes apply in place; a `v2` index needs the
   reindex procedure in the release notes. `make migrate-check` must report no drift.
4. Restart roles one at a time: `scheduler`, `processor`, `collector`, `orchestrator`, `worker`, `ingest`, `api`, `web`.
   Every role drains its NATS consumer on SIGTERM; in-flight steps are redelivered and answered from the
   idempotency store, so a rolling restart loses nothing.

## Backup and restore

* One-time: register an OpenSearch snapshot repository. On the dev `s3` service (or any S3 store) with the
  `repository-s3` plugin (create the `soar-backups` bucket first, e.g. in the console at http://localhost:9001):
  ```bash
  curl -XPUT localhost:9200/_snapshot/soar-backups -H 'Content-Type: application/json' \
    -d '{"type":"s3","settings":{"bucket":"soar-backups","endpoint":"s3:9000","protocol":"http","path_style_access":true}}'
  ```
  (or `"type":"fs"` with `path.repo` configured on every node).
* The evidence copy in the scripts uses rclone and needs `S3_URL` (dev: `http://127.0.0.1:9000`),
  `S3_ACCESS_KEY` and `S3_SECRET_KEY`; without `S3_URL` that part is skipped.
* `scripts/backup.sh [name]` snapshots every `soar-*` index, backs up the NATS streams (EVENTS, STEPS,
  RESULTS, DLQ) and mirrors the `soar-evidence` bucket into `./backups/<name>`.
* `scripts/restore.sh <name>` (roles stopped) restores all three; then `make migrate-check` and start the roles.
* Keep the master key (`SOAR_MASTER_KEY`) in a vault, outside these backups: secrets cannot be decrypted without it.

## Rotate the secret-encryption master key

1. Generate a new key: `openssl rand -base64 32`.
2. Set `SOAR_MASTER_KEY_ID=k2`, `SOAR_MASTER_KEY=<new>`, and `SOAR_OLD_MASTER_KEYS=k1:<old>` on every role; restart.
   New and updated secrets use k2; old ones still decrypt with k1.
3. Re-wrap every data key with k2: run `docker compose run --rm migrate rotate-secrets` (the soar image, any role definition) and
   check that `GET /api/v1/secrets` shows `key_id: k2` everywhere.
4. Remove `SOAR_OLD_MASTER_KEYS` and restart. Values are never re-encrypted, only their data keys.

## TLS and mTLS

1. Put the PEM files in `deploy/prod/certs/` (mounted read-only at `/etc/soar/certs`) and name them in `.env`
   (`deploy/prod/.env.example` lists the variables): `SOAR_OS_CA_FILE` for a private OpenSearch CA,
   `SOAR_OS_CERT_FILE`/`SOAR_OS_KEY_FILE` when OpenSearch requires client certificates, the `SOAR_NATS_*` equivalents
   with `tls://` NATS URLs, and `SOAR_TLS_CERT_FILE`/`SOAR_TLS_KEY_FILE` to serve the roles' ports over HTTPS.
2. `SOAR_TLS_CLIENT_CA_FILE` makes every role require a client certificate signed by that CA, except on `/healthz`
   and `/readyz` (container healthchecks). Prometheus then needs a client certificate to scrape `/metrics`, and the
   web container's nginx must proxy to `https://api:8080` with one.
3. Restart the roles. A missing, unreadable or mismatched file stops a role at start (exit 2, the log names the
   variable); the start log shows `os_tls`, `nats_tls` and `http_tls`.

## Dead letters (alert `DeadLetters`)

1. `GET /api/v1/dlq` (UI: Admin → Queues & DLQ) shows origin subject, reason and delivery count.
2. `dlq.events`: an event the processor could not store (mapping bug, strict-mapping rejection). Fix the
   mapping or the cause, then `POST /api/v1/dlq/{seq}/replay`.
3. `dlq.steps`: a step whose worker kept crashing. Its execution already received a `delivery_failed`
   failure and took its error path or failed. Fix the action, then retry the execution (`POST /executions/{id}/retry`).
4. `dlq.results`: an orchestrator message that could not be applied; the execution may stall until its
   deadline. Replay after fixing the cause; the engine ignores duplicates.

## Queue full (alerts `StreamAlmostFull`, `IngestRejecting`)

Ingest answers 503 with `Retry-After` once EVENTS reaches `max_bytes`; nothing accepted is lost.
1. `GET /api/v1/queues/stats`: which consumer is behind?
2. Processor behind: scale `processor` replicas / `SOAR_PROCESSOR_WORKERS`; check OpenSearch write rejections
   (`_cat/thread_pool/write`) and heap.
3. NATS disk slow: see README "Measured: what limits throughput" (`make nats-relaxed` trades a 200 ms
   durability window on a single node for throughput; production uses R3).

## OpenSearch outage

Roles stop acknowledging; ingest keeps accepting until the stream limit, then answers 503. After recovery
the backlog drains on its own. Verify with `GET /queues/stats` that EVENTS `pending` returns to near zero.

What happens meanwhile: the processor, worker and orchestrator *hold* what they already fetched and retry it in
place (backoff up to 15 s), sending `InProgress` so JetStream neither redelivers it nor counts a delivery; the
processor's other fetch loops pause. Nothing reaches the DLQ because of an outage, however long it lasts.
Signs: logs `OpenSearch is unavailable: holding ...` then `OpenSearch answers again ...` with `held_for`;
metrics `soar_store_unavailable{role="processor"}` (1 while paused) and `soar_store_unavailable_holds_total{role}`.
Only connection errors and HTTP 429/502/503/504 count as an outage; other errors still go the normal
nak/DLQ way, so a genuinely broken message cannot stall a role.

## NATS node failure (production R3)

A 3-node cluster keeps quorum with one node down: publishing continues. Replace the node and restart it;
JetStream re-replicates. With two nodes down, publishing stops (ingest 503) until quorum returns.

## Stuck executions (alerts `OutboxStuck`, `TimerLag`)

* `OutboxStuck`: the scheduler's outbox sweeper republishes unconfirmed commands every 5 s. Check that
  exactly one scheduler leads (`GET :8086/stats` → `leading: true` on one replica) and that NATS is healthy.
* `TimerLag`: the leader scheduler fires due timers every second. Restart the scheduler if it lost its lease.
* An execution waiting on an approval appears under Approvals; a decision resumes it.

## Scaling for 1,000 alerts/s

Start with 2-4 processors, 8 orchestrator partitions (`SOAR_PARTITIONS`), 2+ workers with
`SOAR_WORKER_CONCURRENCY` sized to CPU, and dedicated `critical` workers (`SOAR_WORKER_PRIORITIES=critical`).
Validate with `make loadtest RATE=1000 DURATION=30m` and `make loadtest-burst`.
