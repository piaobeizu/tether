#!/usr/bin/env bash
# build.sh — full production build: codegen → web → go binary.
# Produces bin/tether with embedded SPA.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

echo "==> codegen"
bash scripts/codegen.sh

echo "==> web build"
(cd web && pnpm install --frozen-lockfile && pnpm build)

echo "==> go build (version: ${VERSION})"
go build -ldflags "-X main.version=${VERSION}" -o bin/tether ./cmd/tether

echo "==> done: bin/tether"
