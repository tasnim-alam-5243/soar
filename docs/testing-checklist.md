# SOAR test checklist

What to test for each build step, with the exact commands and what a pass looks like. Steps 1-7 are
detailed; Steps 8-15 have their code in the tree but are not the current focus, so they get a shorter list.

How to use it:

* Work top to bottom inside a step: automated tests first, then the live checks against the running stack.
* **Expect:** is the pass condition. Tick the box when it holds; write anything odd in the results log at the end.
* Commands run from the repo root (`/root/soar/WFsoar/soar`) unless they say otherwise.
* Container names follow Compose: `soar-<service>-1` (for example `soar-processor-1`).

---

## 0. Before you start

- [ ] **Rebuild** after every code update: `make up` rebuilds the images (Go 1.26 builder) and runs `migrate` (additive index
  changes, for example the `applied` flag of `soar-seen`) and `seed`; the runner image carries the integrations.
  ```bash
  cp -n deploy/compose/.env.example deploy/compose/.env   # only if you have no .env yet
  make up                                                 # builds soar, soar-runner, soar-web; waits for /readyz
  make ps
  ```
  **Expect:** every service `healthy` (or `running`); `migrate` and `seed` `exited (0)`.
- [ ] **Object storage.** MinIO no longer publishes images, so the `s3` service runs RustFS (S3-compatible) on 9000/9001.
  ```bash
  curl -s -o /dev/null -w '%{http_code}\n' localhost:9000/health/ready
  docker compose -f deploy/compose/docker-compose.yml logs migrate | grep -E 'bucket ok|migrate finished'
  ```
  **Expect:** 200; four `bucket ok` lines (`soar-raw`, `soar-payloads`, `soar-exports` with `expire_days`, `soar-evidence`
  with 0) and `migrate finished` with `failed: 0` (indices whose schema changed show `updated`, the rest `unchanged`).
  Console: http://localhost:9001, keys `soar-s3` / `soar-s3-dev-password` (or `S3_ACCESS_KEY` / `S3_SECRET_KEY` from `.env`).
- [ ] **Tools:** `jq`, Go, and the Python test venv: `make runner-venv` (needed for Step 7 and the YAML helper below).
  For Step 12 also `make web-install`.
- [ ] **Shell helpers.** Paste into the terminal you test from (values are the `.env` development defaults; use yours if you changed them):
  ```bash
  export SOAR_KEY=soar_dev-bootstrap-key-change-me-0123456789abcdef   # SOAR_BOOTSTRAP_API_KEY
  export TOKEN=dev-wazuh-token-change-me                              # SOAR_SOURCE_TOKEN_WAZUH_PROD
  export API=http://localhost:8080/api/v1
  # api METHOD PATH [curl args...]      e.g.  api GET /me | jq .
  api()  { curl -s -H "Authorization: Bearer $SOAR_KEY" -H 'Content-Type: application/json' -X "$1" "$API$2" "${@:3}"; }
  # push [curl args...]                 e.g.  push -d @file.json
  push() { curl -s -w ' HTTP %{http_code}\n' -XPOST localhost:8081/ingest/wazuh-prod -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }
  # osq PATH [curl args...]             OpenSearch (dev: no auth)
  osq()  { curl -s -H 'Content-Type: application/json' "localhost:9200$1" "${@:2}"; }
  # done_exe EXECUTION_ID               waits until the execution leaves queued/running, prints its status
  done_exe() { for i in $(seq 1 90); do s=$(api GET /executions/$1 | jq -r .status); case $s in queued|running) sleep 2;; *) echo "$s"; return;; esac; done; echo "still $s"; }
  ```
- [ ] `api GET /me | jq .` **Expect:** your principal with role `admin`.

Reset everything (deletes all data) only when you want a clean slate: `make clean && make up`.

---

## Step 1 - Foundation

Spec gate: `make up` healthy; CI green.

Automated
- [ ] `make test` and `make vet` pass.
- [ ] `make tools && make lint` passes (golangci-lint).
- [ ] CI (GitHub Actions) is green after you push (lint, unit, integration, image build).

Live
- [ ] `curl -s localhost:8080/readyz | jq .`
  **Expect:** HTTP 200, `{"checks":{"nats":"ok","opensearch":"ok"},"status":"ok"}`. Same for `localhost:8081/readyz` (ingest).
- [ ] `curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/healthz` **Expect:** 200.
- [ ] Dependency loss is visible:
  ```bash
  docker stop soar-nats-1; sleep 5; curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/readyz
  docker start soar-nats-1; sleep 20; curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/readyz
  ```
  **Expect:** 503 while NATS is down, 200 again after it is back (all roles reconnect by themselves).
- [ ] Bad configuration refuses to start:
  ```bash
  docker compose -f deploy/compose/docker-compose.yml run --rm -e SOAR_PARTITIONS=0 api; echo "exit $?"
  ```
  **Expect:** exit 2 and a log line naming the bad variable.
- [ ] Logs are JSON with correlation IDs: `make logs S=api` **Expect:** lines with `"request_id"`, `"role":"api"`.

---

## Step 2 - Indices, retention, migrations

Spec gate: repeatable migrations; strict mappings.

Automated
- [ ] `make test` passes (schema tests).

