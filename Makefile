.PHONY: all build codegen test go-test web-test check-artifacts ci release clean

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

test: go-test web-test

go-test:
	$(GOTEST) ./...

web-test:
	cd web && pnpm test

# Asserts web/dist and the tsc caches are absent from the git index and still
# ignored. Also run by CI — see the comment in scripts/check-artifacts-uncommitted.sh.
check-artifacts:
	bash scripts/check-artifacts-uncommitted.sh

# scripts/build.sh stamps web/dist and then verifies the binary against it
# (scripts/spa-bundle.sh), so the embed hop is covered here without a separate line.
ci: codegen check-artifacts
	git diff --exit-code web/src/lib/wire.gen.ts
	bash scripts/build.sh
	$(GOTEST) ./...
	cd web && pnpm test

release:
	bash scripts/release.sh

clean:
	rm -rf $(BINARY) dist/ release/
