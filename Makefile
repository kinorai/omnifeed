.DEFAULT_GOAL := help

GO            := go
GOFLAGS       := -trimpath
BIN_DIR       := bin
BINARY        := $(BIN_DIR)/crawl4ai-reddit-proxy
PKG           := ./cmd/crawl4ai-reddit-proxy
VERSION       := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT        := $(shell git rev-parse HEAD 2>/dev/null || echo none)
DATE          := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS       := -s -w \
                 -X github.com/kinorai/crawl4ai-reddit-proxy/internal/version.Version=$(VERSION) \
                 -X github.com/kinorai/crawl4ai-reddit-proxy/internal/version.Commit=$(COMMIT) \
                 -X github.com/kinorai/crawl4ai-reddit-proxy/internal/version.Date=$(DATE)

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

## --- Dev loop ---

.PHONY: run
run: ## Run the proxy locally (uses default CARP_* env vars)
	$(GO) run $(GOFLAGS) -ldflags="$(LDFLAGS)" $(PKG)

.PHONY: build
build: ## Build the binary into ./bin
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BINARY) $(PKG)

.PHONY: test
test: ## Run tests with race detector
	$(GO) test -race ./...

.PHONY: test-cover
test-cover: ## Run tests with coverage report
	$(GO) test -race -coverprofile=coverage.txt -covermode=atomic ./...
	$(GO) tool cover -html=coverage.txt -o coverage.html
	@echo "Coverage report: coverage.html"

.PHONY: bench
bench: ## Run benchmarks
	$(GO) test -bench=. -benchmem ./...

## --- Lint / format ---

.PHONY: fmt
fmt: ## Format code with gofmt + goimports (mutates files)
	gofmt -s -w .
	@command -v goimports >/dev/null 2>&1 && goimports -w . || true

.PHONY: fmt-check
fmt-check: ## Fail if any .go file is not gofmt-clean (non-mutating, used by CI + pre-commit)
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
	  echo "Files not gofmt-clean (run 'make fmt' to fix):"; \
	  echo "$$out"; \
	  exit 1; \
	fi

.PHONY: lint
lint: ## Run golangci-lint (install via `make install-tools`)
	golangci-lint run ./...

.PHONY: config-verify
config-verify: ## Validate .golangci.yml against the schema (catches typos before CI does)
	golangci-lint config verify

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: tidy
tidy: ## Run go mod tidy (mutates go.mod / go.sum)
	$(GO) mod tidy

.PHONY: tidy-check
tidy-check: ## Fail if go.mod or go.sum would change after `go mod tidy` (non-mutating)
	@$(GO) mod tidy
	@if ! git diff --quiet go.mod go.sum; then \
	  echo "go.mod or go.sum are not tidy. Run 'make tidy' and commit the diff:"; \
	  git --no-pager diff go.mod go.sum; \
	  exit 1; \
	fi

.PHONY: check
check: vet lint test ## Run all checks (vet + lint + test)

## --- Docker / compose ---

.PHONY: docker-build
docker-build: ## Build the local Docker image (multi-stage Dockerfile)
	docker build -t kinorai/crawl4ai-reddit-proxy:local .

.PHONY: compose-up
compose-up: ## Start full stack (proxy + crawl4ai upstream)
	docker compose up -d

.PHONY: compose-down
compose-down: ## Stop the stack
	docker compose down

.PHONY: compose-logs
compose-logs: ## Tail logs from the stack
	docker compose logs -f

.PHONY: compose-standalone
compose-standalone: ## Start standalone (Reddit-only) mode
	docker compose -f docker-compose.standalone.yml up -d

## --- Release (delegated to goreleaser) ---

.PHONY: snapshot
snapshot: ## Build a snapshot release locally (no publish)
	goreleaser release --snapshot --clean --skip=publish,sign

.PHONY: release-check
release-check: ## Validate .goreleaser.yaml without releasing
	goreleaser check

## --- Tools ---

.PHONY: install-tools
install-tools: ## Install development tools (golangci-lint, govulncheck, gomarkdoc)
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	$(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	$(GO) install github.com/princjef/gomarkdoc/cmd/gomarkdoc@latest
	$(GO) install github.com/goreleaser/goreleaser/v2@latest

.PHONY: pre-commit-install
pre-commit-install: ## Install git hooks (gofmt/vet/golangci-lint) — run once per clone
	@command -v pre-commit >/dev/null 2>&1 || { echo "pre-commit not installed. Run: brew install pre-commit  (or: pip install pre-commit)"; exit 1; }
	pre-commit install --install-hooks

.PHONY: pre-commit-run
pre-commit-run: ## Run all pre-commit hooks against every file (CI parity check)
	pre-commit run --all-files

.PHONY: govulncheck
govulncheck: ## Scan for known vulnerabilities
	govulncheck ./...

## --- Cleanup ---

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) coverage.txt coverage.html dist
