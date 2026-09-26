# syntax=docker/dockerfile:1
# Build the manager binary
# --platform=$BUILDPLATFORM: compile natively and cross-compile via GOOS/GOARCH
# instead of emulating every target platform under `make docker-buildx`.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Cache modules separately from source for faster rebuilds.
COPY go.mod go.mod
COPY go.sum go.sum
# BuildKit cache mounts: modules and compiled packages (Helm SDK, client-go...)
# survive between builds, so only changed code gets recompiled.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o manager ./cmd/main.go

# Runtime image.
#
# NOT distroless/scratch: the reconciler shells out to `git` to fetch each
# PreviewEnvironment's application chart dynamically per spec.source (DAT §3,
# internal/controller/helmchart.go) — a static/no-shell base can't do that.
FROM alpine:3.22
RUN apk add --no-cache git ca-certificates \
    && addgroup -S -g 65532 ephora \
    && adduser -S -u 65532 -G ephora ephora
WORKDIR /
COPY --from=builder /workspace/manager .
# Numeric UID/GID: required for Kubernetes to verify `runAsNonRoot: true`
# (config/manager/manager.yaml) — a named user is rejected at pod start.
USER 65532:65532

ENTRYPOINT ["/manager"]
