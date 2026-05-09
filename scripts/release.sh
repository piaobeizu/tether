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

# Build web assets first.
(cd web && pnpm install --frozen-lockfile && pnpm build)

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

  # Permission hook binary (D-05b §4.2).
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
    go run ./cmd/build-hook "${OUTDIR}/tether-permission-hook${EXT}" 2>/dev/null || \
    echo "    [warn] hook build skipped for ${GOOS}/${GOARCH}"

  cp README.md "${OUTDIR}/"

  (cd release && tar czf "${NAME}.tar.gz" "${NAME}" && rm -rf "${NAME}")
  echo "  -> release/${NAME}.tar.gz"
done

echo "Done. Artifacts in release/"