Live
- [ ] `make migrate` twice. **Expect:** the second run logs `created 0, updated 0, failed 0`.
- [ ] `make migrate-check; echo $?` **Expect:** 0 (no drift).
- [ ] Strict mapping rejects unknown fields:
  ```bash
  osq /soar-workflows/_doc/t -XPUT -d '{"x":1}' | jq -r .error.type
  ```
  **Expect:** `strict_dynamic_mapping_exception`.
- [ ] Retention policies exist: `osq '/_plugins/_ism/policies' | jq -r '.policies[].policy.policy_id' | grep soar`
  **Expect:** `soar-alerts-retention`, `soar-audit-retention`, `soar-raw-retention`, `soar-steprun-retention`, `soar-timeline-retention`.
- [ ] Rollover aliases: `osq '/_cat/aliases/soar-alerts?v'` **Expect:** exactly one index with `is_write_index true`.
- [ ] (Optional) Retention is configuration: change `alerts: 7d` to `8d` in `config/retention.yaml`, `make migrate`,
  then `osq /_plugins/_ism/policies/soar-alerts-retention | jq '.policy.states[0].transitions'` shows `8d`. Change it back and migrate again.

---

## Steps 3-4 - Storage layer and REST API

Spec gates: integration tests against real OpenSearch; API contract tests.

Automated
- [ ] `make test` passes (contract tests, shared store suite on the in-memory store).
- [ ] `make test-integration` passes. It covers Steps 2-7 against the live stack (store suite on real OpenSearch,
  bulk writes, API, ingestion, an alert-triggered workflow run). Defaults match `make up`; override with
  `SOAR_API_KEY`, `SOAR_INDEX_PREFIX`, `SOAR_INGEST_URL`, `SOAR_API_URL`, `SOAR_IT_HTTP_URL`.

Live
- [ ] Create, read, list:
  ```bash
  CASE=$(api POST /cases -d '{"title":"Checklist case","severity":3}' | jq -r .id); echo $CASE
  api GET /cases/$CASE | jq '{id,title,status,severity}'
  api GET '/cases?status=open&limit=1' | jq '{n: (.items|length), next_cursor}'
  ```
  **Expect:** an id starting `cas_`; the list has 1 item and a `next_cursor` when there are more.
- [ ] Optimistic concurrency end to end:
  ```bash
  ETAG=$(curl -s -D - -o /dev/null -H "Authorization: Bearer $SOAR_KEY" $API/cases/$CASE | grep -i '^etag' | cut -d' ' -f2 | tr -d '\r')
  api PATCH /cases/$CASE -H "If-Match: $ETAG" -d '{"summary":"first"}'  -o /dev/null -w '%{http_code}\n'
  api PATCH /cases/$CASE -H "If-Match: $ETAG" -d '{"summary":"second"}' -o /dev/null -w '%{http_code}\n'
  ```
  **Expect:** 200, then 409 (the second writer holds a stale ETag).
- [ ] Idempotent create:
  ```bash
  api POST /cases -H 'Idempotency-Key: chk-1' -d '{"title":"Idem","severity":2}' -D - | grep -i -E '^HTTP|idempotent'
  api POST /cases -H 'Idempotency-Key: chk-1' -d '{"title":"Idem","severity":2}' -D - | grep -i -E '^HTTP|idempotent'
  ```
  **Expect:** both 201, same id; the second has `Idempotent-Replayed: true`.
- [ ] Error model:
  ```bash
  api GET /cases/cas_nope -D - | grep -i content-type        # application/problem+json, 404, request_id in body
  api POST /cases -d '{"severity":9}' | jq '.status, [.errors[].path]'   # 422 and every problem listed
  curl -s -o /dev/null -w '%{http_code}\n' $API/cases          # 401 without a key
  ```
- [ ] Contract is served: `curl -s $API/openapi.yaml | head -3` **Expect:** the OpenAPI header.

---

## Step 5 - Ingestion (queue first) and Wazuh push + pull

Spec gate: 1,000/s for 30 min and 2,000/s burst for 60 s, zero loss, p99 < 100 ms; queue tests of spec 8.10.

### 5.0 The fixes from 2026-11-11 are in the rebuilt stack

- [ ] No more source-loading errors (these repeated every 10 s before):
  ```bash
  for c in ingest collector; do echo "$c: $(docker logs soar-$c-1 2>&1 | grep -c 'source cannot be loaded')"; done
  ```
  **Expect:** `0` for both (a pull source with no indexer configured simply stays disabled).
- [ ] TTL purge no longer times out: after 10+ minutes, `docker logs soar-scheduler-1 2>&1 | grep -c 'ttl purge failed'` **Expect:** 0.
- [ ] Update the pull source to the new paging config. `make seed` never overwrites an existing source, so the
  `wazuh-pull` you already have keeps the old config (it works, less efficiently):
  ```bash
  PULL=$(api GET '/sources?adapter=poll_http' | jq -r '.items[] | select(.name=="wazuh-pull") | .id')
  runner/python/.venv/bin/python - integrations/wazuh/sources/wazuh-pull.yaml > /tmp/pull-config.json <<'EOF'
  import json, sys, yaml
  print(json.dumps({"config": yaml.safe_load(open(sys.argv[1]))["config"]}))
  EOF
  api PATCH /sources/$PULL --data-binary @/tmp/pull-config.json | jq '.config | {key_path, overlap_pages}'
  ```
  **Expect:** `{"key_path":"sort","overlap_pages":40}`.

### 5.1 Push (Wazuh Integrator path)

