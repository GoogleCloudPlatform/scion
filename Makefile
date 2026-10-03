# Scion Makefile
# Run 'make help' to see available targets.

BINARY        := scion
BUILD_DIR     := ./build
CONTAINER_DIR := ./.build/container
PREFIX        ?= /usr/local
DESTDIR       ?=
INSTALL_DIR   := $(PREFIX)/bin
MAIN_PKG      := ./cmd/scion
LDFLAGS            := $(shell ./hack/version.sh)
SCIONTOOL_LDFLAGS  := $(shell ./hack/version.sh github.com/GoogleCloudPlatform/scion/cmd/sciontool/commands)
CONTAINER_OS  := linux
CONTAINER_ARCH := $(shell if [ "$$(uname -m)" = "x86_64" ]; then echo amd64; else echo arm64; fi)
GOLANGCI_LINT := $(shell command -v golangci-lint 2>/dev/null || echo $(shell go env GOPATH)/bin/golangci-lint)

.DEFAULT_GOAL := help

.PHONY: all build build-a2a-bridge test-a2a-integration install test test-fast test-hub-sqlite vet lint vet-integration vet-integration-extras compat-literals check-annotation-prefix check-authz-guards check-conversation-upsert-guard check-security-marker-gates check-harness-coverage check-authorization-catalog check-custom golangci-lint web web-typecheck web-test fmt fmt-check tidy-extras ci ci-full clean help container-sciontool container-scion container-binaries proto proto-check

## all: Build the web frontend and compile the Go binary (run 'make install' separately to install)
all: web build

## build: Compile the scion binary into ./build/
build:
	@echo "Building $(BINARY)..."
	@mkdir -p $(BUILD_DIR)
	@go build -buildvcs=false -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) $(MAIN_PKG)
	@echo "Binary: $(BUILD_DIR)/$(BINARY)"

## build-a2a-bridge: Build the A2A bridge binary into ./bin/
build-a2a-bridge:
	@echo "Building scion-a2a-bridge..."
	@mkdir -p bin
	@go build -o bin/scion-a2a-bridge ./extras/scion-a2a-bridge/cmd/scion-a2a-bridge/
	@echo "Binary: bin/scion-a2a-bridge"

## test-a2a-integration: Run the A2A bridge deterministic integration suite with fail-closed PostgreSQL
test-a2a-integration:
	@./extras/scion-a2a-bridge/scripts/run-integration-ci.sh

## install: Install a pre-built binary (default: /usr/local/bin, override with PREFIX=~/.local). Run 'make build' first.
install:
	@if [ ! -f $(BUILD_DIR)/$(BINARY) ]; then \
		echo "Error: $(BUILD_DIR)/$(BINARY) not found. Run 'make build' (or 'make all') first."; \
		exit 1; \
	fi
	@echo "Installing $(BINARY) to $(DESTDIR)$(INSTALL_DIR)..."
	@mkdir -p $(DESTDIR)$(INSTALL_DIR)
	@install $(BUILD_DIR)/$(BINARY) $(DESTDIR)$(INSTALL_DIR)/$(BINARY)
	@echo ""
	@echo "✔ Installed $(BINARY) to $(DESTDIR)$(INSTALL_DIR)/$(BINARY)"
	@echo ""
	@echo "  Run 'scion version' to verify."
	@echo ""
	@case ":$$PATH:" in \
		*":$(INSTALL_DIR):"* | *":$(INSTALL_DIR)/:"*) ;; \
		*) echo "  ⚠ WARNING: $(INSTALL_DIR) is not in your PATH."; \
		   echo "  Add it with:"; \
		   echo ""; \
		   echo "    export PATH=\"$(INSTALL_DIR):\$$PATH\""; \
		   echo "" ;; \
	esac

## test: Run all tests
test:
	@echo "Running tests..."
	@go test ./...

## test-fast: Run tests without SQLite (lower memory usage)
test-fast:
	@echo "Running tests (no SQLite)..."
	@go test -tags no_sqlite ./...

