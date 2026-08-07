#!/usr/bin/env bash
# release.sh — cross-compile tether + permission hook for 5 platforms.
# Usage: bash scripts/release.sh [version]
# Output: dist/*.tar.gz
set -euo pipefail

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
echo "Building tether ${VERSION}"

PLATFORMS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
)

# Build web assets first. CI=true because the committed web/node_modules does not
# match pnpm-lock.yaml, so --frozen-lockfile purges it and pnpm refuses to do that
# without a TTY (ERR_PNPM_ABORTED_REMOVE_MODULES_DIR_NO_TTY). Same reason as in
# scripts/build.sh.
(cd web && CI=true pnpm install --frozen-lockfile && pnpm build)

# Stamp before any go build so every cross-compiled artifact carries the build id.
bash scripts/spa-bundle.sh stamp

mkdir -p dist release

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

  # Permission hook binary (D-05b §4.2).
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
    go run ./cmd/build-hook "${OUTDIR}/tether-permission-hook${EXT}" 2>/dev/null || \
    echo "    [warn] hook build skipped for ${GOOS}/${GOARCH}"

  cp README.md "${OUTDIR}/"

  (cd release && tar czf "${NAME}.tar.gz" "${NAME}" && rm -rf "${NAME}")
  echo "  -> release/${NAME}.tar.gz"
done

echo "Done. Artifacts in release/"
