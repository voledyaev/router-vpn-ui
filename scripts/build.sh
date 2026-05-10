#!/usr/bin/env bash
# Builds the installer binary for the local platform.
# Steps:
#   1. Cross-compile yonderd (router daemon) for linux/arm64 into the embed slot.
#   2. Build yonder (installer) for the host platform, picking up the embedded yonderd.
#
# Run from repo root: ./scripts/build.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

EMBED_DIR="cmd/installer/embed"
mkdir -p "$EMBED_DIR"

echo "[build] cross-compiling yonderd for linux/arm64..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -ldflags="-s -w" \
    -o "$EMBED_DIR/yonderd-linux-arm64" \
    ./cmd/router-app

echo "[build] building yonder installer for $(go env GOOS)/$(go env GOARCH)..."
go build -trimpath -ldflags="-s -w" -o yonder ./cmd/installer

echo "[build] done: ./yonder"
ls -lh yonder "$EMBED_DIR/yonderd-linux-arm64"
