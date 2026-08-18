#!/usr/bin/env bash
# check-artifacts-uncommitted.sh — assert generated frontend artifacts are not in git.
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
# The `:(glob)` prefix is load-bearing. git's two glob dialects disagree about `/`:
# in .gitignore a `*` never crosses a slash, but in a plain pathspec it does. Without
# `:(glob)` this list would be strictly broader than the .gitignore rule it is
# supposed to enforce, and would fail on vendored files inside the (committed)
# web/node_modules — e.g. tldts ships its own dist/cjs/tsconfig.tsbuildinfo. That is
# not a hypothetical; it turned CI red the first time this ran.
#
# web/test-results is the vitest junit/json report pair (tether#105). It is in
# the same class — generated per run, describing that run — and it is written by
# `pnpm test`, which runs far more often than `pnpm build`, so it is the entry
# most likely to be swept up by a careless `git add -A`.
INDEX_FORBIDDEN=(web/dist ':(glob)web/*.tsbuildinfo' web/test-results)
MUST_BE_IGNORED=(web/dist/index.html web/tsconfig.app.tsbuildinfo web/test-results/junit.xml)

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
	echo "Generated frontend output must stay out of git. Build it with 'make build';" >&2
	echo "if you need to inspect what a binary carries, use" >&2
	echo "scripts/spa-bundle.sh print <binary>." >&2
	exit 1
fi

echo "OK: no generated frontend artifacts tracked; ignore rules in effect"
