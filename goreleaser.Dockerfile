# Dockerfile used by GoReleaser during `goreleaser release`.
# Unlike the multi-stage `Dockerfile` (which builds from source for local
# `docker build .`), this one assumes the binary is pre-built by goreleaser
# and just copies it in. Do NOT use it directly for local builds.

FROM gcr.io/distroless/static-debian13:nonroot

ARG TARGETPLATFORM

COPY $TARGETPLATFORM/omnifeed /usr/local/bin/omnifeed

USER 65532:65532

EXPOSE 8080 8081 9090

ENTRYPOINT ["/usr/local/bin/omnifeed"]