- [ ] Golden fixtures map as specified: `go test ./integrations -run Wazuh -v` **Expect:** pass (5 manager alerts + 1 indexer hit).
- [ ] Ingest the five manager samples:
  ```bash
  for f in $(ls integrations/wazuh/testdata/0[1-5]_*.json | grep -v expected); do
    jq --arg s "$(date +%s%N)" '.id = .id + "." + $s' $f | push -d @-; done
  sleep 6; api GET '/alerts?source_id=wazuh-prod&limit=10' | jq '.items[] | {title, severity, original_severity, n: (.observables|length)}'
  ```
  **Expect:** five `HTTP 202` with `event_ids`; level 3 -> severity 1 and level 5 -> severity 2 (spec 10.1 table;
  level 7 -> 3 shows up in 5.2); the FIM sample with empty hashes (`npx-22`) has no hash observables.
- [ ] Batch endpoint (one request, several records):
  ```bash
  jq -s --arg s "$RANDOM" 'map(.id = .id + ".b" + $s)' $(ls integrations/wazuh/testdata/0[3-5]_*.json | grep -v expected) | push -d @-
  ```
  **Expect:** one 202 with 3 `event_ids`.
- [ ] Rejections: wrong token -> 401; `/ingest/nosuch` -> 404; body over 1 MB -> 413; more than 500 records -> 422.
- [ ] Exact duplicate is stored once: push the same file twice without changing `id`. **Expect:** the second answer is
  still 202, but there is one alert with `occurrence_count` 1.
- [ ] Grouping (same rule/agent/srcip/dstuser, different `id`s, within 10 minutes):
  ```bash
  G=grp$RANDOM; for i in 1 2 3; do jq --arg g $G --arg s "$(date +%s%N)" '.id = $g + "." + $s | .agent.id = $g | .agent.name = $g' \
    integrations/wazuh/testdata/05_sshd_auth_success.json | push -d @-; done
  sleep 6; osq /soar-alerts/_search -d "{\"query\":{\"term\":{\"entities.hosts\":\"$G\"}}}" | jq '[.hits.total.value, .hits.hits[0]._source.occurrence_count]'
  ```
  **Expect:** `[1, 3]`: one alert document counting three occurrences.
- [ ] Unmappable record is not dropped: `push -d '{"id":"bad-1","rule":{"id":"1","level":5,"description":"no timestamp"}}'`
  **Expect:** 202 with `failed: 1`; `api GET '/dlq?limit=50' | jq '.items | length, .[0]'` shows it with the failing
  expression; `api POST /dlq/<id>/replay` (the id is the DLQ sequence number) republishes it.
- [ ] Dry runs publish nothing:
  ```bash
  PUSH=$(api GET '/sources?adapter=webhook' | jq -r '.items[] | select(.name=="wazuh-prod") | .id')
  api POST /sources/$PUSH/test -d "{\"payload\": $(cat integrations/wazuh/testdata/05_sshd_auth_success.json)}" | jq .
  api POST /mappings/test -d "{\"mapping\":\"wazuh_alert@1\",\"sample\": $(cat integrations/wazuh/testdata/05_sshd_auth_success.json)}" | jq .
  ```
  **Expect:** the normalized event; no new alert appears.
- [ ] Filters change without a deploy (fixed: the filter test now sees an edited list at once):
  ```bash
  api PUT /lists/wazuh_noisy_rule_ids -d '{"items":["5715"]}' | jq .items
  api POST /filters/test -d "{\"filter_set\":\"wazuh-default@1\",\"sample\": $(cat integrations/wazuh/testdata/05_sshd_auth_success.json)}" | jq '{drop, rule}'
  sleep 12; jq '.id = "noisy.'$RANDOM'"' integrations/wazuh/testdata/05_sshd_auth_success.json | push -d @-
  api PUT /lists/wazuh_noisy_rule_ids -d '{"items":[]}' > /dev/null      # undo
  ```
  **Expect:** `{"drop":true,"rule":"drop-noisy-rule-ids"}` immediately; the push 10 s later answers `filtered: 1`.
- [ ] Health and queue: `api GET /sources/$PUSH/health | jq .` (last event time, rate) and `api GET /queues/stats | jq .` (EVENTS pending near 0).

### 5.2 Pull (Wazuh indexer path)

Use a real indexer (set `SOAR_WAZUH_INDEXER_URL`, `_USER`, `_PASSWORD`, `_CA_FILE` in `deploy/compose/.env`), or
fake one inside the dev OpenSearch as below. Fake setup, once:

```bash
cat >> deploy/compose/.env <<'EOF'
SOAR_WAZUH_INDEXER_URL=http://opensearch:9200
SOAR_WAZUH_INDEXER_USER=reader
SOAR_WAZUH_INDEXER_PASSWORD=unused-dev-opensearch-has-no-auth
EOF
make seed && make up          # seed creates the wazuh_indexer connection and enables wazuh-pull
IDX=wazuh-alerts-4.x-$(date -u +%Y.%m.%d)   # daily index named by the UTC date
osq /$IDX -XPUT -d '{"settings":{"number_of_replicas":0},"mappings":{"date_detection":false,"properties":{"@timestamp":{"type":"date"},"id":{"type":"keyword"},"timestamp":{"type":"keyword"}}}}'
# gen N PREFIX SPAN_SECONDS: N indexer docs from the fixture, spread over the last SPAN seconds, unique ids and paths
gen() { jq -c --arg idx "$IDX" --argjson now "$(date -u +%s)" --argjson n "$1" --arg p "$2" --argjson span "$3" \
  '._source as $s | range(0;$n) as $i | ($p + "." + ($i|tostring)) as $id | {index:{_index:$idx,_id:$id}},
   ($s + {id:$id, "@timestamp":(($now - $span + ($i * $span / $n | floor))|todate), syscheck:($s.syscheck + {path:("/etc/pulltest/"+$id)})})' \
  integrations/wazuh/testdata/06_indexer_hit_fim_modified.json; }
pulled() { osq /soar-alerts/_count -d "{\"query\":{\"prefix\":{\"external_id\":\"$1.\"}}}" | jq .count; }
```

