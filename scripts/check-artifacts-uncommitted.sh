#!/usr/bin/env bash
# check-artifacts-uncommitted.sh — assert build artifacts are not in git.
#
# Scope note: this started as a frontend-only gate (tether#81) and now also covers
# pnpm's install tree, the compiled build-hook and release/ (tether#86). The shared
# invariant is not "frontend" — it is "a file produced by a build, which no one
# decided to track, and which therefore has no mechanism keeping it current".
#
# Why this exists (tether#81): web/dist is `vite build` output that gets compiled
# into the binary by web/embed.go. It used to be committed, so there were two
# sources for the same SPA and nothing kept them equal — one commit changed
# web/src without rebuilding dist, and from then on a bare `go build`/`go install`
# would have produced a binary serving the previous SPA. Tests, CI and even a live
# check that "the backend really sent the frontend a frame" all stayed green; the
# mismatch was found by grepping a binary for an asset hash.
#
# .gitignore alone does not hold that invariant: `git add -f` bypasses it, and a
# future edit can simply delete the ignore line. This script is the gate that
# turns either of those into a red build.
set -euo pipefail
cd "$(dirname "$0")/.."

# Paths that must never appear in the git index, and one representative file per
# path that must be reported as ignored (so a deleted .gitignore entry is caught
# even before anyone re-adds the files).
#
# The `:(glob)` prefix is load-bearing, and its job outlived the situation that
# first exposed it. git's two glob dialects disagree about `/`: in .gitignore a `*`
# never crosses a slash, but in a plain pathspec it does. Without `:(glob)` this
# entry would be strictly broader than the .gitignore rule it enforces. It first
# showed up as a CI failure on vendored files inside web/node_modules, which used
# to be committed — tldts ships its own dist/cjs/tsconfig.tsbuildinfo. That tree is
# no longer in the index (tether#86), so `git ls-files` no longer surfaces those
# files; keep the prefix anyway, because `git ls-files` still lists anything
# force-added, and the broad form would blame /web/*.tsbuildinfo for a nested file
# that the web/node_modules entry below is the correct rule for.
#
# web/test-results is the vitest junit/json report pair (tether#105). It is in
# the same class — generated per run, describing that run — and it is written by
# `pnpm test`, which runs far more often than `pnpm build`, so it is the entry
# most likely to be swept up by a careless `git add -A`.
#
# The last three are tether#86. They are not "generated frontend output" like the
# rest, but they are the same invariant — an artifact nobody decided to track —
# and this is the only gate in the repo that enforces it:
#   web/node_modules  pnpm's install tree, committed by accident on 2026-05-09.
#                     While tracked, one `pnpm install --frozen-lockfile` left
#                     7264 dirty entries, so every build's `git describe --dirty`
#                     version stamp was untrustworthy.
#   build-hook        a compiled copy of ./cmd/build-hook. Both callers build the
#                     wrapper to their own temp path, so the committed binary had
#                     no reader — the kind of file that goes stale invisibly.
#   release           scripts/release.sh's tarball output.
INDEX_FORBIDDEN=(web/dist ':(glob)web/*.tsbuildinfo' web/test-results web/node_modules build-hook release)
MUST_BE_IGNORED=(web/dist/index.html web/tsconfig.app.tsbuildinfo web/test-results/junit.xml web/node_modules/.modules.yaml build-hook release/tether-linux-amd64.tar.gz)

fail=0
all_tracked=""

for pathspec in "${INDEX_FORBIDDEN[@]}"; do
	# `git ls-files` exits 0 with empty output when nothing matches, so test the
	# output, not the exit code. Assigning first (no pipe) keeps set -e honest and
	# avoids taking the exit status of a downstream filter.
	tracked=$(git ls-files -- "$pathspec")
	if [[ -n "$tracked" ]]; then
		echo "FAIL: generated artifacts are tracked in git under '$pathspec':" >&2
		sed 's/^/  /' <<<"$tracked" >&2
		all_tracked+="$tracked"$'\n'
		fail=1
	fi
done

for f in "${MUST_BE_IGNORED[@]}"; do
	# git deliberately reports a *tracked* path as not-ignored, so this check would
	# otherwise fire for a `git add -f`'d file and blame .gitignore, which is intact
	# in that case. The loop above already owns that failure.
	if grep -q -x -F -- "$f" <<<"$all_tracked"; then
		continue
	fi
	if ! git check-ignore -q -- "$f"; then
		echo "FAIL: '$f' is not gitignored — the .gitignore entry that keeps build" >&2
		echo "      output out of git was weakened or removed. See .gitignore and" >&2
		echo "      web/embed.go for why it has to stay." >&2
		fail=1
	fi
done

if [[ "$fail" -ne 0 ]]; then
	echo >&2
	echo "Build output must stay out of git. Produce it with 'make build' (which runs" >&2
	echo "pnpm install itself, so web/node_modules needs no committed copy); if you" >&2
	echo "need to inspect what a binary carries, use scripts/spa-bundle.sh print" >&2
	echo "<binary>." >&2
	exit 1
fi

echo "OK: no build artifacts tracked; ignore rules in effect"
