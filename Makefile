.PHONY: all build codegen test go-test web-test ci release clean

BINARY  := bin/tether
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

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

ci: codegen
	git diff --exit-code web/src/lib/wire.gen.ts
	bash scripts/build.sh
	$(GOTEST) ./...
	cd web && pnpm test

release:
	bash scripts/release.sh

clean:
	rm -rf $(BINARY) dist/ release/
