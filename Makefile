.PHONY: all build codegen test web-test ci release clean

BINARY  := bin/tether
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

all: build

build:
	bash scripts/build.sh

codegen:
	bash scripts/codegen.sh

test: go-test web-test

go-test:
	go test ./...

web-test:
	cd web && pnpm test

ci: codegen
	git diff --exit-code web/src/lib/wire.gen.ts
	bash scripts/build.sh
	go test ./...
	cd web && pnpm test

release:
	bash scripts/release.sh

clean:
	rm -rf $(BINARY) dist/ release/