## test-hub-sqlite: Run pkg/hub (and perf/bench/seed) tests with SQLite
# enabled (no build tag). This is the ~67% of pkg/hub's test files that
# "make test-fast" never compiles (see ptone/scion#1118), plus
# perf/bench/seed's own SQLite-backed tests, which carry the same
# `//go:build !no_sqlite` constraint for the same reason (ptone/scion#2393).
# Skips four pkg/hub tests with known pre-existing, tracked failures
# (ptone/scion#1847) so this target can be used as a CI merge gate.
test-hub-sqlite:
	@echo "Running pkg/hub + perf/bench/seed tests (SQLite-enabled)..."
	@go test -count=1 -timeout 40m \
		-skip '^(TestDEF164_AtAgentSlug_DeliversToAgent|TestDEF164_AtAgentSlug_DMConversationCreated|TestDEF152_AgentToAgentDM_DeliversViaOutbound|TestCreateTemplateV2_ScopeIDInjectionBlocked)$$' \
		./pkg/hub/... ./perf/bench/seed/...

## test-launch-store-postgres: Run the T1 async-create launch store/reaper
# suite against a real Postgres server (design t1-async-create-v11.md §6,
# "Postgres in CI"). Requires -tags integration and SCION_TEST_POSTGRES_URL;
# see pkg/store/enttest's package doc for the connection string format.
#
# pkg/store/integrationtest runs in full (its own self-contained Postgres-only
# harness). pkg/store/entadapter is scoped with -run to just the new launch
# store/reaper/report tests, per the design's literal wording ("runs
# pkg/store/integrationtest ... plus the new store and report-handler
# tests") -- NOT the whole existing entadapter suite. That distinction
# matters: this is the first CI job ever to run entadapter's existing tests
# against real Postgres (they otherwise only run against SQLite, or against
# Postgres in a developer's local -tags integration run), and running the
# full package here surfaced multiple pre-existing failures unrelated to T1
# (a backfill migration's raw SQL is invalid on Postgres; a conversation
# upsert test asserts timestamp equality at a precision Postgres does not
# preserve). Fixing those is out of scope for this design; -run keeps this
# job to what it was scoped to test.
#
# The -run regex also includes the broker-settings compare-and-set and
# row-lock tests (TestPutBrokerSettings*, TestDeleteBrokerSettings*,
# TestUsesRowLocks_ReflectsBackend, ptone/scion#2327): they assert
# dialect-dependent behavior (usesRowLocks/FOR UPDATE) the same way the T1
# tests do, so they belong in this job's Postgres coverage rather than running
# only against SQLite.
#
# It also includes the ListSchedules keyset-cursor tests (TestListSchedules_*,
# ptone/scion#2502): the keyset compares and binds `created` timestamps, whose
# storage and precision differ between SQLite and Postgres. It also includes
# TestListActiveZonePrefixedSchedules: its prefix match compiles to LIKE, whose
# case sensitivity differs between the two backends.
#
# Fail loudly, not green, if a Postgres-only case in this job's own suite
# skips instead of running. SCION_TEST_POSTGRES_URL is checked explicitly
# first; on -v test output, any "--- SKIP" line (including an indented
# subtest skip) also fails the target, since in this job every test that
# self-skips on a missing Postgres backend (enttest.Active() == false)
# indicates the job is misconfigured, not that skipping is an acceptable
# outcome here.
#
# pkg/store/integrationtest is exempt from that skip check: its Category 7
# (multi-process) tests always self-skip TestWorker_AdvisoryLock and
# TestWorker_NotifyPublisher when run normally -- those are child-process
# entrypoints a parent test in the same file launches as a subprocess with
# SCION_TEST_WORKER_DSN set (see multiprocess_test.go's package comment),
# not Postgres-availability skips. They are excluded by name so a genuine
# new skip in that package still fails the target.
test-launch-store-postgres:
	@echo "Running launch store tests against Postgres..."
	@if [ -z "$$SCION_TEST_POSTGRES_URL" ]; then \
		echo "ERROR: SCION_TEST_POSTGRES_URL is not set -- the Postgres-only cases would silently skip instead of running." >&2; \
		exit 1; \
	fi
	@go test -tags integration -count=1 -timeout 20m -v \
		./pkg/store/integrationtest/... > /tmp/test-launch-store-postgres-integrationtest.log 2>&1; \
	status=$$?; \
	cat /tmp/test-launch-store-postgres-integrationtest.log; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	if grep -E '^[[:space:]]*--- SKIP' /tmp/test-launch-store-postgres-integrationtest.log \
		| grep -qvE 'TestWorker_(AdvisoryLock|NotifyPublisher)'; then \
		echo "ERROR: one or more Postgres-only integration tests were skipped -- see '--- SKIP' lines above." >&2; \
		exit 1; \
	fi
	@go test -tags integration -count=1 -timeout 10m -v \
		-run '^(TestLaunchStore_|TestReaper_|TestListSchedules_|TestListActiveZonePrefixedSchedules|TestReport_H1_|TestPutBrokerSettings|TestDeleteBrokerSettings|TestUsesRowLocks_ReflectsBackend|TestCountAgents_|TestListAgentMembers_)' \
		./pkg/store/entadapter/... > /tmp/test-launch-store-postgres.log 2>&1; \
	status=$$?; \
	cat /tmp/test-launch-store-postgres.log; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	if grep -qE '^[[:space:]]*--- SKIP' /tmp/test-launch-store-postgres.log; then \
		echo "ERROR: one or more Postgres-only launch tests were skipped -- see '--- SKIP' lines above." >&2; \
		exit 1; \
	fi

