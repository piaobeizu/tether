.PHONY: all build codegen test clean

BINARY := bin/tether
CMD    := ./cmd/tether

all: codegen build

build:
	go build -o $(BINARY) $(CMD)

codegen:
	bash scripts/codegen.sh

test:
	go test ./...

clean:
	rm -f $(BINARY)