- [ ] Basic pull: `gen 200 p1 600 | osq '/_bulk?refresh=true' -XPOST --data-binary @- | jq .errors`, wait 30 s, `pulled p1`.
  **Expect:** `false`, then `200`.
- [ ] Cursor is a keyset position and only moves forward:
  `osq /soar-cursors/_search -d '{"query":{"term":{"source_id":"wazuh-pull"}}}' | jq -r '.hits.hits[0]._source.cursor'`
  **Expect:** `{"ts":"...","after":[<millis>,"p1.199"]}`; run it again after the next tests, `ts` never goes back.
- [ ] Late arrival (a record indexed after the poller passed its timestamp). Run this before the burst below: a
  25,000-record burst packs the 60 s window denser than the sweep budget.
  ```bash
  LATE=$(osq /soar-cursors/_search -d '{"query":{"term":{"source_id":"wazuh-pull"}}}' \
    | jq -r '.hits.hits[0]._source.cursor | fromjson | (.ts | sub("\\.[0-9]+";"") | fromdateiso8601) - 20 | todate')
  jq -c --arg ts "$LATE" '._source | .id="late.1" | .["@timestamp"]=$ts' integrations/wazuh/testdata/06_indexer_hit_fim_modified.json \
    | osq /$IDX/_doc/late.1?refresh=true -XPUT --data-binary @- | jq -r .result
  sleep 35; pulled late
  ```
  **Expect:** `created`, then `1` (picked up by the 60 s overlap sweep).
- [ ] Burst catch-up: `gen 25000 p2 30 | osq '/_bulk?refresh=true' -XPOST --data-binary @- | jq .errors`, then `pulled p2` every few seconds.
  **Expect:** 25000 within about a minute (several back-to-back cycles of 10,000); the collector log
  (`docker logs soar-collector-1`) has no `a full page did not advance the cursor`.
- [ ] Push and pull of the same alert are stored once:
  `jq -c '._source | .id="p1.5"' integrations/wazuh/testdata/06_indexer_hit_fim_modified.json | push -d @-`, then
  `osq /soar-alerts/_search -d '{"query":{"term":{"external_id":"p1.5"}}}' | jq '[.hits.total.value, .hits.hits[0]._source.occurrence_count]'`
  **Expect:** `[1, 1]`.
- [ ] Pull health: `api GET /sources/$PULL/health | jq .` **Expect:** recent `last_event_at`, `status: ok`.
- [ ] Cleanup of the fake: `osq /$IDX -XDELETE`, remove the three lines from `.env`, then disable the source:
  `api PATCH /sources/$PULL -d '{"enabled":false}'`.

### 5.3 Load and queue gates (spec 3.3 and 8.10)

Run these on the machine you will size from; they take time.

- [ ] Peak: `make loadtest RATE=1000 DURATION=30m` **Expect:** `RESULT: PASS` (no 5xx, p99 < 100 ms, zero loss, each event guarded once).
- [ ] Burst: `make loadtest-burst` **Expect:** PASS and "queue drained" well under 5 minutes.
  If throughput stops near 1,100/s, read "Measured: what limits throughput" in the README (`make nats-relaxed`).
- [ ] Processor killed mid-batch: start `make loadtest RATE=1000 DURATION=3m`; after 60 s run `docker restart -t 0 soar-processor-1`.
  **Expect:** PASS (redelivered messages are harmless, nothing lost or counted twice).
- [ ] OpenSearch outage (fixed 2026-11-11). During a 5-minute load run, `docker stop soar-opensearch-1` for 60 s,
  then `docker start soar-opensearch-1`.
  **Expect:** ingest keeps answering 202 (the queue absorbs it); `docker logs soar-processor-1` shows
  `OpenSearch is unavailable: holding in-flight events and pausing fetches`, then `OpenSearch answers again: resuming`;
  loadgen PASS (zero loss, every event guarded once); `GET /dlq` gains nothing.
  Before the fix the processor NAK-ed each failed batch every 2 s and ~10 s of outage sent events to the DLQ.
  (Verified on 2026-11-11 against real OpenSearch with a ~40 s cut-off during 1,000/s: PASS, 0 redeliveries.)
- [ ] NATS outage: `docker stop soar-nats-1`, then `push -d @integrations/wazuh/testdata/05_sshd_auth_success.json`.
  **Expect:** 503 within a few seconds (never a silent 202). `docker start soar-nats-1` and the next push gets 202.
- [ ] Poison message reaches the DLQ and does not block the queue: the unmappable record of 5.1 is in `GET /dlq`,
  and pushes right after it are processed normally.

---

## Step 6 - Engine (DSL, validator, CEL, state machine)

Spec gate: deterministic + property tests pass.

