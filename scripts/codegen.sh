#!/usr/bin/env bash
# codegen.sh — generate wire.gen.ts from internal/wire/types.go via tygo.
# Run via `make codegen`. CI checks drift with `git diff --exit-code` after.
set -euo pipefail

cd "$(dirname "$0")/.."

go tool tygo generate
