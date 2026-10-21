SHELL := /bin/bash
BINARY := bin/jq-mcp
EXT_DIR := extension
DIST_EXT := dist/jqhelper.zip

.PHONY: all build tidy test vet fmt run clean ext package ext-lint help install-service uninstall-service

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

## install-service: install a systemd --user unit that keeps the daemon running
install-service: build
	@mkdir -p $(HOME)/.config/systemd/user
	@sed 's|@BIN@|$(CURDIR)/$(BINARY)|' deploy/jq-mcp.service.in > $(HOME)/.config/systemd/user/jq-mcp.service
	@systemctl --user daemon-reload
	@echo "installed. enable it with: systemctl --user enable --now jq-mcp"

## uninstall-service: remove the systemd --user unit (does not stop the daemon)
uninstall-service:
	@rm -f $(HOME)/.config/systemd/user/jq-mcp.service
	@systemctl --user daemon-reload
	@echo "removed ~/.config/systemd/user/jq-mcp.service"

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