Automated
- [ ] `go test -v ./internal/engine` **Expect:** all pass, including `TestPropertyRandomWorkflows` (600 random
  workflows, shuffled and duplicated events: termination, single dispatch, deterministic replay, legal transitions),
  `TestStateMachineTable` and `FuzzCompile`.
- [ ] `go test ./integrations -run Example` **Expect:** both example playbooks compile against the real action registry.

Live: validation at save time (`POST /workflows/validate` answers `{"valid": ..., "errors": [...]}`)
- [ ] Each of these returns `valid: false` with a path-addressed error:
  ```bash
  v() { api POST /workflows/validate -d "{\"definition\": $1}" | jq -c '{valid, errors: [.errors[] | .path + ": " + .message]}'; }
  v '{"nodes":[{"id":"a","type":"set","vars":{"x":"1"}},{"id":"b","type":"set","vars":{"y":"2"}}],"edges":["a -> b","b -> a"]}'  # cycle
  v '{"nodes":[{"id":"a","action":"nope.nothing"}]}'                                       # unknown action
  v '{"nodes":[{"id":"a","type":"set","when":"1 +","vars":{"x":"1"}}]}'                    # condition (when) does not compile
  v '{"nodes":[{"id":"a","action":"http.request"}]}'                                       # required param url missing
  v '{"nodes":[{"id":"a","type":"agent"}]}'                                                # agent node: not enabled
  v '{"nodes":[{"id":"a","type":"set","vars":{"x":"1"}},{"id":"a","type":"set","vars":{"y":"1"}}]}'   # duplicate id
  ```
  Note: only conditions (`when`, branch `cases`, `until`, foreach `items`, `inputs.from`) must be valid CEL. Values
  (params, `set` vars, messages) that do not compile are kept as literal text by design (docs/workflow-dsl.md).
- [ ] A valid one: `v '{"nodes":[{"id":"a","type":"set","vars":{"x":"1"}}]}'` **Expect:** `{"valid":true,"errors":[]}`.

Live: running workflows. Helper to create, publish and run (prints the execution id):
```bash
# run_wf NAME DEFINITION_JSON [RUN_BODY]
run_wf() { local id body=${3:-'{}'}; id=$(api POST /workflows -d "{\"name\":\"$1-$RANDOM\",\"definition\":$2}" | jq -r .id)
  api POST /workflows/$id/publish > /dev/null; api POST /workflows/$id/run -d "$body" | jq -r .id; }
```
- [ ] Branch + set (the API container is reachable from the worker by name):
  ```bash
  E=$(run_wf t6-branch '{"nodes":[
    {"id":"check","action":"http.request","params":{"method":"GET","url":"\"http://api:8080/healthz\""}},
    {"id":"decide","type":"branch","cases":[{"when":"steps.check.output.status == 200","next":["ok"]}],"default":["bad"]},
    {"id":"ok","type":"set","vars":{"result":"\"healthy\""}},
    {"id":"bad","type":"set","vars":{"result":"\"unhealthy\""}}],"edges":["check -> decide"]}')
  done_exe $E; api GET /executions/$E | jq -c '{vars: .context.vars, ok: .context.nodes.ok.status, bad: .context.nodes.bad.status}'
  ```
  **Expect:** `succeeded`; `{"vars":{"result":"healthy"},"ok":"succeeded","bad":"skipped"}`.
- [ ] Retries with backoff, then the error path:
  ```bash
  E=$(run_wf t6-retry '{"nodes":[
    {"id":"call","action":"http.request","params":{"method":"GET","url":"\"http://10.255.255.1/\""},"timeout":"2s","retry":{"max_attempts":3,"initial":"1s","max":"2s"},"on_error":["handle"]},
    {"id":"handle","type":"set","vars":{"failed":"true"}}]}')
  done_exe $E; sleep 6; api GET /executions/$E/steps | jq -c '[.items[] | {node_id, attempt, status, code: .error.code}]'
  ```
  **Expect:** `succeeded` (handled by `on_error`); three `call` step runs, attempts 1-3, `timeout` or `network`.
- [ ] Foreach with a concurrency cap: `E=$(run_wf t6-each '{"nodes":[{"id":"each","type":"foreach","items":"[1,2,3,4]","action":"delay.sleep","params":{"seconds":"item"},"concurrency":2}]}')`
  **Expect:** `succeeded` after about 6 s (2 at a time), 4 step runs.
- [ ] Wait, then cancel: `E=$(run_wf t6-wait '{"nodes":[{"id":"pause","type":"wait","duration":"5m"}]}')`, check
  `api GET /executions/$E | jq -r .status` is `waiting`, then `api POST /executions/$E/cancel`. **Expect:** `cancelled`.
- [ ] Dry run: `run_wf t6-dry '{"nodes":[{"id":"call","action":"http.request","params":{"method":"POST","url":"\"https://example.com/\""}}]}' '{"dry_run":true}'`
  **Expect:** `succeeded`; the step output has `dry_run: true` and `would_send`; nothing was sent.
