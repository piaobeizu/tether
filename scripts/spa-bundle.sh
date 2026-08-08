#!/usr/bin/env bash
# spa-bundle.sh — tie a tether binary to the exact SPA build it embeds.
#
# Usage:
#   scripts/spa-bundle.sh stamp             # after `pnpm build`, before `go build`
#   scripts/spa-bundle.sh check <binary>    # assert the binary embeds this web/dist
#   scripts/spa-bundle.sh print <binary>    # report what a binary carries
#
# Why this exists (tether#81): web/dist is compiled into the binary at build time
# by web/embed.go, and a finished binary otherwise says nothing about which build
# of the SPA is inside it. A binary built from a stale dist is byte-for-byte
# plausible and passes every test — the way that bug was actually found was
# grepping a deployed binary for an asset hash. This makes that a build step.
#
# How it can tell. `stamp` writes web/dist/BUILD-ID, a "tether-spa-<sha256>" token
# derived from the content of every other file in web/dist, so `go build` compiles
# it in along with the rest of the bundle. `check` recomputes that token from disk
# and requires it to be present in the binary. Content, not file names: vite's
# hashed names only cover web/src, so editing web/index.html or web/public/*
# changes the bundle while every asset name stays the same. (That was the first
# version of this script, and review broke it with exactly that case.)
#
# The name-level comparison is kept as well, because it produces a far more useful
# message than "the token is missing" — but the token is the authority.
set -euo pipefail
cd "$(dirname "$0")/.."

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 | cut -d' ' -f1
	else
		echo "FAIL: neither sha256sum nor shasum found; cannot hash the bundle" >&2
		exit 1
	fi
}

# Every file in web/dist as an embed path ("dist/..."), BUILD-ID itself excluded.
dist_files() {
	find web/dist -type f ! -name BUILD-ID | LC_ALL=C sort | sed 's|^web/||'
}

require_dist() {
	if [[ ! -d web/dist ]]; then
		echo "FAIL: web/dist does not exist. Run 'make build' — a bare 'go build'" >&2
		echo "      cannot produce it, and will not even compile without it." >&2
		exit 1
	fi
	if [[ -z "$(dist_files)" ]]; then
		echo "FAIL: web/dist contains no files, so there is nothing to compare. A go" >&2
		echo "      build against an empty dist does not compile either ('contains no" >&2
		echo "      embeddable files'), so this means the web build never ran." >&2
		exit 1
	fi
	if [[ ! -f web/dist/index.html ]]; then
		echo "FAIL: web/dist/index.html is missing. A binary built from this directory" >&2
		echo "      compiles, then answers 500 'index.html not found' on every page" >&2
		echo "      request (internal/server/static.go). Run 'make build'." >&2
		exit 1
	fi
}

compute_id() {
	local manifest
	manifest="$(dist_files | while IFS= read -r p; do
		printf '%s  %s\n' "$(sha256 <"web/$p")" "$p"
	done)"
	printf 'tether-spa-%s' "$(printf '%s' "$manifest" | sha256)"
}

# Asset paths as go:embed stores them. Anchoring on the extension keeps each match
# from running into the next string in the binary's (unseparated) string data.
embedded_assets() {
	grep -a -o -E 'dist/assets/[A-Za-z0-9._-]+\.(js|css)' "$1" | sort -u || true
}

embedded_id() {
	grep -a -o -E 'tether-spa-[0-9a-f]{64}' "$1" | sort -u || true
}

cmd="${1:-}"
case "$cmd" in
stamp)
	require_dist
	compute_id >web/dist/BUILD-ID
	echo "==> stamped web/dist/BUILD-ID = $(cat web/dist/BUILD-ID)"
	;;

