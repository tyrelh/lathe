BIN := $(HOME)/.local/bin/lathe
# `make release` stamps VERSION into the binary. `make build` and `make install`
# report `dev`, which never refreshes existing seeded files in ~/.lathe.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build release install test fmt

build:
	go build -o $(BIN) .

release:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) .

install: build
	$(BIN) install

test:
	go test ./...

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