- [ ] Alert trigger end to end (the Step 6 + 7 path):
  ```bash
  runner/python/.venv/bin/python - examples/workflows/wazuh_ssh_root_login.yaml > /tmp/wf.json <<'EOF'
  import json, sys, yaml
  print(json.dumps({"name": "wazuh-root-login", "definition": yaml.safe_load(open(sys.argv[1]))}))
  EOF
  WF=$(api POST /workflows --data-binary @/tmp/wf.json | jq -r .id); api GET /workflows/$WF | jq -c '.triggers'
  api POST /workflows/$WF/publish | jq -r .status
  sleep 12      # the processor reloads triggers every 10 s
  jq --arg s "$(date +%s%N)" '.id = "root." + $s' integrations/wazuh/testdata/05_sshd_auth_success.json | push -d @-
  sleep 8; E=$(api GET "/executions?workflow_id=$WF&limit=1" | jq -r '.items[0].id'); api GET /executions/$E | jq -r .status
  ```
  **Expect:** the example's trigger (written inside its definition) is listed under the workflow's top-level
  `triggers`, then `published`, then `waiting` (case opened, IP recorded, approval pending). Approve it:
  `APR=$(api GET "/approvals?execution_id=$E&status=pending" | jq -r '.items[0].id'); api POST /approvals/$APR/decision -d '{"approved":true,"comment":"checklist"}'`,
  then `done_exe $E` **Expect:** `succeeded`; `block` failed with `invalid_connection` (no Wazuh manager connection) and
  `note_failed` wrote that onto the case (`api GET /cases?limit=1 | jq '.items[0].summary'`).
- [ ] Triggers can be written in either place (fixed 2026-11-11; before, triggers inside `definition` never fired):
  the import above sends the example's triggers inside `definition`. Also send one at top level and one inside
  the definition in the same request. **Expect:** the stored workflow has both under `triggers` (no duplicates) and
  none left inside `definition`; a cron trigger inside the definition (`{"type":"schedule","cron":"@every 1m"}`) runs
  once a minute after publishing.
- [ ] No illegal state transitions were refused: `docker logs soar-orchestrator-1 2>&1 | grep -c 'illegal state transition'` **Expect:** 0.

---

## Step 7 - Worker, built-in actions, Python runner, sandbox

Spec gate: limits enforced.

Automated
- [ ] `go test -v ./internal/actions ./internal/runner` **Expect:** pass (`TestSSRFGuard`, `TestExecuteThroughRealRunner` against the real Python runner).
- [ ] `make test-python` **Expect:** 45 passed: timeout kill, CPU budget, memory limit, secret scrubbing, empty environment, path-escape refusal, gRPC round trip, and every integration module importing with its manifest entrypoints present.

Live
- [ ] Action catalog: `api GET /connectors | jq -r '.items[].id'` **Expect:** built-ins plus `dfir_iris`, `outlook`, `wazuh`.
  `api GET /connectors/dfir_iris/actions | jq '.items[0]'` shows `description`, `side_effect`, `risk`, `supports_dry_run`, schemas.
- [ ] SSRF guard (not retried):
  ```bash
  E=$(run_wf t7-ssrf '{"nodes":[{"id":"meta","action":"http.request","params":{"method":"GET","url":"\"http://169.254.169.254/latest/meta-data/\""}}]}')
  done_exe $E; sleep 6; api GET /executions/$E/steps | jq -c '[.items[] | {attempt, code: .error.code}]'
  ```
  **Expect:** `failed`; one step run with `ssrf_blocked`. Same with `http://127.0.0.1:8080/healthz` (loopback).
- [ ] Python action through the runner, with a stored secret:
  ```bash
  api PUT /secrets/iris_test_key -d '{"value":"not-a-real-key-4711"}' -o /dev/null -w '%{http_code}\n'
  CON=$(api POST /connections -d '{"connector_id":"dfir_iris","name":"iris-test","auth_type":"bearer","secret_refs":{"api_key":"iris_test_key"},"config":{"base_url":"https://iris.invalid"}}' | jq -r .id)
  api POST /connections/$CON/test | jq .
  ```
  **Expect:** `ok: false`, a network/DNS error from the Python action (the call went worker -> runner -> sandbox).
- [ ] Secrets never leak:
  ```bash
  api GET /secrets | jq '.items[] | select(.name=="iris_test_key")'          # metadata only, no value
  for c in worker runner api; do echo "$c: $(docker logs soar-$c-1 2>&1 | grep -c not-a-real-key-4711)"; done
  ```
  **Expect:** no `value` field; `0` for every container.
- [ ] Runner container hardening:
  `docker inspect soar-runner-1 --format '{{.Config.User}} ro={{.HostConfig.ReadonlyRootfs}} caps={{.HostConfig.CapDrop}}'`
  **Expect:** `10001:10001 ro=true caps=[ALL]`.
- [ ] A Python action without a connection fails fast and is not retried: in the Step 6 end-to-end run,
  `api GET /executions/$E/steps | jq -c '[.items[] | select(.node_id=="block") | {attempt, code: .error.code}]'`
  **Expect:** `[{"attempt":1,"code":"invalid_connection"}]` even though the node allows 3 attempts.
- [ ] Clean up: `api DELETE /connections/$CON`, `api DELETE /secrets/iris_test_key`.

CPU, memory and timeout limits are covered by `make test-python` (a busy loop with `cpu: 0.1` and a 20 s timeout is
stopped as `cpu_limit` after about 2 CPU seconds). To see one live, an action package with a busy loop must be
installed in the runner image.

---

## Steps 8-15 (Steps 8 and 9 verified 2026-11-11; 10-15 have code in the tree, not yet verified live)

