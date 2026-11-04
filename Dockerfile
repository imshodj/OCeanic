# syntax=docker/dockerfile:1

# ---- build: server + agents for every supported platform ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN LDFLAGS="-s -w -X github.com/imshodj/oceanic/internal/protocol.AgentVersion=${VERSION}" && \
    go build -trimpath -ldflags "$LDFLAGS" -o dist/oceanic-server ./cmd/oceanic-server && \
    mkdir -p dist/agents && \
    for p in windows/amd64 windows/arm64 linux/amd64 linux/arm64 linux/arm; do \
      os=${p%/*}; arch=${p#*/}; ext=""; [ "$os" = windows ] && ext=.exe; \
      echo "building agent $os/$arch" && \
      GOOS=$os GOARCH=$arch GOARM=7 go build -trimpath -ldflags "$LDFLAGS" \
        -o dist/agents/oceanic-agent-$os-$arch$ext ./cmd/oceanic-agent || exit 1; \
    done && \
    mkdir -p /out/data

# ---- runtime: tiny, non-root, no shell ----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /src/dist/oceanic-server /app/oceanic-server
COPY --from=build /src/dist/agents /app/agents
# Pre-create /data owned by the nonroot user so a fresh named volume is writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV OCEANIC_LISTEN=:8080 \
    OCEANIC_DATA_DIR=/data \
    OCEANIC_AGENTS_DIR=/app/agents
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/app/oceanic-server", "healthcheck"]
ENTRYPOINT ["/app/oceanic-server"]
