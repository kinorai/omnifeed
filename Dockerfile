# syntax=docker.io/docker/dockerfile:1
#
# Multi-stage build for search-crawl-reddit-proxy.
# Final image is distroless/static (~3-4MB layer + 16MB binary), runs as nonroot.

ARG GO_VERSION=1.26
ARG DISTROLESS_TAG=nonroot

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /build

# Cache module downloads in their own layer.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# CGO off → pure-static binary. ldflags strip debug info for size.
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/search-crawl-reddit-proxy ./cmd/search-crawl-reddit-proxy

FROM gcr.io/distroless/static-debian13:${DISTROLESS_TAG}

LABEL org.opencontainers.image.title="search-crawl-reddit-proxy"
LABEL org.opencontainers.image.description="Self-hosted web search (SearXNG) + LLM-friendly crawling with a dedicated Reddit engine — MCP server, Open WebUI compatible"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.source="https://github.com/kinorai/search-crawl-reddit-proxy"

COPY --from=builder /out/search-crawl-reddit-proxy /usr/local/bin/search-crawl-reddit-proxy

USER 65532:65532

EXPOSE 8080 8081 9090

ENTRYPOINT ["/usr/local/bin/search-crawl-reddit-proxy"]