### Step 8 - Reliability
Gate: chaos (NATS node kill, processor/worker kill mid-batch): no lost events, no double side effects.
Already run on 2026-11-11 against an isolated stack with real `kill -9` (README "Step 8" has the table): two processor
kills during 60,000 events at 1,000/s (zero loss, exact counts, one execution per alert), 12 worker + 5 orchestrator
kills during 60 executions with side effects (all succeeded, one case each), a NATS kill (503 during the outage,
every accepted event stored once). The checks below repeat it on your stack.
- [ ] `go test -v ./internal/e2e -run Flow` passes (retries, error paths, redelivered step runs its side effect once, outbox sweeper, poison step to DLQ, cancel).
- [ ] `go test -v ./internal/processor -run Redelivery` passes (a crash before or after the main write never loses or double-counts an event).
- [ ] Processor killed mid-batch: `make loadtest RATE=1000 DURATION=60s`, and 20 s in `docker kill soar-processor-1` (it restarts by itself).
  **Expect:** `[PASS] zero loss: occurrences == accepted events` and `[PASS] each event guarded exactly once`.
- [ ] Worker killed mid-step: run `{"nodes":[{"id":"s","action":"delay.sleep","params":{"seconds":30}},{"id":"c","action":"case.create","params":{"title":"\"chaos\"","severity":2}}],"edges":["s -> c"]}`, after 5 s `docker restart -t 0 soar-worker-1`.
  **Expect:** the step is redelivered after its ack deadline (timeout 60 s + 15 s), the execution succeeds within about 2 minutes, exactly one `chaos` case.
- [ ] Orchestrator restarted mid-run (`docker restart -t 0 soar-orchestrator-1`): the execution still finishes.
- [ ] Workflows survive an OpenSearch outage: run `{"nodes":[{"id":"nap","action":"delay.sleep","params":{"seconds":15}},{"id":"after","action":"notify.log","params":{"message":"\"done\""}}],"edges":["nap -> after"]}`,
  5 s later `docker stop soar-opensearch-1`, wait 40 s, `docker start soar-opensearch-1` (the API is down meanwhile:
  it needs OpenSearch too). **Expect:** the execution succeeds; the worker and orchestrator logs show `holding ...` then
  `OpenSearch answers again`; `GET /dlq` gains nothing.
- [ ] NATS restarted during a load test (`docker restart soar-nats-1`): with one dev node, ingest answers 503 while
  NATS is down, so loadgen's "no 5xx" gate fails by design. **Expect:** the zero-loss gate passes (every accepted
  event stored once). The real test needs the 3-node cluster of `deploy/prod`.

### Step 9 - Security
Gate: security suite passes.
- [ ] `make test-security` passes: every route x every role (and no token, a wrong key, an unsigned JWT) is allowed or
  refused exactly as the role table says, every mutation needs a write permission, API keys are stored hashed and
  shown once, secrets are write-only, mutations are audited, secret values are scrubbed from step outputs, errors
  and logs, SSRF guard, TLS/mTLS, 1 MiB / 64-level body limits, runner sandbox limits.
- [ ] `make vulncheck` (needs network): govulncheck and pip-audit report nothing your code calls. A standard-library
  finding fixed in a newer Go patch means: update Go (`dnf update golang`); the images build with the latest 1.26.
- [ ] Workflow audit: run any workflow that reads a secret or creates a case, then
  `api GET '/audit?limit=20' | jq -c '.items[] | {actor, actor_type, action, resource, request_id}'`.
  **Expect:** `actor: "workflow:<name>"`, `actor_type: "workflow"`, a `secret.read` per secret used and one record per
  case created; `request_id` is `<execution_id>:<node>:<attempt>`; never a secret value.
- [ ] Roles: `VIEWER=$(api POST /principals -d '{"type":"api_key","name":"chk-viewer","roles":["viewer"]}' | jq -r .api_key)`;
  with that key `GET /cases` is 200 and `POST /cases` is 403.
