#!/usr/bin/env bash
# build.sh — full production build: codegen → web → go binary.
# Produces bin/tether with embedded SPA.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "==> codegen"
bash scripts/codegen.sh

echo "==> web build"
(cd web && pnpm install --frozen-lockfile && pnpm build)

echo "==> go build"
go build -o bin/tether ./cmd/tether

echo "==> done: bin/tether"
