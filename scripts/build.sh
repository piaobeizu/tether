#!/usr/bin/env bash
# build.sh — full production build: codegen → web → go binary.
# Produces bin/tether with embedded SPA.
#
# This is the only supported way to build tether. web/dist is not in git
# (tether#81), so a bare `go build` either fails outright on a fresh checkout or
# embeds whatever bundle a previous build left in web/dist — which is how a deploy
# came within one step of serving a stale SPA against a current backend. The stamp
# and verify steps below make every build state, out loud, which SPA is inside the
# binary it just produced.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

echo "==> codegen"
bash scripts/codegen.sh

# CI=true is not cosmetic. web/node_modules is committed and does not match
# pnpm-lock.yaml, so --frozen-lockfile wants to purge and reinstall it; without a
# TTY pnpm refuses ("ERR_PNPM_ABORTED_REMOVE_MODULES_DIR_NO_TTY") and this script
# exits non-zero on any fresh checkout driven by a script or an agent. Since
# `make build` is now the only supported build and the documented fix for a fresh
# clone, it has to work in a non-interactive shell.
echo "==> web build"
(cd web && CI=true pnpm install --frozen-lockfile && pnpm build)

# Must sit between the web build and the go build: it writes web/dist/BUILD-ID,
# which go:embed then compiles into the binary.
echo "==> stamp SPA bundle"
bash scripts/spa-bundle.sh stamp

echo "==> go build (version: ${VERSION})"
go build -ldflags "-X main.version=${VERSION}" -o bin/tether ./cmd/tether

echo "==> verify embedded SPA"
bash scripts/spa-bundle.sh check bin/tether

echo "==> done: bin/tether"
