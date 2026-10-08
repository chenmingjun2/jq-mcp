SHELL := /bin/bash
BINARY := bin/jq-mcp
EXT_DIR := extension
DIST_EXT := dist/jqhelper.zip

.PHONY: all build tidy test vet fmt run clean ext package ext-lint help

all: build

## build: compile the Go MCP server into ./bin/jq-mcp
build:
	@mkdir -p bin
	go build -trimpath -o $(BINARY) ./cmd/jq-mcp

## tidy: sync go.mod/go.sum
tidy:
	go mod tidy

## fmt: format Go sources
fmt:
	gofmt -w ./cmd ./internal

## vet: run go vet
vet:
	go vet ./...

## test: run Go unit tests
test:
	go test ./...

## run: run the server (attach mode by default)
run: build
	$(BINARY)

## clean: remove build artifacts
clean:
	rm -rf bin dist

## package: zip the unpacked extension for distribution
package:
	@mkdir -p dist
	cd $(EXT_DIR) && zip -qr ../$(DIST_EXT) . -x '*.zip' 'default-config.json' '*.log'
	@echo "packaged -> $(DIST_EXT)"

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
