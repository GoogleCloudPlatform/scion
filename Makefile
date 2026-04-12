# Scion Makefile
# Run 'make help' to see available targets.

BINARY        := scion
BUILD_DIR     := ./build
CONTAINER_DIR := ./.build/container
INSTALL_DIR   := $(HOME)/.local/bin
MAIN_PKG      := ./cmd/scion
LDFLAGS            := $(shell ./hack/version.sh)
SCIONTOOL_LDFLAGS  := $(shell ./hack/version.sh github.com/GoogleCloudPlatform/scion/cmd/sciontool/commands)
CONTAINER_OS  := linux
CONTAINER_ARCH := $(shell if [ "$$(uname -m)" = "x86_64" ]; then echo amd64; else echo arm64; fi)
GOLANGCI_LINT := $(shell command -v golangci-lint 2>/dev/null || echo $(shell go env GOPATH)/bin/golangci-lint)

.DEFAULT_GOAL := help

.PHONY: all build install test test-fast vet lint golangci-lint web web-typecheck fmt fmt-check ci ci-full clean help container-sciontool container-scion container-binaries ko-build-local

## all: Build the web frontend, then compile the Go binary with embedded assets
all: web install

## build: Compile the scion binary into ./build/
build:
	@echo "Building $(BINARY)..."
	@mkdir -p $(BUILD_DIR)
	@go build -buildvcs=false -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) $(MAIN_PKG)
	@echo "Binary: $(BUILD_DIR)/$(BINARY)"

## install: Build and install the binary to ~/.local/bin
install: build
	@echo "Installing $(BINARY) to $(INSTALL_DIR)..."
	@mkdir -p $(INSTALL_DIR)
	@install $(BUILD_DIR)/$(BINARY) $(INSTALL_DIR)/$(BINARY)
	@echo "Installed: $(INSTALL_DIR)/$(BINARY)"

## test: Run all tests
test:
	@echo "Running tests..."
	@go test ./...

## test-fast: Run tests without SQLite (lower memory usage)
test-fast:
	@echo "Running tests (no SQLite)..."
	@go test -tags no_sqlite ./...

## vet: Run go vet
vet:
	@go vet ./...

## lint: Run go vet (no SQLite, memory-safe)
lint:
	@go vet -tags no_sqlite ./...

## golangci-lint: Run golangci-lint on new issues only (install via: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest)
golangci-lint:
	@if [ ! -x "$(GOLANGCI_LINT)" ]; then \
		echo "ERROR: golangci-lint not found. Install with: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		exit 1; \
	fi
	@echo "Running golangci-lint (new issues vs main)..."
	@GOGC=50 $(GOLANGCI_LINT) run --new-from-rev=main ./...
	@echo "golangci-lint passed."

## web: Build the web frontend
web:
	@echo "Building web frontend..."
	@cd web && npm install && npm run build
	@echo "Web frontend built."

## container-sciontool: Cross-compile sciontool for Linux containers
container-sciontool:
	@echo "Building sciontool for $(CONTAINER_OS)/$(CONTAINER_ARCH)..."
	@mkdir -p $(CONTAINER_DIR)
	@GOOS=$(CONTAINER_OS) GOARCH=$(CONTAINER_ARCH) CGO_ENABLED=0 \
		go build -buildvcs=false -ldflags "$(SCIONTOOL_LDFLAGS)" \
		-o $(CONTAINER_DIR)/sciontool ./cmd/sciontool
	@echo "Built: $(CONTAINER_DIR)/sciontool"

## container-scion: Cross-compile scion CLI for Linux containers
container-scion:
	@echo "Building scion for $(CONTAINER_OS)/$(CONTAINER_ARCH)..."
	@mkdir -p $(CONTAINER_DIR)
	@GOOS=$(CONTAINER_OS) GOARCH=$(CONTAINER_ARCH) CGO_ENABLED=0 \
		go build -buildvcs=false -tags no_embed_web -ldflags "$(LDFLAGS)" \
		-o $(CONTAINER_DIR)/scion ./cmd/scion
	@echo "Built: $(CONTAINER_DIR)/scion"

## container-binaries: Build both scion and sciontool for Linux containers
container-binaries: container-sciontool container-scion
	@echo ""
	@echo "Dev binaries ready in $(CONTAINER_DIR)/"
	@echo "Usage: export SCION_DEV_BINARIES=$(CONTAINER_DIR)"

## ko-build-local: Build the hub container image locally with ko (loads into local Docker)
ko-build-local: web
	@echo "Building hub container image with ko..."
	@VERSION=$$(git describe --tags --exact-match 2>/dev/null || echo "dev") \
		COMMIT=$$(git rev-parse HEAD 2>/dev/null || echo "unknown") \
		BUILD_TIME=$$(date -u +"%Y-%m-%dT%H:%M:%SZ") \
		KO_DOCKER_REPO=ko.local \
		ko build ./cmd/scion --bare --local

## web-typecheck: Run TypeScript type checking on the web frontend
web-typecheck:
	@echo "Type-checking web frontend..."
	@cd web && npm run typecheck
	@echo "Type check passed."

## fmt: Auto-format Go source files
fmt:
	@echo "Formatting Go source files..."
	@gofmt -w .
	@echo "Go formatting done."

## fmt-check: Check Go formatting without modifying files (mirrors GitHub Actions)
fmt-check:
	@echo "Checking Go formatting..."
	@UNFORMATTED=$$(gofmt -l .); \
	if [ -n "$$UNFORMATTED" ]; then \
		echo "Go formatting issues found. Run 'make fmt' to fix:"; \
		echo "$$UNFORMATTED"; \
		exit 1; \
	fi
	@echo "Go formatting OK."

## ci: Run fast CI checks (format check, vet, tests, build)
ci: fmt-check lint test-fast build
	@echo ""
	@echo "CI passed."

## ci-full: Run the full CI pipeline locally (mirrors GitHub Actions, includes web + golangci-lint)
ci-full: fmt-check web web-typecheck lint golangci-lint test-fast build
	@echo ""
	@echo "CI (full) passed."

## clean: Remove build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR) .build
	@rm -f $(BINARY)
	@echo "Done."

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | column -t -s ':'
