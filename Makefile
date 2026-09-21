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

.PHONY: test
test: ## Run Go unit tests
	$(GO) test -count=1 . ./internal/...

# REDIS_TEST_DSN points the opt-in cross-instance suite at a live Redis. The
# suite skips without it, which is what keeps `make test` hermetic.
REDIS_TEST_DSN ?= redis://127.0.0.1:16379/0

.PHONY: test-distributed
test-distributed: ## Run the cross-instance tests against a live Redis
	docker run --rm -d --name gpt-load-redis-test -p 16379:6379 redis:7-alpine >/dev/null
	@trap 'docker rm -f gpt-load-redis-test >/dev/null' EXIT; \
	GPT_LOAD_REDIS_TEST_DSN=$(REDIS_TEST_DSN) \
	$(GO) test -count=1 -run '^TestExternalRedis' \
		./internal/coordination ./internal/control ./internal/gateway

.PHONY: check
check: _web-deps ## Run source checks and build
	@go_root="$$($(GO) env GOROOT)"; formatted_files="$$("$${go_root}/bin/gofmt" -l .)"; test -z "$${formatted_files}"
	$(GO) mod tidy -diff
	$(GO) vet ./...
	$(PNPM) --dir $(WEB_DIR) run lint
	$(PNPM) --dir $(WEB_DIR) run format
	$(PNPM) --dir $(WEB_DIR) run build
	$(GO) build -o $(APP) .
	$(GO) test -count=1 . ./internal/...
	git --no-pager diff --check

.PHONY: help
help: ## Display available targets
	@awk 'BEGIN {FS = ":.*?## "; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*?## / { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
