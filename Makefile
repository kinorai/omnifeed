.DEFAULT_GOAL := help

GO            := go
# Go-built dev tools are pinned in tools/go.mod (`tool` directives), a module
# of its own so their dependencies never enter omnifeed's go.mod or go.sum.
# govulncheck only: gomarkdoc v1.1.0 depends on go-git v5.3.0 and viper 1.7,
# whose CVEs Trivy reports on any go.mod in the repo; it stays a pinned
# `go install` (here and in docs.yml).
GOTOOL        := $(GO) tool -modfile=tools/go.mod
GOFLAGS       := -trimpath
BIN_DIR       := bin
BINARY        := $(BIN_DIR)/omnifeed
PKG           := ./cmd/omnifeed
VERSION       := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT        := $(shell git rev-parse HEAD 2>/dev/null || echo none)
DATE          := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS       := -s -w \
                 -X github.com/kinorai/omnifeed/internal/version.Version=$(VERSION) \
                 -X github.com/kinorai/omnifeed/internal/version.Commit=$(COMMIT) \
                 -X github.com/kinorai/omnifeed/internal/version.Date=$(DATE)

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

## --- Dev loop ---

.PHONY: run
run: ## Run the proxy locally (uses default OMNIFEED_* env vars)
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

.PHONY: fix
fix: ## Apply the go fix modernizers (mutates files)
	$(GO) fix ./...

.PHONY: fix-check
fix-check: ## Fail if `go fix` would rewrite anything (non-mutating, used by CI + pre-commit)
	@out=$$($(GO) fix -diff ./...); rc=$$?; \
	if [ $$rc -ne 0 ] || [ -n "$$out" ]; then \
	  echo "go fix has pending modernizations or failed (run 'make fix' and commit the diff):"; \
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

.PHONY: tools-tidy
tools-tidy: ## Tidy tools/go.mod after bumping a tool (go get -modfile=tools/go.mod -tool <pkg>@<v>)
	cd tools && $(GO) mod tidy

.PHONY: tidy-check
tidy-check: ## Fail if go.mod/go.sum or tools/go.mod/go.sum would change after `go mod tidy` (non-mutating)
	@$(GO) mod tidy
	@cd tools && $(GO) mod tidy
	@if ! git diff --quiet go.mod go.sum tools/go.mod tools/go.sum; then \
	  echo "go.mod or go.sum are not tidy. Run 'make tidy tools-tidy' and commit the diff:"; \
	  git --no-pager diff go.mod go.sum tools/go.mod tools/go.sum; \
	  exit 1; \
	fi

.PHONY: check
check: vet fix-check lint test ## Run all checks (vet + fix-check + lint + test)

## --- Docker / compose ---

.PHONY: docker-build
docker-build: ## Build the local Docker image (multi-stage Dockerfile)
	docker build -t kinorai/omnifeed:local .

.PHONY: compose-up
compose-up: ## Start full stack (proxy + crawl4ai upstream); creates .env on first run
	@test -f .env || echo "CRAWL4AI_API_TOKEN=$$(openssl rand -hex 32)" > .env
	docker compose up -d

.PHONY: compose-down
compose-down: ## Stop the stack
	docker compose down

.PHONY: compose-logs
compose-logs: ## Tail logs from the stack
	docker compose logs -f

## --- Apple container (macOS, Apple Silicon — no Docker) ---

.PHONY: container-up
container-up: ## Start the stack on Apple `container` (compose-less; see scripts/container)
	./scripts/container up

.PHONY: container-down
container-down: ## Stop + remove the Apple `container` stack
	./scripts/container down

.PHONY: container-status
container-status: ## Show Apple `container` stack state / IP / health
	./scripts/container status

.PHONY: container-logs
container-logs: ## Tail omnifeed logs from the Apple `container` stack
	./scripts/container logs -f


## --- Release (delegated to goreleaser) ---

.PHONY: snapshot
snapshot: ## Build a snapshot release locally (no publish)
	goreleaser release --snapshot --clean --skip=publish,sign

.PHONY: release-check
release-check: ## Validate .goreleaser.yaml without releasing
	goreleaser check

## --- Tools ---

.PHONY: install-tools
install-tools: ## Install the tools tools/go.mod does not pin (golangci-lint, goreleaser, gomarkdoc)
	# .golangci.yml declares `version: "2"`, so the v2 module path is required.
	# The unversioned path resolves to the last v1 release, which then refuses
	# the config with "you are using a configuration file for golangci-lint v2
	# with golangci-lint v1" — a confusing failure a long way from its cause.
	# Upstream does not recommend `go install` at all (the binary depends on the
	# local Go version); CI uses golangci-lint-action, which downloads a build.
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	# goreleaser stays a binary too: CI runs goreleaser-action, and its module
	# graph (every cloud SDK it publishes to) would dwarf the rest of tools/.
	$(GO) install github.com/goreleaser/goreleaser/v2@latest
	# Same version as GOMARKDOC_VERSION in .github/workflows/docs.yml.
	$(GO) install github.com/princjef/gomarkdoc/cmd/gomarkdoc@v1.1.0
	# govulncheck needs no install: `go tool` builds it from tools/go.mod.

.PHONY: pre-commit-install
pre-commit-install: ## Install git hooks (gofmt/vet/golangci-lint) — run once per clone
	@command -v pre-commit >/dev/null 2>&1 || { echo "pre-commit not installed. Run: brew install pre-commit  (or: pip install pre-commit)"; exit 1; }
	pre-commit install --install-hooks

.PHONY: pre-commit-run
pre-commit-run: ## Run all pre-commit hooks against every file (CI parity check)
	pre-commit run --all-files

.PHONY: govulncheck
govulncheck: ## Scan for known vulnerabilities
	$(GOTOOL) govulncheck ./...

## --- Cleanup ---

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) coverage.txt coverage.html dist