## vet: Run go vet
vet:
	@go vet ./...

## lint: Run go vet (no SQLite, memory-safe)
lint:
	@go vet -tags no_sqlite ./...

## vet-integration: Compile-check integration-tagged code (go vet -tags 'integration volume_test')
# Catches build breaks in integration-tagged files that other vet/lint
# targets skip (ptone/scion#2348).
vet-integration:
	@go vet -tags 'integration volume_test' ./...

## vet-integration-extras: Compile-check integration-tagged code in every extras/ module that has it
# vet-integration only covers the root module's ./... tree; extras/*
# modules are separate go.mod trees it never reaches. Discovers modules
# dynamically (grep for the build tag) so new ones are covered without
# editing this target. Discovery uses only POSIX find and grep options
# (find -type f -name -exec ... \; -print, grep -qE), so it works with
# GNU, BSD/macOS and BusyBox. grep read errors still print to stderr; a
# find traversal error fails the target. The target also fails if it
# vets zero modules (e.g. extras/ moved or the build tag was renamed).
# A symlinked module dir is followed; symlinks inside a module are not.
vet-integration-extras:
	@echo "Vetting integration-tagged code in extras modules..."
	@failed=0; vetted=0; \
	for gomod in extras/*/go.mod; do \
		[ -f "$$gomod" ] || continue; \
		moddir=$$(dirname "$$gomod"); \
		if ! hits=$$(find "$$moddir/" -type f -name '*.go' -exec grep -qE '^//go:build.*[^A-Za-z0-9_]integration([^A-Za-z0-9_]|$$)' {} \; -print); then \
			echo "  FAILED: $$moddir (module discovery)"; failed=$$((failed + 1)); \
		elif [ -n "$$hits" ]; then \
			echo "  $$moddir"; \
			vetted=$$((vetted + 1)); \
			(cd "$$moddir" && go vet -tags integration ./...) || { echo "  FAILED: $$moddir"; failed=$$((failed + 1)); }; \
		fi; \
	done; \
	if [ "$$failed" -gt 0 ]; then \
		echo "$$failed extras module(s) failed discovery or integration vet ($$vetted vetted)."; \
		exit 1; \
	fi; \
	if [ "$$vetted" -eq 0 ]; then \
		echo "No extras modules with integration-tagged code found; expected at least one (check extras/ layout and the integration build tag)."; \
		exit 1; \
	fi; \
	echo "Vetted $$vetted extras module(s) with integration-tagged code."

## compat-literals: Check legacy grove literals stay in compatibility surfaces
compat-literals:
	@./hack/check-project-compat-literals.sh

## check-authz-guards: Verify no authorization-bypass patterns exist in handler code
# NOTE: make reports its own failure code (2) rather than the recipe's, so the
# script's exit 1 (violations found) and exit 2 (nothing was analysed — skipped,
# not clean) are indistinguishable to anything reading this target's exit status.
# The messages still differ on stderr. Any caller that needs to tell those two
# apart must invoke ./hack/check-authz-guards.sh directly, as CI does.
check-authz-guards:
	@./hack/check-authz-guards.sh

## check-annotation-prefix: Flag new scion.io/ annotation keys (migration to scion.dev/)
check-annotation-prefix:
	@./hack/check-annotation-prefix.sh

## check-conversation-upsert-guard: Verify UpsertConversationByExternalRef is only called from pkg/messaging and pkg/store
check-conversation-upsert-guard:
	@./hack/check-conversation-upsert-guard.sh

## check-security-marker-gates: Verify security symbols (authenticatedSender, validateDefaultAgent, ActionAttach) remain in handler code
check-security-marker-gates:
	@./hack/check-security-marker-gates.sh

## check-harness-coverage: Verify every harnesses/<name>/Dockerfile has a build step in each full-catalog cloudbuild-*.yaml
# NOTE: same caveat as check-authz-guards above -- make collapses the
# script's exit 1 (a harness is out of sync) and exit 2 (could not run) into
# one code. CI invokes the script directly to tell those apart.
check-harness-coverage:
	@./image-build/scripts/check-harness-coverage.sh

## check-authorization-catalog: Validate authorization operation catalog, permission coverage, and generated report
check-authorization-catalog:
	@./hack/check-authorization-catalog.sh

## check-custom: Run all custom CI lint checks (see hack/LINT-CONVENTIONS.md)
check-custom: compat-literals check-annotation-prefix check-authz-guards check-conversation-upsert-guard check-security-marker-gates check-authorization-catalog
	@echo "All custom checks passed."

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
	@rm -rf web/dist
	@cd web && npm install && npm run build
	@mkdir -p web/dist/client && touch web/dist/client/.gitkeep
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

## web-typecheck: Run TypeScript type checking on the web frontend
web-typecheck:
	@echo "Type-checking web frontend..."
	@cd web && npm run typecheck
	@echo "Type check passed."

## web-test: Run the web frontend unit tests (vitest)
web-test:
	@echo "Running web frontend tests..."
	@cd web && npm test
	@echo "Web tests passed."

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

## tidy-extras: Run go mod tidy in every extras/ module (fixes stale go.sum after root dep changes)
tidy-extras:
	@echo "Tidying extras modules..."
	@failed=0; \
	for moddir in $$(find extras -maxdepth 2 -name go.mod -printf '%h\n' | sort); do \
		echo "  $$moddir"; \
		(cd "$$moddir" && go mod tidy) || { echo "  FAILED: $$moddir"; failed=$$((failed + 1)); }; \
	done; \
	if [ "$$failed" -gt 0 ]; then \
		echo "$$failed module(s) failed to tidy."; \
		exit 1; \
	fi; \
	echo "All extras modules tidied."

## ci: Run fast CI checks (format check, vet, custom lint checks, tests, build)
ci: fmt-check lint check-custom test-fast build
	@echo ""
	@echo "CI passed."

## ci-full: Run the full CI pipeline locally (mirrors GitHub Actions, includes web + golangci-lint)
ci-full: fmt-check web web-typecheck web-test lint vet-integration vet-integration-extras check-custom golangci-lint test-fast build
	@echo ""
	@echo "CI (full) passed."

## proto: Generate Go code from .proto files
proto:
	@echo "Generating protobuf Go code..."
	@protoc \
		--proto_path=proto \
		--go_out=. --go_opt=module=github.com/GoogleCloudPlatform/scion \
		--go-grpc_out=. --go-grpc_opt=module=github.com/GoogleCloudPlatform/scion \
		proto/broker/v1/broker.proto
	@echo "Proto generation done."

## proto-check: Verify generated protobuf code is up to date
proto-check:
	@echo "Checking protobuf generated code is up to date..."
	@TMP=$$(mktemp -d) && \
	protoc \
		--proto_path=proto \
		--go_out=$$TMP --go_opt=module=github.com/GoogleCloudPlatform/scion \
		--go-grpc_out=$$TMP --go-grpc_opt=module=github.com/GoogleCloudPlatform/scion \
		proto/broker/v1/broker.proto && \
	diff $$TMP/proto/broker/v1/broker.pb.go proto/broker/v1/broker.pb.go && \
	diff $$TMP/proto/broker/v1/broker_grpc.pb.go proto/broker/v1/broker_grpc.pb.go && \
	rm -rf $$TMP && \
	echo "Proto generated code is up to date." || \
	(rm -rf $$TMP; echo "Proto generated code is out of date. Run 'make proto' to regenerate."; exit 1)

## clean: Remove build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR) .build web/dist
	@mkdir -p web/dist/client && touch web/dist/client/.gitkeep
	@rm -f $(BINARY)
	@echo "Done."

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | column -t -s ':'
