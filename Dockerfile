# syntax=docker.io/docker/dockerfile:1
#
# Multi-stage build for crawl4ai-reddit-proxy.
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
    go build -trimpath -ldflags="-s -w" -o /out/crawl4ai-reddit-proxy ./cmd/crawl4ai-reddit-proxy

FROM gcr.io/distroless/static-debian13:${DISTROLESS_TAG}

LABEL org.opencontainers.image.title="crawl4ai-reddit-proxy"
LABEL org.opencontainers.image.description="LLM-friendly web crawler & scraper with a dedicated Reddit engine, built on Crawl4AI — Open WebUI compatible"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.source="https://github.com/kinorai/crawl4ai-reddit-proxy"

COPY --from=builder /out/crawl4ai-reddit-proxy /usr/local/bin/crawl4ai-reddit-proxy

USER 65532:65532

EXPOSE 8080 8081 9090

ENTRYPOINT ["/usr/local/bin/crawl4ai-reddit-proxy"]
