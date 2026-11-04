# syntax=docker/dockerfile:1
#
# Multi-arch image (linux/amd64, linux/arm64). Everything is cross-compiled on
# the build machine's native platform, so ARM images build quickly without
# CPU emulation:
#   docker buildx build --platform linux/amd64,linux/arm64 -t oceanic-rmm .

# ---- shared Go build environment (always the builder's native platform) ----
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS base
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ENV LDFLAGS="-s -w -X github.com/imshodj/oceanic/internal/protocol.AgentVersion=${VERSION}"

# ---- agents for every OS the dashboard can deploy to ----
# Doesn't depend on the image's target platform, so buildx builds it once
# and shares it between the amd64 and arm64 images.
FROM base AS agents
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out/agents && \
    for p in windows/amd64 windows/arm64 linux/amd64 linux/arm64 linux/arm darwin/arm64 darwin/amd64; do \
      os=${p%/*}; arch=${p#*/}; ext=""; [ "$os" = windows ] && ext=.exe; \
      echo "building agent $os/$arch" && \
      GOOS=$os GOARCH=$arch GOARM=7 go build -trimpath -ldflags "$LDFLAGS" \
        -o /out/agents/oceanic-agent-$os-$arch$ext ./cmd/oceanic-agent || exit 1; \
    done

# ---- server for the image's own platform ----
FROM base AS server
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "$LDFLAGS" -o /out/oceanic-server ./cmd/oceanic-server && \
    mkdir -p /out/data

# ---- runtime: tiny, non-root, no shell ----
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="OCeanic RMM" \
      org.opencontainers.image.description="Self-hosted homelab RMM: monitoring, scripting, terminal and remote desktop behind a Cloudflare Tunnel" \
      org.opencontainers.image.source="https://github.com/imshodj/oceanic"
WORKDIR /app
COPY --from=server /out/oceanic-server /app/oceanic-server
COPY --from=agents /out/agents /app/agents
# Pre-create /data owned by the nonroot user so a fresh named volume is writable.
COPY --from=server --chown=65532:65532 /out/data /data
ENV OCEANIC_LISTEN=:8080 \
    OCEANIC_DATA_DIR=/data \
    OCEANIC_AGENTS_DIR=/app/agents
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/app/oceanic-server", "healthcheck"]
ENTRYPOINT ["/app/oceanic-server"]
