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
	@awk 'BEGIN{FS=":.*## "} /^[a-zA-Z_-]+:.*## /{printf "  \033[36m%-18s\033[0m %s\n",$$1,$$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the soar binary into bin/
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/soar ./cmd/soar

.PHONY: image
image: ## Build the soar:dev container image
	$(DOCKER) build -f deploy/docker/Dockerfile --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t soar:dev .

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
migrate: ## Run `soar migrate` once
	$(COMPOSE) $(COMPOSE_F) run --rm migrate

.PHONY: smoke
smoke: ## curl /readyz on the api (Step 1 exit gate)
	@curl -fsS -i http://127.0.0.1:8080/readyz && echo

.PHONY: test
test: ## Unit tests
	$(GO) test ./...

.PHONY: test-race
test-race: ## Unit tests with the race detector (needs gcc)
	CGO_ENABLED=1 $(GO) test -race ./...

.PHONY: test-integration
test-integration: ## Integration tests against a running stack (make up first)
	$(GO) test -tags integration -count=1 ./test/integration/...

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
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8
