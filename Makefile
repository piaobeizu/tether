.PHONY: all build codegen test clean

BINARY := bin/tether
CMD    := ./cmd/tether

all: build

build:
	bash scripts/build.sh

codegen:
	bash scripts/codegen.sh

test:
	go test ./...

clean:
	rm -f $(BINARY)
