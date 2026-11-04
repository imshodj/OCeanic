VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/imshodj/oceanic/internal/protocol.AgentVersion=$(VERSION)
AGENTS := windows/amd64 windows/arm64 linux/amd64 linux/arm64 linux/arm darwin/arm64 darwin/amd64

.PHONY: all server agents run docker test

all: server agents

server:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/oceanic-server ./cmd/oceanic-server

agents:
	@mkdir -p dist/agents
	@for p in $(AGENTS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building agent $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch GOARM=7 go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/agents/oceanic-agent-$$os-$$arch$$ext ./cmd/oceanic-agent || exit 1; \
	done

run: all
	OCEANIC_DATA_DIR=./data OCEANIC_AGENTS_DIR=./dist/agents ./dist/oceanic-server

test:
	go vet ./... && GOOS=windows go vet ./... && go test ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t oceanic-rmm:latest .
