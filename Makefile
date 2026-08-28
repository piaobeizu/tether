.PHONY: all build codegen test go-test web-test check-artifacts check-vendor \
        check-vendor-diff verify-vendor-upstream ci release clean

BINARY  := bin/tether
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

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
