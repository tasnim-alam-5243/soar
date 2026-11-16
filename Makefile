SHELL := /bin/bash
.DEFAULT_GOAL := help

GO        ?= go
COMPOSE   ?= docker compose
DOCKER    ?= docker
COMPOSE_F := -f deploy/compose/docker-compose.yml
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
MODULE    := $(shell $(GO) list -m 2>/dev/null)
LDFLAGS   := -s -w -X $(MODULE)/internal/obs.Version=$(VERSION) -X $(MODULE)/internal/obs.Commit=$(COMMIT)

.PHONY: help
help: ## Show this help
	@awk 'BEGIN{FS=":.*## "} /^[a-zA-Z0-9_-]+:.*## /{printf "  \033[36m%-18s\033[0m %s\n",$$1,$$2}' $(MAKEFILE_LIST)

.PHONY: build build-go
build: web-build ## Build bin/soar with the web UI embedded (one binary: API + UI; needs Node)
	CGO_ENABLED=0 $(GO) build -tags ui -trimpath -ldflags '$(LDFLAGS)' -o bin/soar ./cmd/soar

build-go: ## Build bin/soar without the UI (no Node needed; / explains how to get the UI)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/soar ./cmd/soar

.PHONY: image image-soar image-runner
image: image-soar image-runner ## Build all container images (soar with the UI, soar-runner)

image-soar: ## Build the soar:dev image (every Go role; the api role serves the UI)
	$(DOCKER) build --pull -f deploy/docker/Dockerfile --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t soar:dev .

image-runner: ## Build the soar-runner:dev image (sandboxed Python runner + integrations)
	$(DOCKER) build -f deploy/docker/runner.Dockerfile -t soar-runner:dev .

PY_VENV := runner/python/.venv
PY      := $(PY_VENV)/bin/python
GOBIN_DIR := $(shell $(GO) env GOPATH)/bin

.PHONY: runner-venv proto test-python web-install web-dev web-test web-build gen-api
runner-venv: ## Create the Python virtualenv for runner development and tests
	python3 -m venv $(PY_VENV) && $(PY) -m pip install -q -r runner/python/requirements-dev.txt

proto: ## Regenerate Go and Python stubs from api/proto/runner.proto (needs runner-venv)
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
	$(PY) -m grpc_tools.protoc -I api/proto \
	  --plugin=protoc-gen-go=$(GOBIN_DIR)/protoc-gen-go --plugin=protoc-gen-go-grpc=$(GOBIN_DIR)/protoc-gen-go-grpc \
	  --go_out=internal/runner/runnerpb --go_opt=paths=source_relative \
	  --go-grpc_out=internal/runner/runnerpb --go-grpc_opt=paths=source_relative api/proto/runner.proto
	$(PY) -m grpc_tools.protoc -I api/proto --python_out=runner/python/soar_runner --grpc_python_out=runner/python/soar_runner api/proto/runner.proto
	sed -i 's/^import runner_pb2 as runner__pb2/from . import runner_pb2 as runner__pb2/' runner/python/soar_runner/runner_pb2_grpc.py

test-python: ## Runner tests (sandbox limits, secret scrubbing, gRPC)
	cd runner/python && .venv/bin/python -m pytest -q tests

web-install: ## Install UI dependencies
	cd web && npm ci --no-audit --no-fund

gen-api: ## Regenerate the UI's API types from api/openapi.yaml
	cd web && npm run gen:api

web-dev: ## UI dev server on :3000 with hot reload (proxies /api to 127.0.0.1:8080, or SOAR_API)
	cd web && npm run dev

web-test: ## UI unit tests and type check
	cd web && npm run typecheck && npm test

web-build: ## Production UI bundle into internal/webui/dist (embedded by `make build`)
	@test -d web/node_modules || (cd web && npm ci --no-audit --no-fund)
	cd web && npm run gen:api && npm run build

.PHONY: up
up: image ## Build the image, start the stack, wait until /readyz is 200
	$(COMPOSE) $(COMPOSE_F) up -d
	./scripts/wait-ready.sh http://127.0.0.1:8080/readyz 240
	@$(COMPOSE) $(COMPOSE_F) ps

.PHONY: down
down: ## Stop the stack (keeps data volumes)
	$(COMPOSE) $(COMPOSE_F) down

.PHONY: clean
clean: ## Stop the stack and DELETE data volumes
	$(COMPOSE) $(COMPOSE_F) down -v

.PHONY: ps
ps: ## Show container status
	$(COMPOSE) $(COMPOSE_F) ps

.PHONY: logs
logs: ## Follow logs (make logs S=api to pick a service)
	$(COMPOSE) $(COMPOSE_F) logs -f --tail=100 $(S)

.PHONY: migrate
migrate: image ## Apply index templates, indices and retention policies (safe to repeat)
	$(COMPOSE) $(COMPOSE_F) run --rm migrate

