.DEFAULT_GOAL := help

APP := gpt-load
WEB_DIR := web
GO ?= go
PNPM ?= corepack pnpm

.PHONY: _web-deps
_web-deps:
	$(PNPM) --dir $(WEB_DIR) install --frozen-lockfile

.PHONY: _web-build
_web-build: _web-deps
	$(PNPM) --dir $(WEB_DIR) run build

.PHONY: dev
dev: _web-build ## Build the Web UI and run with race detection
	$(GO) run -race .

.PHONY: run
run: _web-build ## Build the Web UI and run the application
	$(GO) run .

.PHONY: build
build: _web-build ## Build the Web UI and application binary
	$(GO) build -o $(APP) .

GPT_LOAD_DATABASE_TEST_DSN ?= postgres://postgres:postgres@127.0.0.1:55432/gpt_load?sslmode=disable
GPT_LOAD_REDIS_TEST_ADDR ?= 127.0.0.1:56379
export GPT_LOAD_DATABASE_TEST_DSN GPT_LOAD_REDIS_TEST_ADDR

.PHONY: test-deps
test-deps: ## Start the PostgreSQL and Redis the Go tests use (needs Docker)
	docker compose -f tools/testdeps/compose.yml up -d --wait

.PHONY: test
test: test-deps ## Run Go unit tests
	$(GO) test -count=1 . ./internal/... ./tools/...

.PHONY: check
check: _web-deps test-deps ## Run source checks and build
	@go_root="$$($(GO) env GOROOT)"; formatted_files="$$("$${go_root}/bin/gofmt" -l .)"; test -z "$${formatted_files}"
	$(GO) mod tidy -diff
	$(GO) vet ./...
	$(PNPM) --dir $(WEB_DIR) run lint
	$(PNPM) --dir $(WEB_DIR) run format
	$(PNPM) --dir $(WEB_DIR) run build
	$(GO) build -o $(APP) .
	$(GO) test -count=1 . ./internal/... ./tools/...
	git --no-pager diff --check

.PHONY: cluster-e2e
cluster-e2e: ## Run cluster acceptance on the compose sample with a real Redis Cluster
	tools/clusterbench/run.sh e2e

.PHONY: bench-cluster
bench-cluster: ## Measure cluster throughput and p99 against a single instance (needs >= 8 CPUs)
	tools/clusterbench/run.sh bench

.PHONY: help
help: ## Display available targets
	@awk 'BEGIN {FS = ":.*?## "; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*?## / { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
