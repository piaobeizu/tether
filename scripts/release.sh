#!/usr/bin/env bash
# release.sh — cross-compile tether + permission hook for 5 platforms.
# Usage: bash scripts/release.sh [version]
# Output: release/*.tar.gz, one per platform, each holding the tether binary,
#         the permission hook + its .hash sidecar, and README.md.
set -euo pipefail

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
echo "Building tether ${VERSION}"

# Defines PLATFORMS. Shared with the CI cross-compile gate so this loop cannot
# name a platform that nothing ever compiles for (tether#85).
source "$(dirname "$0")/platforms.sh"

# Build web assets first. CI=true because the committed web/node_modules does not
# match pnpm-lock.yaml, so --frozen-lockfile purges it and pnpm refuses to do that
# without a TTY (ERR_PNPM_ABORTED_REMOVE_MODULES_DIR_NO_TTY). Same reason as in
# scripts/build.sh.
(cd web && CI=true pnpm install --frozen-lockfile && pnpm build)

# Stamp before any go build so every cross-compiled artifact carries the build id.
bash scripts/spa-bundle.sh stamp

mkdir -p dist release

# cmd/build-hook is a wrapper that shells out to `go build`, so it has to run on
# this machine — the GOOS/GOARCH below belong on the build it launches, not on
# the wrapper itself. Building it once, for the host, is what makes that split
# possible. Same reasoning as the release workflow's hook step.
#
# Until tether#85 this loop ran `GOOS=... go run ./cmd/build-hook`, which
# cross-compiled the wrapper and then tried to execute it: "fork/exec ...:
# exec format error" on every platform except the host's. A `|| echo "[warn]
# hook build skipped"` turned that into one line of output, so every tarball
# this script produced except the host's had no permission hook in it and the
# loop carried on as though it had one. Confined to this script — the published
# releases come from .github/workflows/release.yml, which has built the wrapper
# for the host since 976a703, before the first release run.
HOOK_TMP="$(mktemp -d)"
trap 'rm -rf "${HOOK_TMP}"' EXIT
HOOK_BUILDER="${HOOK_TMP}/build-hook"
CGO_ENABLED=0 go build -o "${HOOK_BUILDER}" ./cmd/build-hook

for PLATFORM in "${PLATFORMS[@]}"; do
  GOOS="${PLATFORM%/*}"
  GOARCH="${PLATFORM#*/}"
  EXT=""
  [[ "$GOOS" == "windows" ]] && EXT=".exe"

  NAME="tether-${VERSION}-${GOOS}-${GOARCH}"
  OUTDIR="release/${NAME}"
  mkdir -p "${OUTDIR}"

  echo "  building ${NAME}..."

  # Main binary.
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
    go build -ldflags="-s -w -X main.version=${VERSION}" \
    -o "${OUTDIR}/tether${EXT}" ./cmd/tether

  # These are the binaries people download, so they get the same embed check the
  # local build gets. Works on cross-compiled output: -s -w strips symbols and
  # DWARF, not embedded data.
  bash scripts/spa-bundle.sh check "${OUTDIR}/tether${EXT}"

  # Permission hook binary (D-05b §4.2). No `|| warn` fallback: a tarball whose
  # hook is missing is a tarball that cannot gate tool calls, which is worth
  # failing the release over.
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
    "${HOOK_BUILDER}" "${OUTDIR}/tether-permission-hook${EXT}"

  cp README.md "${OUTDIR}/"

  (cd release && tar czf "${NAME}.tar.gz" "${NAME}" && rm -rf "${NAME}")
  echo "  -> release/${NAME}.tar.gz"
done

echo "Done. Artifacts in release/"
