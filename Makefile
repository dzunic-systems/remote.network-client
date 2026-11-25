GO ?= go
BIN := bin
DIST ?= dist

.PHONY: all build test fmt vet tidy release

all: fmt vet test build

LDFLAGS = -X remote.network/client/internal/cli.Version=$(VERSION)

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/remote-client ./cmd/remote-client

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

# Client releases are tagged client-vX.Y.Z in the private tree (node
# releases keep plain vX.Y.Z); the public repo gets plain vX.Y.Z.
VERSION ?= $(shell git describe --tags --match 'client-v*' --always --dirty 2>/dev/null | sed 's/^client-//' || echo dev)
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

# Release artifacts: static, cgo-free, plus checksums.txt; each binary twice — the versioned
# name a release keeps and the stable name the installer downloads from
# releases/latest/download (the node agent's layout).
release:
	mkdir -p $(DIST)
	for plat in $(PLATFORMS); do \
		os=$${plat%/*}; arch=$${plat#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/remote-client-$(VERSION)-$$os-$$arch$$ext ./cmd/remote-client; \
		cp $(DIST)/remote-client-$(VERSION)-$$os-$$arch$$ext $(DIST)/remote-client-$$os-$$arch$$ext; \
	done
	cd $(DIST) && shasum -a 256 remote-client-* > checksums.txt
