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

# This line carried `CI=true pnpm install` from tether#81 until tether#86. The
# reason given was that the committed web/node_modules did not match
# pnpm-lock.yaml, so --frozen-lockfile wanted to purge the directory and pnpm
# refuses to do that without a TTY (ERR_PNPM_ABORTED_REMOVE_MODULES_DIR_NO_TTY),
# which broke every script- or agent-driven build.
#
# web/node_modules is no longer tracked, so there is nothing left to purge. Two
# things are worth writing down rather than leaving implied:
#
#  - The purge never depended on repo state alone. Whether pnpm wipes the tree
#    turns on the pnpm version recorded in node_modules/.modules.yaml versus the
#    one installed locally, and this repo pins neither: web/package.json has no
#    `packageManager` field and both workflows install a floating `pnpm@10`.
#  - The failure did not reproduce when the guard was removed. Measured on
#    2026-08-20 at dbdfb24 with the tree still committed, pnpm 10.33.0, no TTY:
#    `pnpm install --frozen-lockfile` without CI=true exited 0 in 1.3s, and again
#    in 643ms. With node_modules absent entirely — the state this script now
#    always sees on a fresh clone — it exited 0 in 1.5s. So this is not "the fix
#    made it safe"; the guard was already inert here, and now it has nothing to
#    guard. Whether it was ever live on some other machine's pnpm is not
#    recoverable from the repo.
echo "==> web build"
(cd web && pnpm install --frozen-lockfile && pnpm build)

# Must sit between the web build and the go build: it writes web/dist/BUILD-ID,
# which go:embed then compiles into the binary.
echo "==> stamp SPA bundle"
bash scripts/spa-bundle.sh stamp

echo "==> go build (version: ${VERSION})"
go build -ldflags "-X main.version=${VERSION}" -o bin/tether ./cmd/tether

echo "==> verify embedded SPA"
bash scripts/spa-bundle.sh check bin/tether

echo "==> done: bin/tether"
