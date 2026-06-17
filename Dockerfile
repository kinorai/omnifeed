# syntax=docker.io/docker/dockerfile:1
#
# Multi-stage build for omnifeed.
# Final image is distroless/static (~3-4MB layer + 16MB binary), runs as nonroot.

ARG GO_VERSION=1.26
ARG DISTROLESS_TAG=nonroot

# Pin the builder to the native build platform and cross-compile via GOARCH —
# far faster than emulating the target arch under QEMU.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

WORKDIR /build

# Cache module downloads in their own layer.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# CGO off → pure-static binary. TARGETOS/TARGETARCH are injected by buildx per
# target platform, so Go cross-compiles natively on the build host (no QEMU).
# VERSION stamps the binary (release tag, or "edge"/"dev" for channel builds).
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w -X github.com/kinorai/omnifeed/internal/version.Version=${VERSION}" \
    -o /out/omnifeed ./cmd/omnifeed

FROM gcr.io/distroless/static-debian13:${DISTROLESS_TAG}

LABEL org.opencontainers.image.title="omnifeed"
LABEL org.opencontainers.image.description="Self-hosted web search (SearXNG) + LLM-friendly crawling with a dedicated Reddit engine — MCP server, Open WebUI compatible"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.source="https://github.com/kinorai/omnifeed"

COPY --from=builder /out/omnifeed /usr/local/bin/omnifeed

USER 65532:65532

EXPOSE 8080 8081 9090

ENTRYPOINT ["/usr/local/bin/omnifeed"]