print)
	bin="${2:-}"
	[[ -f "$bin" ]] || {
		echo "FAIL: '$bin' is not a file" >&2
		exit 1
	}
	echo "SPA embedded in $bin:"
	ids="$(embedded_id "$bin")"
	if [[ -z "$ids" ]]; then
		echo "  build id: (none — built without 'spa-bundle.sh stamp', so this binary"
		echo "            cannot be tied to a specific bundle; compare asset names below)"
	else
		sed 's/^/  build id: /' <<<"$ids"
	fi
	assets="$(embedded_assets "$bin")"
	if [[ -z "$assets" ]]; then
		echo "  assets:   (no dist/assets/*.{js,css} names found — either no SPA is"
		echo "            embedded, or vite's assetsDir is no longer 'assets')"
	else
		sed 's/^/  asset:    /' <<<"$assets"
	fi
	;;

check)
	bin="${2:-}"
	[[ -f "$bin" ]] || {
		echo "FAIL: '$bin' is not a file" >&2
		exit 1
	}
	require_dist

	if [[ ! -f web/dist/BUILD-ID ]]; then
		echo "FAIL: web/dist/BUILD-ID is missing, so this binary cannot be tied to a" >&2
		echo "      bundle. It is written by 'scripts/spa-bundle.sh stamp', which" >&2
		echo "      'make build' runs between the web build and the go build." >&2
		exit 1
	fi
	want="$(compute_id)"
	stamped="$(cat web/dist/BUILD-ID)"
	if [[ "$want" != "$stamped" ]]; then
		echo "FAIL: web/dist changed after it was stamped." >&2
		echo "      BUILD-ID says     $stamped" >&2
		echo "      contents hash to  $want" >&2
		echo "      Something wrote into web/dist after the build. Re-run 'make build'." >&2
		exit 1
	fi

	fail=0
	if ! grep -a -q -F -- "$want" "$bin"; then
		echo "FAIL: $bin does not embed this bundle." >&2
		echo "      expected build id $want" >&2
		got="$(embedded_id "$bin")"
		if [[ -z "$got" ]]; then
			echo "      the binary carries no build id at all — it was built from an" >&2
			echo "      unstamped web/dist, i.e. not by 'make build'." >&2
		else
			sed 's/^/      binary carries    /' <<<"$got" >&2
		fi
		fail=1
	fi

	# Name-level cross-check. Kept only because it says *which* file is wrong; the
	# build id above is what actually decides.
	expected_files="$(dist_files)"
	while IFS= read -r path; do
		[[ -z "$path" ]] && continue
		if ! grep -a -q -F -- "$path" "$bin"; then
			echo "FAIL: '$path' is in web/dist but NOT embedded in $bin" >&2
			fail=1
		fi
	done <<<"$expected_files"

	expected_assets="$(grep -E '^dist/assets/.*\.(js|css)$' <<<"$expected_files" | sort -u || true)"
	if [[ -z "$expected_assets" ]]; then
		# Said out loud rather than skipped in silence: with no assets on disk the
		# loop below would call every embedded asset name "extra", so it is turned
		# off here — and anyone reading the output needs to know that happened.
		echo "note: web/dist has no dist/assets/*.{js,css} files, so the stale-asset" >&2
		echo "      cross-check does not apply here (did vite's assetsDir change?)." >&2
		echo "      The BUILD-ID check above still covers the whole bundle." >&2
	else
		while IFS= read -r asset; do
			[[ -z "$asset" ]] && continue
			if ! grep -q -x -F -- "$asset" <<<"$expected_assets"; then
				echo "FAIL: $bin embeds '$asset', which is not in web/dist — this binary" >&2
				echo "      was built from a different (stale) SPA build." >&2
				fail=1
			fi
		done <<<"$(embedded_assets "$bin")"
	fi

	if [[ "$fail" -ne 0 ]]; then
		echo >&2
		echo "web/dist on disk:" >&2
		sed 's/^/  /' <<<"$expected_files" >&2
		echo >&2
		echo "Rebuild with 'make build' (pnpm build → stamp → go build); a bare" >&2
		echo "'go build' embeds whatever happens to be sitting in web/dist." >&2
		exit 1
	fi

	echo "OK: $bin embeds the SPA currently in web/dist"
	echo "    build id $want"
	;;

*)
	echo "usage: scripts/spa-bundle.sh {stamp | check <binary> | print <binary>}" >&2
	exit 2
	;;
esac
