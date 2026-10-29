# SOAR platform

Implementation of the SOAR technical spec v0.9. **Current state: Step 1 (Foundation).**

One Go binary (`soar`), one image, role chosen by subcommand. Step 1 delivers the repo layout, the dev stack,
config validation, JSON logging, health endpoints and CI. No business logic yet.

## Quick start (Rocky Linux 10.x)

```bash
sudo ./scripts/rocky10-setup.sh      # once: Docker CE, compose plugin, Go, make, vm.max_map_count
# log out/in so the docker group applies, then:
./scripts/rename-module.sh github.com/<you>/soar     # once: set your real module path
cp deploy/compose/.env.example deploy/compose/.env   # optional
make up                              # builds soar:dev, starts the stack, waits for /readyz
make smoke                           # curl -i localhost:8080/readyz  -> 200 {"checks":{"nats":"ok","opensearch":"ok"},"status":"ok"}
make test                            # unit tests
make test-integration                # needs the stack running
make down                            # stop (keep data) | make clean (delete data)
```

`make help` lists every target. Go on the host is only needed for `make test/lint/build`; images build inside Docker.

## Ports (all bound to 127.0.0.1)

| Service | Port | Notes |
|---|---|---|
| api | 8080 | `/healthz`, `/readyz` (REST arrives in Step 4) |
| ingest | 8081 | receiver arrives in Step 5 |
| processor / collector / orchestrator / worker / scheduler | 8082 / 8083 / 8084 / 8085 / 8086 | health only, inside the compose network |
| OpenSearch | 9200 | security plugin **off** in dev |
| OpenSearch Dashboards | 5601 | |
| NATS | 4222 (client), 8222 (monitor) | JetStream on, `sync_interval: always` |

## Configuration (environment, validated at start, bad config = exit 2)

| Variable | Required | Default |
|---|---|---|
| `SOAR_OS_URLS` | yes | comma-separated http(s) URLs |
| `SOAR_OS_USER` / `SOAR_OS_PASSWORD` | no | |
| `SOAR_NATS_URL` | yes | one or more `nats://` URLs, comma-separated |
| `SOAR_PARTITIONS` | no | 8 (1-256) |
| `SOAR_LOG_LEVEL` | no | info |
| `SOAR_HTTP_ADDR` | no | role default (table above) |

Master key, OIDC and blob-store variables from spec §16 are added by the steps that use them (9 and 5).

## Layout

Follows spec §4. Packages that belong to later steps contain only a `doc.go` naming the step that fills them.
Import rule: `domain` imports nothing internal; nothing imports `cmd`.

## Step 1 exit gate

- [ ] `make up` finishes and all containers are healthy (`make ps`)
- [ ] `curl localhost:8080/readyz` returns 200 with `opensearch` and `nats` both `ok`
- [ ] `make test` and `make vet` pass; CI green on GitHub
- [ ] `docker stop soar-nats-1` makes `/readyz` return 503 within seconds; starting it restores 200
- [ ] `docker compose -f deploy/compose/docker-compose.yml run --rm -e SOAR_PARTITIONS=0 api` exits 2 and logs every problem
