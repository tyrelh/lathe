BIN := $(HOME)/.local/bin/lathe

.PHONY: build install test

build:
	go build -o $(BIN) .

install: build
	$(BIN) install

test:
	go test ./...
