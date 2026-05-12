.PHONY: build build-server build-client build-node build-toolkit build-linux \
       rootfs download-kernel run-server smoke release clean \
       test test-smoke test-e2e

BIN_DIR := ./bin
GO := go

# AGPL § 13: bake the source URL, version, and commit into the server binary
# so /source reports the actually-running source. Override SOURCE_URL on the
# command line if you are building a fork.
SOURCE_URL ?= https://github.com/agent-testnet/agent-testnet
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
META_PKG   := github.com/agent-testnet/agent-testnet/server/controlplane
LDFLAGS    := -X '$(META_PKG).SourceURL=$(SOURCE_URL)' \
              -X '$(META_PKG).Version=$(VERSION)' \
              -X '$(META_PKG).Commit=$(COMMIT)'

build: build-server build-client build-node build-toolkit

build-server:
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/testnet-server ./cmd/testnet-server

build-client:
	$(GO) build -o $(BIN_DIR)/testnet-client ./cmd/testnet-client

build-node:
	$(GO) build -o $(BIN_DIR)/testnet-node ./cmd/testnet-node

build-toolkit:
	$(GO) build -o $(BIN_DIR)/testnet-toolkit ./cmd/testnet-toolkit

build-linux:
	docker build --network=host -f Dockerfile.build -t agent-testnet-builder .
	@mkdir -p build-linux
	@CONTAINER_ID=$$(docker create --entrypoint="" agent-testnet-builder /bin/true) && \
	  docker cp $$CONTAINER_ID:/testnet-server  build-linux/ && \
	  docker cp $$CONTAINER_ID:/testnet-client  build-linux/ && \
	  docker cp $$CONTAINER_ID:/testnet-node    build-linux/ && \
	  docker cp $$CONTAINER_ID:/testnet-toolkit build-linux/ && \
	  docker rm $$CONTAINER_ID >/dev/null
	@echo "Linux binaries in build-linux/"

download-kernel:
	@mkdir -p ~/.testnet/bin
	curl -sSL "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/vmlinux-5.10.223" \
		-o ~/.testnet/bin/vmlinux-5.10.bin
	@echo "Kernel saved to ~/.testnet/bin/vmlinux-5.10.bin"

rootfs:
	sudo bash scripts/gen-rootfs.sh

run-server:
	$(BIN_DIR)/testnet-server --config ./configs/server.yaml

smoke: build
	@echo "Running smoke test..."
	bash scripts/smoke-test.sh

release:
	bash scripts/build-release.sh

release-rootfs:
	bash scripts/build-release.sh --rootfs

test:
	$(GO) test ./...

test-smoke: build
	@echo "Running smoke test..."
	bash scripts/smoke-test.sh

test-e2e:
	@echo "Running AWS E2E test (requires AWS credentials)..."
	bash tests/e2e/aws-e2e-test.sh

clean:
	rm -rf $(BIN_DIR) build-linux/ dist/ data/
