GO ?= go
BIN := bin/natflow-collector

.PHONY: all build test vet fmt tidy run clean

all: build

build:
	./scripts/build.sh

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

run: build
	$(BIN) --config configs/collector.yaml

clean:
	rm -rf bin
