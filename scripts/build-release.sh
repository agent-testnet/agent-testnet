#!/usr/bin/env bash
#
# Build release artifacts for all platforms.
#
# Produces:
#   dist/testnet-server-linux-amd64
#   dist/testnet-client-linux-amd64
#   dist/testnet-node-linux-amd64
#   dist/testnet-toolkit-linux-amd64
#   dist/testnet-server-linux-arm64
#   dist/testnet-client-linux-arm64
#   dist/testnet-node-linux-arm64
#   dist/testnet-toolkit-linux-arm64
#   dist/install.sh
#
# If --rootfs is passed and running on Linux, also builds:
#   dist/rootfs-agent-amd64.ext4.gz
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
DIST_DIR="${PROJECT_DIR}/dist"

BUILD_ROOTFS=false
if [[ "${1:-}" == "--rootfs" ]]; then
    BUILD_ROOTFS=true
fi

rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

# ---- Build metadata (AGPL § 13: /source must report the running source) ----
# Override SOURCE_URL when building a fork so users of the network service
# can reach *your* source, not ours.
SOURCE_URL="${SOURCE_URL:-https://github.com/agent-testnet/agent-testnet}"
VERSION="${VERSION:-$(git -C "$PROJECT_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git -C "$PROJECT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
META_PKG="github.com/agent-testnet/agent-testnet/server/controlplane"
META_LDFLAGS="-X '${META_PKG}.SourceURL=${SOURCE_URL}' -X '${META_PKG}.Version=${VERSION}' -X '${META_PKG}.Commit=${COMMIT}'"

# ---- Cross-compile binaries ----

build_binaries() {
    local arch="$1"
    echo "==> Building linux/${arch} binaries (version=${VERSION}, commit=${COMMIT})..."

    for bin in testnet-server testnet-client testnet-node testnet-toolkit; do
        local cmd_dir="./cmd/${bin}"
        local out="${DIST_DIR}/${bin}-linux-${arch}"
        local ldflags="-s -w"
        if [[ "$bin" == "testnet-server" ]]; then
            ldflags="${ldflags} ${META_LDFLAGS}"
        fi
        CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
            go build -ldflags="${ldflags}" -o "$out" "$cmd_dir"
        echo "    ${out}"
    done
}

cd "$PROJECT_DIR"

if command -v go >/dev/null 2>&1; then
    build_binaries amd64
    build_binaries arm64
else
    echo "==> Go not found locally, building via Docker..."
    docker build -f Dockerfile.build -t agent-testnet-builder .
    CONTAINER_ID=$(docker create --entrypoint="" agent-testnet-builder /bin/true)
    for bin in testnet-server testnet-client testnet-node testnet-toolkit; do
        docker cp "${CONTAINER_ID}:/${bin}" "${DIST_DIR}/${bin}-linux-amd64"
    done
    docker rm "$CONTAINER_ID" >/dev/null
    echo "    Built amd64 binaries via Docker (arm64 skipped)"
fi

# ---- Copy install script ----

cp "${PROJECT_DIR}/deploy/install.sh" "${DIST_DIR}/install.sh"
chmod +x "${DIST_DIR}/install.sh"
echo "==> Copied install.sh"

# ---- Build rootfs (optional, Linux only) ----

if $BUILD_ROOTFS; then
    if [[ "$(uname)" != "Linux" ]]; then
        echo "==> Skipping rootfs build (requires Linux)"
    else
        echo "==> Building rootfs..."
        sudo bash "${PROJECT_DIR}/scripts/gen-rootfs.sh"

        ROOTFS_SRC="/tmp/testnet-rootfs/rootfs.ext4"
        ROOTFS_DST="${DIST_DIR}/rootfs-agent-amd64.ext4.gz"
        echo "    Compressing rootfs..."
        gzip -c "$ROOTFS_SRC" > "$ROOTFS_DST"
        echo "    ${ROOTFS_DST} ($(du -sh "$ROOTFS_DST" | cut -f1))"
    fi
fi

# ---- Summary ----

echo ""
echo "==> Release artifacts in ${DIST_DIR}/:"
ls -lh "$DIST_DIR/"
echo ""
echo "To create a GitHub release:"
echo "  gh release create vX.Y.Z dist/* --title 'vX.Y.Z' --notes 'Release notes'"