.PHONY: nats-relaxed nats-always
nats-relaxed: ## NATS fsync every 200 ms (needed for 1,000+ events/s on ordinary disks; see README)
	@touch deploy/compose/.env && sed -i '/^NATS_CONF=/d' deploy/compose/.env && echo 'NATS_CONF=nats-relaxed.conf' >> deploy/compose/.env
	$(COMPOSE) $(COMPOSE_F) up -d --force-recreate nats
nats-always: ## NATS fsync on every write (spec default for one node)
	@touch deploy/compose/.env && sed -i '/^NATS_CONF=/d' deploy/compose/.env
	$(COMPOSE) $(COMPOSE_F) up -d --force-recreate nats

.PHONY: seed
seed: image ## Install the built-in source packs (Wazuh); safe to repeat
	$(COMPOSE) $(COMPOSE_F) run --rm seed

.PHONY: loadtest loadtest-burst
RATE ?= 1000
DURATION ?= 60s
loadtest: ## Zero-loss load test through ingest -> NATS -> processor -> OpenSearch (RATE, DURATION)
	go run ./cmd/loadgen -rate $(RATE) -duration $(DURATION)

loadtest-burst: ## 2,000/s burst for 60 s (spec 3.3)
	go run ./cmd/loadgen -rate 2000 -duration 60s -drain-timeout 5m

.PHONY: migrate-check
migrate-check: image ## Verify live indices match the schema (Step 2 exit gate); changes nothing
	$(COMPOSE) $(COMPOSE_F) run --rm migrate migrate-check

.PHONY: smoke
smoke: ## curl /readyz on the api (Step 1 exit gate)
	@curl -fsS -i http://127.0.0.1:8080/readyz && echo

.PHONY: test
test: ## Unit tests (Go)
	$(GO) test ./...

.PHONY: test-all
test-all: test test-python web-test ## Go, Python runner and UI tests

.PHONY: test-race
test-race: ## Unit tests with the race detector (needs gcc)
	CGO_ENABLED=1 $(GO) test -race ./...

.PHONY: test-integration
test-integration: ## Integration tests against a running stack (make up first)
	$(GO) test -tags integration -count=1 ./test/integration/...

.PHONY: test-security vulncheck
test-security: ## Security suite (Step 9 gate): authN/Z matrix, API keys, secrets, audit, scrubbing, SSRF, TLS/mTLS, limits, sandbox
	$(GO) test -count=1 ./internal/security/... ./internal/config/... ./internal/obs/... ./internal/policy/... ./cmd/soar/...
	$(GO) test -count=1 -run 'TestSecurity|TestAuthenticationAuthorisationAndAudit' ./internal/api/...
	$(GO) test -count=1 -run 'TestFlowSecrets|TestFlowDryRunWrites|TestFlowDestructiveActionNeedsApproval|TestFlowScopedSecret' ./internal/e2e/...
	$(GO) test -count=1 -run 'MutualTLS' ./internal/runner/...
	$(GO) test -count=1 -run 'TestSSRFGuard' ./internal/actions/...
	$(GO) test -count=1 -run 'TestAuthentication|TestRejections' ./internal/ingest/...
	$(GO) test -count=1 -run 'PrivateCA' ./internal/store/opensearch/...
	cd runner/python && .venv/bin/python -m pytest -q tests -k "secrets or environment or path_escape or limit or timeout"

vulncheck: ## Known-vulnerability scan of Go modules and the runner's Python packages (network)
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
	$(PY) -m pip install -q pip-audit==2.10.1 && $(PY) -m pip_audit -r runner/python/requirements.txt

.PHONY: check-steps check-step5 check-step6 check-step7
check-steps: ## Live PASS/FAIL checks of Steps 5, 6 and 7 against the running stack (~3 min)
	$(GO) run ./cmd/stepcheck
check-step5: ## Live checks of Step 5 only (ingestion, queue, filters, DLQ)
	$(GO) run ./cmd/stepcheck -step 5
check-step6: ## Live checks of Step 6 only (engine)
	$(GO) run ./cmd/stepcheck -step 6
check-step7: ## Live checks of Step 7 only (worker, runner, SSRF, secrets)
	$(GO) run ./cmd/stepcheck -step 7

.PHONY: sync-pull-source
sync-pull-source: ## Copy the current wazuh-pull pack config onto an existing (older) wazuh-pull source
	$(GO) run ./cmd/stepcheck -sync-pull-source

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## golangci-lint (install with `make tools`)
	golangci-lint run ./...

.PHONY: fmt
fmt: ## gofumpt -w (install with `make tools`)
	gofumpt -w .

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: tools
tools: ## Install gofumpt and golangci-lint into $(go env GOPATH)/bin
	$(GO) install mvdan.cc/gofumpt@v0.7.0
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
