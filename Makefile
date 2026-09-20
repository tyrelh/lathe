BIN := $(HOME)/.local/bin/lathe
# Stamped into the binary and shown in the dashboard's top bar. An unstamped
# build reports `dev`, which is the honest answer for one built from a checkout.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build release install test

build:
	go build -o $(BIN) .

release:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) .

install: build
	$(BIN) install

test:
	go test ./...