- [ ] Every mutation is audited with the real actor: `api GET /audit | jq '.items[0]'`.
- [ ] UI sign-in through Keycloak (http://localhost:3000, users `admin/admin`, `analyst/analyst`, `viewer/viewer`).
- [ ] Key rotation (docs/runbooks.md): secrets still decrypt after `rotate-secrets`.
- [ ] (Optional) HTTPS on the api: put a certificate in `deploy/compose/.env` as `SOAR_TLS_CERT_FILE`/`SOAR_TLS_KEY_FILE`
  (mounted into the container), `make up`. **Expect:** `curl --cacert <ca> https://localhost:8080/readyz` is 200, plain
  `http://` fails, and `make ps` still shows the api healthy (the healthcheck follows the TLS setting).

### Step 10 - Triggers and waits
Gate: survives a full restart.
- [ ] Cron: a workflow with top-level `"triggers":[{"type":"schedule","cron":"@every 1m"}]` runs once a minute.
- [ ] Approval timeout: `{"type":"approval","timeout":"1m","on_timeout":"reject"}` resolves without anyone acting.
- [ ] Restart survival: start a `wait` of 5 minutes, `make down && make up`, the execution resumes and finishes on time.
- [ ] Not built yet (expect nothing): syslog, Kafka/AMQP, file/object, SQL adapters; wait-for-event nodes.

### Step 11 - Cases and observables
Gate: alert -> case -> task end to end.
- [ ] `api POST /alerts/<alert_id>/case -d '{}'` opens a case; `api GET /cases/<id>/alerts` lists the snapshot.
- [ ] Add a task, complete it (`PATCH /cases/{id}/tasks/{task_id}`), upload evidence (`POST /cases/{id}/evidence?filename=x.txt` with `--data-binary`), download link works.
- [ ] Close the case: `closed_at` set; it disappears 3 days later (retention), its tasks, snapshots and evidence with it.

### Step 12 - Web UI
Gate: an analyst can run a playbook from the UI.
- [ ] `make web-test` passes.
- [ ] Sign in as `analyst/analyst`; open a workflow, run it, watch node states on the live canvas.
- [ ] Editor: change a node in the canvas, the YAML view follows, a bad CEL expression shows an inline error.

### Step 13 - Integrations
Gate: a new SIEM source added with configuration only, in under an hour.
- [ ] DFIR-IRIS 2.4.27: real service-account key; `POST /connections/{id}/test` is `ok: true`; then each `iris.*` action (all endpoints are marked *verify*).
- [ ] Outlook: app registration with a certificate; connection test; `outlook.mail.search` on the shared mailbox; `delete`/`purge` and sends over 50 recipients ask for approval.
- [ ] Wazuh manager actions (`wazuh.*`) against the real manager.
- [ ] New source by configuration only: a mapping (`POST /mappings`) + a webhook source (`POST /sources`), test with `/sources/{id}/test`, then real pushes. Time it.

### Step 14 - Observability and soak
Gate: 24 h soak with a daily traffic shape and hourly 1,000/s peaks; baselines documented.
- [ ] `docker compose -f deploy/compose/docker-compose.yml --profile monitoring up -d`; Prometheus on http://localhost:9090 shows every role `up` and the rules from `deploy/prometheus/alerts.yml`.
- [ ] Soak: `make loadtest RATE=100 DURATION=24h` for the average. `loadgen` sends a constant rate, so the hourly 1,000/s peaks need a second run on a schedule (for example `make loadtest RATE=1000 DURATION=5m` every hour from cron).

### Step 15 - Packaging
Gate: clean install < 15 min.
- [ ] Fresh Rocky Linux VM: `scripts/rocky10-setup.sh` + `make up`, timed.
- [ ] `scripts/backup.sh chk1`, delete a case, `scripts/restore.sh chk1` with the roles stopped (docs/runbooks.md), `make migrate-check`, the case is back.
- [ ] `deploy/prod/docker-compose.yml` starts against external OpenSearch, S3 and Keycloak with the required secrets.

---

## Known bugs

None open. Fixed on 2026-11-11 (1-2 found while writing this checklist, 3 by the first `make up`, 4-5 by live tests,
6-9 by a security review):

1. Triggers inside `definition` never fired (Step 6). The API now lifts them into the workflow's `triggers`, and
   the processor and scheduler also read any left in older stored definitions. Checked in Step 6.
2. An OpenSearch outage longer than ~10 s moved queued work to the DLQ (Steps 5/8). The processor, worker and
   orchestrator now hold in-flight messages and retry in place, and the processor pauses fetching. Checked in 5.3 and 8.
3. `make up` failed pulling `minio/minio` ("pull access denied, repository does not exist"): MinIO removed its
   images from Docker Hub and Quay. The dev stack's object storage is now the `s3` service (RustFS 1.0.5), and
   `scripts/backup.sh` / `restore.sh` copy the evidence bucket with rclone instead of `minio/mc`. Checked in section 0.
4. Every DFIR-IRIS action failed at once (Step 7/13): `integrations/dfir_iris/actions/endpoints.yaml` had unquoted
   `{case_id}` paths inside YAML flow mappings, which PyYAML rejects when the module loads. Paths are quoted; a Go test
   parses every integration YAML file and a Python test imports every integration module. Needs `make up` (runner image).
5. A processor killed between its main write and its acknowledgements counted the redelivered events twice in
   `occurrence_count` (Step 8; 160 extra in 60,000). The duplicate guard now records the applied increment in the
   same bulk request. Checked in Step 8.
6. A connection could name any environment variable as a secret (`env:SOAR_MASTER_KEY`), so an engineer could have
   the worker send the master key to a server of their choice. Only `SOAR_SECRET_*` (and the documented ingest/indexer
   prefixes) resolve now; secret scopes are enforced. Checked in Step 9 (`make test-security`).
7. Worker -> runner gRPC was always plain text. It is TLS/mTLS now; plain text needs the runner on the same host or
   `SOAR_RUNNER_INSECURE=true` (set by the dev stack).
8. Restarting the api re-enabled a disabled bootstrap API key. It is created once and left as admins leave it.
9. Approvers were matched by any principal's name (an API key could be labelled like a user) and an unverified OIDC
   email counted as a name. API keys match by id only; emails must be verified.

## Not built yet (from the spec; do not expect these to work)

* `POST /sources/{id}/replay` and `POST /filters/simulate` (spec 9.5.1, 11); replay one record at a time with `POST /dlq/{id}/replay`.
* Syslog, Kafka/AMQP/SQS, file/object and SQL pull adapters; wait-for-event nodes (Step 10).
* Outlook delta links and change notifications (polling with overlap is used); OpenTelemetry tracing; a Helm chart.
* Pull overlap above ~330 alerts/s: only the newest 20,000 records of the 60 s window are re-read (`overlap_pages`).

## Results log

| Date | Step | Check | Result | Notes |
|---|---|---|---|---|
| | | | | |
