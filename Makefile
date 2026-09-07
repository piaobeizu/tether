.PHONY: all build codegen test go-test web-test check-artifacts check-vendor \
        check-vendor-diff verify-vendor-upstream ci release clean

BINARY  := bin/tether
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# Make the make targets hermetic against an enclosing Go workspace (tether#177).
#
# This only bites when the checkout sits *inside* someone else's workspace, and
# in the polyforge workspace both checkouts do: a task worktree and the canonical
# .repo/<repo> clone each resolve GOWORK to the workspace-root go.work, which
# `use`s other repos there and not this module. It is not only the pf.* trees.
# `go` then resolves in workspace mode against a workspace this module is not
# part of, and every target that shells out to `go` fails:
#
#   make codegen  ->  go: no such tool "tygo"
#   make go-test  ->  pattern ./...: directory prefix . does not contain modules
#                     listed in go.work or their selected dependencies
#
# The codegen one is the trap: it names a *tool*, so it reads as a missing
# dependency and invites `go install .../tygo`. The tool is not missing — go.mod
# declares it and `GOWORK=off go tool` lists it. Single-module resolution is what
# is missing. This used to be a hand-maintained rule in two step templates, i.e.
# maintained by whoever remembered it.
#
# CI never invokes make (it runs scripts/*.sh and `go` directly), so this reaches
# local make targets only. In an *unmodified* clone `off` merely restates what
# `go` already concludes. Once you write your own go.work it no longer restates —
# it overrides — and the two shapes of workspace differ sharply:
#
#   use-only:     a benefit. The package set of `make go-test` stays exactly this
#                 module's, identical to CI's, instead of drifting with local
#                 workspace state. (You cannot `use` both this module and v0/
#                 regardless: v0/go.mod declares the same module path, so go
#                 rejects the workspace outright — "module
#                 github.com/piaobeizu/tether appears multiple times in
#                 workspace".)
#   with replace: SILENTLY IGNORED, and this is the case to know about. Pointing
#                 a dependency at a local fork is the usual reason to write a
#                 go.work, and every make target drops it: `make build` exits 0
#                 and links the upstream module, with no warning and no error.
#                 Measured by pointing a direct dependency at a local fork — the
#                 recipe resolves the version go.mod pins, a bare `go` in the
#                 same directory resolves the fork.
#
# `?=` yields to any outer value, so the way back in is to pass one explicitly:
#
#   GOWORK="$PWD/go.work" make build    # ABSOLUTE path required; a relative one
#                                       # dies with `invalid GOWORK: not an
#                                       # absolute path`
#
# `export` is load-bearing — a plain make variable is not in the recipe's
# environment, which is where `go` reads it. `?=` does not assign to a
# set-but-empty variable, so `GOWORK= make build` hands the recipe an empty
# GOWORK, `go` falls back to auto-discovery, and the original failure comes back.
# That is an unhandled edge, not a designed control: nothing exercises it.
#
# Scope: make targets only. A bare `go test ./...` in the shell is still on you.
export GOWORK ?= off

# `make build` is the only supported build. web/dist (the embedded SPA) is not in
# git, so until it has been built every go command in this module — including
# `go list ./...`, hence gopls — fails with "pattern all:dist: no matching files
# found". Run `make build` once after cloning. See web/embed.go for the why.

# Must stay in sync with .github/workflows/ci.yml. See the comment there for why
# -race and -count=1 are the baseline rather than an opt-in.
GOTEST  := go test -count=1 -race

all: build

build:
	VERSION="$(VERSION)" bash scripts/build.sh

codegen:
	bash scripts/codegen.sh

# check-vendor is in here and not only in `ci` because `make test` is what anyone
# actually runs before pushing, and a gate that only exists in CI is a gate you
# meet after the fact. It needs neither Go nor node, so it costs nothing here.
test: check-vendor go-test web-test

go-test:
	$(GOTEST) ./...

# Leaves web/test-results/{junit.xml,vitest.json} behind — every run, pass or
# fail. Read those instead of the terminal when a run goes red: this suite has a
# ~5% flake (tether#105) whose two sightings so far both lost the name of the
# failing test, once because the output had been piped into `grep`. The files
# survive that; they are gitignored and rewritten by the next run, so copy one
# aside before rerunning. web/vite.config.ts has the full record.
web-test:
	cd web && pnpm test

# Asserts web/dist and the tsc caches are absent from the git index and still
# ignored. Also run by CI — see the comment in scripts/check-artifacts-uncommitted.sh.
check-artifacts:
	bash scripts/check-artifacts-uncommitted.sh

# Asserts every file under web/src/vendor/cloudcli is still byte-identical to the
# upstream tag its provenance header names (tether#171). Offline. See
# docs/vendoring-cloudcli.md.
check-vendor:
	bash scripts/check-vendor-provenance.sh check

# The other half of the offline gate, and the half that reads the *change* rather
# than the state: a content hash that moved while its tag/sha/status did not is an
# in-place edit that was rehashed, which `check` is structurally unable to see
# (after the rehash the state is consistent). CI runs this per PR against the PR
# base; locally, BASE is whatever you are stacked on.
#
#   make check-vendor-diff BASE=@origin/main
#
# BASE and HEAD take '@<git-ref>' or a path to a manifest file.
BASE ?= @origin/main
check-vendor-diff:
	bash scripts/check-vendor-provenance.sh check-diff "$(BASE)" $(HEAD)

# The network half — "are the recorded hashes what upstream actually published at
# the recorded sha". Step 5 of an absorption; not a per-PR gate, because a gate
# that reddens when github.com has a bad afternoon is one people rerun past.
#
#   make verify-vendor-upstream CLONE=/tmp/ccui
verify-vendor-upstream:
	@test -n "$(CLONE)" || { echo "usage: make verify-vendor-upstream CLONE=<upstream-clone>"; exit 2; }
	bash scripts/check-vendor-provenance.sh verify-upstream "$(CLONE)"

# scripts/build.sh stamps web/dist and then verifies the binary against it
# (scripts/spa-bundle.sh), so the embed hop is covered here without a separate line.
#
# check-vendor needs neither Go nor node, so it sits with check-artifacts ahead of
# the builds: a broken vendor pin should be reported in seconds, not after a web
# build.
ci: codegen check-artifacts check-vendor
	git diff --exit-code web/src/lib/wire.gen.ts
	bash scripts/build.sh
	$(GOTEST) ./...
	cd web && pnpm test

release:
	bash scripts/release.sh

clean:
	rm -rf $(BINARY) dist/ release/
