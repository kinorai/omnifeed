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
fmt: ## Format code with gofmt + goimports
	gofmt -s -w .
	@command -v goimports >/dev/null 2>&1 && goimports -w . || true

.PHONY: lint
lint: ## Run golangci-lint (install via `make install-tools`)
	golangci-lint run ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: tidy
tidy: ## Run go mod tidy
	$(GO) mod tidy

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

.PHONY: govulncheck
govulncheck: ## Scan for known vulnerabilities
	govulncheck ./...

## --- Cleanup ---

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) coverage.txt coverage.html dist
