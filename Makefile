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

.PHONY: all build build-a2a-bridge test-a2a-integration install test test-fast test-hub-sqlite test-sqlite-nonhub test-cmd-sqlite test-fast-ci test-launch-store-postgres test-store-postgres test-store-postgres-others test-store-postgres-env-check test-store-postgres-integrationtest test-store-postgres-entadapter test-store-postgres-tail testpart-selftest test-hub-postgres test-hub-postgres-env-check test-hub-postgres-default test-hub-postgres-integration hub-postgres-integration-shards test-webchat-postgres test-conduit-authz-postgres test-artifacts-postgres test-fixture-coverage hubshard-parity build-hub-shard test-hub-shard test-hub-shards vet-hub-shard vet lint lint-ci vet-integration vet-integration-ci vet-integration-extras compat-literals check-annotation-prefix check-authz-guards check-conversation-upsert-guard check-hub-store-reassign check-store-delete-callers check-security-marker-gates cli-time-zones time-literals check-setenv-guard check-harness-coverage check-image-go-dirs check-authorization-catalog check-route-authz-manifest check-method-not-allowed check-debug-defaults check-sqlite-coverage sqlite-ci-pkgs check-custom golangci-lint web web-budget web-typecheck web-lint web-test fmt fmt-check tidy-extras ci ci-full clean help container-sciontool container-scion container-binaries proto proto-check ent-check tidy-check

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

## test-fast-ci: test-fast without the pkg/hub package itself (CI runs it as no_sqlite shards: test-hub-shard NOSQLITE=1)
test-fast-ci:
	@echo "Running tests (no SQLite, pkg/hub itself excluded)..."
	@pkgs=$$($(call non_hub_pkgs,no_sqlite)) || exit 1; \
	go test -tags no_sqlite $$pkgs

# HUB_TEST_GOGC is prefixed to go test commands that compile large test
# packages: the test-sqlite-nonhub prebuild (go test -c, build only) and the
# cmd run in test-store-postgres (there the go test command also runs the
# tests, so GOGC=25 applies to them too; they are short). The pkg/hub
# package itself compiles only in shards, through hack/hubshard/shard.sh,
# which applies the same default (GOGC=25 for its compiles, unless GOGC is
# set). History: with the default GOGC the whole pkg/hub test-package
# compile peaked at about 15 GB RSS (15.4 GB with -tags integration) on a
# 16 GB GitHub-hosted runner, filled its 3 GB swap, and the runner was shut
# down mid-step (exit 143, ptone/scion#4083); GOGC=25 cut that to about
# 12 GB for about 5% more compile time. A non-empty GOGC in the environment
# wins.
HUB_TEST_GOGC = GOGC=$${GOGC:-25}

# HUB_PKG is the pkg/hub package itself. Its tests are split into build-tag
# shards (hack/hubshard/README.md), so a shard's test binary compiles with
# about half the memory of the whole package's; CI never compiles the whole
# pkg/hub test package. Its subpackages are ordinary packages.
HUB_PKG := github.com/GoogleCloudPlatform/scion/pkg/hub

# non_hub_pkgs is a shell command that lists every package except HUB_PKG
# under build tags $(1), as ./... would match them for go test or go vet
# with those tags. It runs in the recipe, so other make invocations do not
# run go list.
non_hub_pkgs = go list -tags '$(1)' ./... | grep -vx '$(HUB_PKG)'

# SHARD selects the pkg/hub test shard (1..N, N from
# hack/hubshard/hubshard.sh -shards) for test-hub-shard, build-hub-shard,
# vet-hub-shard and test-hub-postgres.
SHARD ?= 1

## test-hub-sqlite: Run pkg/hub (every shard) and the non-hub SQLite packages (SQLITE_NONHUB_PKGS) with SQLite
# enabled (no build tag). This is the ~67% of pkg/hub's test files that
# "make test-fast" never compiles (see ptone/scion#1118), plus the
# non-hub SQLite packages (test-sqlite-nonhub). pkg/hub runs shard by
# shard (test-hub-shard), never as one test binary. CI runs the same
# pieces in parallel jobs: SQLite Tests (non-hub) and pkg/hub shard K/N.
test-hub-sqlite: test-sqlite-nonhub test-hub-shards

## test-sqlite-nonhub: Run the pkg/hub subpackages and the other non-hub SQLite packages (SQLITE_NONHUB_PKGS) with SQLite
# perf/bench/seed's and pkg/store/entadapter's own SQLite-backed tests carry
# a `//go:build !no_sqlite` constraint (ptone/scion#2393,
# ptone/scion#2851), pkg/conduit's relay/router tests run against the
# SQLite-backed conduit registry store, and pkg/artifacts' store and
# service tests run on SQLite. pkg/brokerownership, pkg/brokerregistration,
# pkg/ent/entc, pkg/secret, pkg/store/storetest and pkg/testinfra also have
# `//go:build !no_sqlite` test files; before ptone/scion#1088 they ran only
# in the non-blocking Full Test Suite. The pkg/hub package itself is left
# out: it runs in shards (test-hub-shard).
#
# The test binaries are first compiled with HUB_TEST_GOGC (go test -c into a
# scratch directory, then deleted). The go test run that follows reuses the
# cached compiled packages, so only the build runs with the lower GOGC; the
# tests themselves run with the default GOGC. go test -c with several
# packages fails if two of the test packages share a package name (Go
# 1.21+). There are no duplicates among the test packages today; if one
# appears, prebuild with $(HUB_TEST_GOGC) go test -count=1 -run '^$'
# instead, which compiles every package and runs no test functions.
#
# check-sqlite-coverage fails if a package with a `//go:build !no_sqlite`
# test file is in none of SQLITE_CI_PKGS, so add new SQLite packages here.
SQLITE_NONHUB_PKGS := ./pkg/hub/... ./perf/bench/seed/... ./pkg/conduit/... \
	./pkg/store/entadapter/... ./pkg/artifacts/... ./pkg/brokerownership/... \
	./pkg/brokerregistration/... ./pkg/ent/entc/... ./pkg/secret/... \
	./pkg/store/storetest/... ./pkg/store/relaydb ./pkg/testinfra/...
SQLITE_NONHUB_PKGS_CMD = go list $(SQLITE_NONHUB_PKGS) | grep -vx '$(HUB_PKG)'

# SQLITE_CI_PKGS are the package patterns that blocking CI jobs test with
# SQLite enabled (no no_sqlite tag): SQLITE_NONHUB_PKGS (SQLite Tests
# (non-hub), test-sqlite-nonhub; ./pkg/hub/... there also matches the
# pkg/hub package itself, which the pkg/hub shard jobs run instead),
# SQLITE_CMD_PKGS (test-cmd-sqlite) and FIXTURE_PKGS
# (test-fixture-coverage). Those targets test exactly these variables, so
# the list check-sqlite-coverage checks is the list CI runs.
SQLITE_CMD_PKGS := ./cmd
FIXTURE_PKGS := ./internal/fixturegen/...
SQLITE_CI_PKGS = $(SQLITE_NONHUB_PKGS) $(SQLITE_CMD_PKGS) $(FIXTURE_PKGS)

## sqlite-ci-pkgs: Print SQLITE_CI_PKGS, one pattern per line (read by hack/check-sqlite-coverage.sh)
sqlite-ci-pkgs:
	@printf '%s\n' $(SQLITE_CI_PKGS)

test-sqlite-nonhub:
	@echo "Running pkg/hub subpackages + the other SQLITE_NONHUB_PKGS tests (SQLite-enabled)..."
	@pkgs=$$($(SQLITE_NONHUB_PKGS_CMD)) || exit 1; \
	dir=$$(mktemp -d) || exit 1; \
	$(HUB_TEST_GOGC) go test -c -o "$$dir/" $$pkgs; \
	status=$$?; \
	if [ -n "$$dir" ]; then rm -rf "$$dir"; fi; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	go test -count=1 -timeout 60m $$pkgs

## test-cmd-sqlite: Run the cmd package tests with SQLite enabled (no build tag)
# 41 cmd test files carry `//go:build !no_sqlite`, so "make test-fast"
# (-tags no_sqlite) never compiles them (ptone/scion#1088). This runs the
# whole cmd package untagged: those files plus the untagged ones, which
# test-fast also runs (about 30s). CI runs it in the SQLite Tests
# (non-hub) job.
test-cmd-sqlite:
	@echo "Running cmd tests (SQLite-enabled)..."
	@go test -count=1 -timeout 20m $(SQLITE_CMD_PKGS)

## hubshard-parity: Check the pkg/hub test shards: every test runs in exactly one shard, every shard compiles (parse only)
# Also fails if a hubshard tag or a generated imports file is committed:
# the tags exist only in the overlay hack/hubshard writes on every run.
# hubshard prints the shard sizes; -untag strips tags an older generator
# committed. hack/hubshard/hubshard.sh builds the tool into a temp dir and
# runs it (as go run would). See hack/hubshard/README.md.
hubshard-parity:
	@./hack/hubshard/hubshard.sh -parity

## testpart-selftest: Test hack/testpart.sh, which splits the T1 entadapter Postgres tests over CI jobs (bash only)
testpart-selftest:
	@./hack/test-testpart.sh

## test-hub-shard: Run pkg/hub test shard SHARD=K (NOSQLITE=1: with -tags no_sqlite; RACE=1: with -race)
# Generates the shard overlay in a temp dir (the tree is not modified),
# compiles the shard, checks its test list against hubshard -list, then
# runs it. The common files' tests run only in shard 1.
test-hub-shard:
	@./hack/hubshard/shard.sh test $(SHARD) $(if $(NOSQLITE),--nosqlite) $(if $(RACE),--race)

## test-hub-shards: Run every pkg/hub test shard in turn (NOSQLITE=1, RACE=1 as for test-hub-shard)
# The shard list comes from hack/hubshard/hubshard.sh -shards. Every shard
# runs even when an earlier one fails; the target fails if any did.
test-hub-shards:
	@shards=$$(./hack/hubshard/hubshard.sh -shards) || exit 1; \
	failed=; \
	for k in $$shards; do \
		./hack/hubshard/shard.sh test $$k $(if $(NOSQLITE),--nosqlite) $(if $(RACE),--race) || failed="$$failed $$k"; \
	done; \
	if [ -n "$$failed" ]; then echo "ERROR: pkg/hub shard(s) failed:$$failed" >&2; exit 1; fi

## build-hub-shard: Compile (go test -c, discarded) pkg/hub test shard SHARD=K (NOSQLITE=1: with -tags no_sqlite)
build-hub-shard:
	@./hack/hubshard/shard.sh build $(SHARD) $(if $(NOSQLITE),--nosqlite)

## vet-hub-shard: go vet pkg/hub test shard SHARD=K with -tags no_sqlite and -tags 'integration volume_test tzcontract'
# The shards' vets together cover every pkg/hub file that make lint and
# make vet-integration cover (hubshard-parity checks it). CI vets pkg/hub
# this way (lint-ci and vet-integration-ci leave the package out).
vet-hub-shard:
	@./hack/hubshard/shard.sh vet $(SHARD)

## test-fixture-coverage: Run the hub fixture coverage gate (TestFixtureCoverage) with SQLite
# internal/fixturegen's tests carry `//go:build !no_sqlite`, so
# "make test-fast" never compiles them and a schema change that skips the
# fixture went red only in the non-blocking full suite (ptone/scion#625,
# ptone/scion#1931). CI runs this in the SQLite Tests (non-hub) job.
test-fixture-coverage:
	@echo "Running fixture coverage gate (SQLite-enabled)..."
	@go test -count=1 $(FIXTURE_PKGS)

# TEST_IDENTITY_POSTGRES_TESTS are the pkg/hub test identity concurrency
# tests (ptone/scion#4240: the Phase 3b purge, and the X4 parallel admin-kind
# mints across workload delegations) run by test-launch-store-postgres
# (pkg/hub shard Postgres (integration) jobs in CI).
TEST_IDENTITY_POSTGRES_TESTS := TestFixturePurge_ConcurrentCoOwner_Postgres \
	TestWorkloadAdminDelegation_ParallelMintsHoldCaps_Postgres

# MEMBERSHIP_LOSS_POSTGRES_TESTS are the pkg/hub membership loss
# concurrency tests (ptone/scion#3433) run by test-launch-store-postgres.
MEMBERSHIP_LOSS_POSTGRES_TESTS := TestMembershipLossProcessor_vs_ProjectDelete_Postgres \
	TestMembershipLossProcessor_vs_AgentHardDelete_Postgres \
	TestMembershipLossProcessor_vs_ReAdd_Postgres \
	TestMembershipLossProcessor_vs_CredentialMint_Postgres \
	TestMembershipLossProcessor_vs_ChildCreate_Postgres \
	TestMembershipLossProcessor_TwoInstances_Postgres \
	TestMembershipLossProcessor_vs_UserDelete_Postgres \
	TestMembershipReconciler_LocksCoexist_Postgres

## test-store-postgres: Run the T1 async-create launch store/reaper
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
# It also includes the secret-value compare-and-swap tests
# (TestUpdateSecretValueIfVersion*): the Conduit grant key ring rotation relies
# on this conditional UPDATE, and HA hubs run it on Postgres.
#
# It also includes the ListSchedules keyset-cursor tests (TestListSchedules_*,
# ptone/scion#2502): the keyset compares and binds `created` timestamps, whose
# storage and precision differ between SQLite and Postgres. It also includes
# TestListActiveZonePrefixedSchedules: its prefix match compiles to LIKE, whose
# case sensitivity differs between the two backends.
#
# It also includes the utc-timestamp-normalize JSON tests
# (TestUTCTimestampNormalizeJSON_*, ptone/scion#2499): the JSON-embedded
# timestamp rewrite is the part of that operation that runs on Postgres, with
# its own SQL (jsonb casts, id keyset).
# It also includes the agent run intent tests (TestRunIntent_*): they take
# a FOR UPDATE row lock on Postgres, read the store clock with now(), and
# compare run_intent_at values whose stored precision differs between the
# two backends.
#
# It also includes the Conduit registry suite (TestConduitRegistry_*,
# ptone/scion#2778): design conduit v2.1 §3.4 requires relay_instances,
# conduit_sessions and conduit_principal_epochs to behave identically on
# Postgres and SQLite (generation/epoch upsert ... RETURNING, generation-CAS
# deletes, concurrent epoch allocation), so the same suite runs on both.
#
# It also includes the project owner_id store-contract tests
# (TestProjectOwnerID_*, ptone/scion#2597): UpdateProject must not write
# owner_id on either backend, so SetProjectOwnerID stays its only writer.
#
# It also includes the child-group edge removal tests
# (TestRemoveChildGroupEdge_*, ptone/scion#2723): on Postgres a second
# transaction deleting the same edge waits on the first one's row lock and
# must then report ErrNotFound, so only one caller writes an audit record.
# It also includes the orphaned group-membership tests and the composite
# purge tests (ptone/scion#2769): PurgeDeletedAgents re-reads its batch with
# FOR UPDATE on Postgres only, and the restore-during-purge cases exercise
# that path. A third run covers the storetest group/MembershipCleanup
# conformance (pkg/store/storetest), selected by name so only that subtest
# of the CRUD-parity suite runs here. The TestCompositeDeleteAgent_ and
# TestCompositeDeleteProject_ prefixes also select the PostgreSQL-only
# *_LockOrderNoDeadlock tests, which check that both deletes lock agent rows
# before deleting their memberships (the purge/finalize order).
# TestCompositeDeleteProject_LocksAgentsInIDOrder pins the ascending order of
# that lock, and the TestPurgeDeletedAgents_ prefix selects
# TestPurgeDeletedAgents_CrossBatchLockOrderNoDeadlock (purge batches in ID
# order). A fourth run covers the production project-delete path in pkg/hub
# (TestProjectDeletionService_LockOrderNoDeadlock: ProjectDeletionService
# locks the project's agents before its project-group cascade deletes agent
# memberships), selected by name so only that test runs here. In the second
# run (the full entadapter suite), the TestDeleteGroupMembershipsForUser_
# prefix also selects
# TestDeleteGroupMembershipsForUser_LockOrderVsProjectGroupCascade (a user
# delete locks the groups it owns before its memberships, so it cannot
# deadlock against a project delete's group cascade) and
# TestDeleteGroupMembershipsForUser_OwnedGroupLockAllowsMemberInsert (that
# lock is FOR NO KEY UPDATE, so it does not block the FK check of a
# membership inserted into an owned group, which would deadlock).
#
# Since ptone/scion#2207 the job runs the FULL pkg/store/entadapter suite on
# Postgres (no -run filter); the lists above document why particular groups
# matter here, not what is selected. Every entadapter test now has to pass on
# both backends.
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
#
# The pkg/ent/entc run covers the SQLite-to-Postgres `scion server migrate`
# path (TestMigrateBeta_SQLiteToPostgres, ptone/scion#4366) and skill creation
# against a pre-existing `visibility` column
# (TestSkillStore_CreateAgainstOldSchemaWithVisibilityColumn_Postgres,
# ptone/scion#4370). Each works in its own throwaway schema, so neither
# disturbs the shared database. Any skip fails the target first, then each
# test must print its own PASS line.
#
# The cmd run covers the PostgreSQL search_path shadow guard in every
# standalone command that migrates the hub database (server migrate,
# server backfill, server migrate-dm-keys, server recover-authz,
# server test-infra mark, hub secret migrate-names; ptone/scion#4432):
# TestMaintenanceCommandsSchemaShadowGuard_Postgres. Each case uses its own
# throwaway schemas. Any skip fails the target, and the test must print its
# own PASS line.
#
# In the entadapter run, a test that is SQLite-only by design skips through
# enttest.SkipOnPostgres, whose skip message is
# "enttest-sqlite-only: <test name> <reason>". A skipped test is allowed only
# if its own skip message carries that marker, so there is no name list here
# and any other skip still fails the target.
#
# The entadapter run has a 60m go test timeout. On green main runs it took
# ~774s and ~1259s, but GoogleCloudPlatform/scion#2855 hit the old 40m
# (2400s) limit on a runner 2-3x slower than usual, with no failing or hung
# test (ptone/scion#3945). A timeout there also stops the storetest and
# later runs below, so the headroom is kept generous.
#
# test-store-postgres is the part without the pkg/hub package itself
# (CI's T1 job runs it); test-launch-store-postgres adds the pkg/hub
# Postgres tests, which CI runs in the pkg/hub shard Postgres jobs.
#
# CI splits this target over several T1 jobs (see the T1 jobs in
# .github/workflows/ci.yml): test-store-postgres-entadapter with
# ENTADAPTER_PART=K ENTADAPTER_PARTS=N runs part K of the entadapter tests,
# and test-store-postgres-others runs the rest. Locally,
# make test-store-postgres runs it all, in the order below. The umbrella
# targets run their parts one after another through sub-makes (not as
# prerequisites), so make -j never runs them at once: they share one
# Postgres server, and the cmd run is a large compile.
test-store-postgres:
	@$(MAKE) --no-print-directory test-store-postgres-integrationtest
	@$(MAKE) --no-print-directory test-store-postgres-entadapter
	@$(MAKE) --no-print-directory test-store-postgres-tail

## test-store-postgres-others: test-store-postgres without pkg/store/entadapter (CI runs entadapter in its own jobs)
test-store-postgres-others:
	@$(MAKE) --no-print-directory test-store-postgres-integrationtest
	@$(MAKE) --no-print-directory test-store-postgres-tail

# test-store-postgres-env-check fails if SCION_TEST_POSTGRES_URL is unset.
test-store-postgres-env-check:
	@if [ -z "$$SCION_TEST_POSTGRES_URL" ]; then \
		echo "ERROR: SCION_TEST_POSTGRES_URL is not set -- the Postgres-only cases would silently skip instead of running." >&2; \
		exit 1; \
	fi

test-store-postgres-integrationtest: test-store-postgres-env-check
	@echo "Running launch store tests against Postgres..."
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

# ENTADAPTER_PARTS splits the pkg/store/entadapter Postgres tests into that
# many parts and ENTADAPTER_PART (1..ENTADAPTER_PARTS) selects the one
# test-store-postgres-entadapter runs. The default, 1 of 1, runs the whole
# package without -run, as before the split. CI runs every part in its own
# job (the T1 entadapter jobs in .github/workflows/ci.yml).
ENTADAPTER_PART ?= 1
ENTADAPTER_PARTS ?= 1

## test-store-postgres-entadapter: The pkg/store/entadapter part of test-store-postgres (ENTADAPTER_PART=K ENTADAPTER_PARTS=N: part K of N)
# With ENTADAPTER_PARTS > 1 the target lists the package's top-level tests
# (go test -list, which runs TestMain and so needs the Postgres server),
# checks with hack/testpart.sh parity that the N parts together hold every
# listed test exactly once, and runs part K with -run '^(A|B|...)$'
# (round-robin over the sorted names; subtests of a selected test run with
# it). After the usual checks, every test of the part must print its own
# top-level PASS or SKIP line, so a test that the pattern misses fails the
# target instead of silently not running.
test-store-postgres-entadapter: test-store-postgres-env-check
	@part='$(ENTADAPTER_PART)'; parts='$(ENTADAPTER_PARTS)'; \
	set --; \
	if [ "$$parts" = 1 ] && [ "$$part" != 1 ]; then \
		echo "ERROR: ENTADAPTER_PART must be 1 when ENTADAPTER_PARTS is 1 (got $$part)." >&2; \
		exit 1; \
	fi; \
	if [ "$$parts" != 1 ]; then \
		if ! go test -tags integration -count=1 -list '.*' \
			./pkg/store/entadapter/... > /tmp/test-launch-store-postgres.list 2>&1; then \
			cat /tmp/test-launch-store-postgres.list; \
			echo "ERROR: could not list the entadapter tests." >&2; \
			exit 1; \
		fi; \
		./hack/testpart.sh parity "$$parts" < /tmp/test-launch-store-postgres.list || exit 1; \
		./hack/testpart.sh list "$$part" "$$parts" < /tmp/test-launch-store-postgres.list \
			> /tmp/test-launch-store-postgres.part || exit 1; \
		regex=$$(./hack/testpart.sh regex "$$part" "$$parts" < /tmp/test-launch-store-postgres.list) || exit 1; \
		echo "Running part $$part of $$parts of the entadapter Postgres tests ($$(wc -l < /tmp/test-launch-store-postgres.part | tr -d ' ') top-level tests)..."; \
		set -- -run "$$regex"; \
	fi; \
	go test -tags integration -count=1 -timeout 60m -v "$$@" \
		./pkg/store/entadapter/... > /tmp/test-launch-store-postgres.log 2>&1; \
	status=$$?; \
	cat /tmp/test-launch-store-postgres.log; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	sed -nE 's/^[[:space:]]*--- SKIP: ([^ ]+).*/\1/p' /tmp/test-launch-store-postgres.log | sort -u \
		> /tmp/test-launch-store-postgres.skipped; \
	grep -oE 'enttest-sqlite-only: [^ ]+' /tmp/test-launch-store-postgres.log | awk '{print $$2}' | sort -u \
		> /tmp/test-launch-store-postgres.sqlite-only; \
	unexpected=$$(grep -vxF -f /tmp/test-launch-store-postgres.sqlite-only /tmp/test-launch-store-postgres.skipped); \
	if [ -n "$$unexpected" ]; then \
		echo "ERROR: these entadapter tests were skipped on Postgres without enttest.SkipOnPostgres:" >&2; \
		echo "$$unexpected" >&2; \
		exit 1; \
	fi; \
	if [ "$$parts" != 1 ]; then \
		sed -nE 's/^--- (PASS|SKIP): ([^ ]+).*/\2/p' /tmp/test-launch-store-postgres.log | LC_ALL=C sort -u \
			> /tmp/test-launch-store-postgres.ran; \
		missing=$$(LC_ALL=C comm -23 /tmp/test-launch-store-postgres.part /tmp/test-launch-store-postgres.ran); \
		if [ -n "$$missing" ]; then \
			echo "ERROR: these entadapter tests of part $$part of $$parts printed no top-level PASS or SKIP line:" >&2; \
			echo "$$missing" >&2; \
			exit 1; \
		fi; \
	fi

# test-store-postgres-tail: the storetest, pkg/store/enttest, pkg/ent/entc,
# pkg/testinfra and cmd runs of test-store-postgres (after entadapter). The enttest run
# includes TestSchemaSnapshot_MatchesAutoMigrate, which checks that the
# schema snapshot the harness provisions test schemas from matches
# entc.AutoMigrate.
test-store-postgres-tail: test-store-postgres-env-check
	@go test -tags integration -count=1 -timeout 10m -v \
		-run '^TestCompositeStore_CRUDParity$$/^group$$/^MembershipCleanup$$' \
		./pkg/store/storetest/... > /tmp/test-launch-store-postgres-storetest.log 2>&1; \
	status=$$?; \
	cat /tmp/test-launch-store-postgres-storetest.log; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	if ! grep -qE '^[[:space:]]*--- PASS: TestCompositeStore_CRUDParity/group/MembershipCleanup' /tmp/test-launch-store-postgres-storetest.log; then \
		echo "ERROR: the storetest group/MembershipCleanup conformance did not run." >&2; \
		exit 1; \
	fi; \
	if grep -qE '^[[:space:]]*--- SKIP' /tmp/test-launch-store-postgres-storetest.log; then \
		echo "ERROR: one or more storetest group/MembershipCleanup cases were skipped -- see '--- SKIP' lines above." >&2; \
		exit 1; \
	fi
	@go test -tags integration -count=1 -timeout 10m -v \
		./pkg/store/enttest/ > /tmp/test-launch-store-postgres-enttest.log 2>&1; \
	status=$$?; \
	cat /tmp/test-launch-store-postgres-enttest.log; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	if grep -qE '^[[:space:]]*--- SKIP' /tmp/test-launch-store-postgres-enttest.log; then \
		echo "ERROR: one or more pkg/store/enttest Postgres tests were skipped -- see '--- SKIP' lines above." >&2; \
		exit 1; \
	fi; \
	if ! grep -qE '^--- PASS: TestSchemaSnapshot_MatchesAutoMigrate ' /tmp/test-launch-store-postgres-enttest.log; then \
		echo "ERROR: the schema snapshot equivalence test did not run." >&2; \
		exit 1; \
	fi
	@go test -tags integration -count=1 -timeout 10m -v \
		-run '^(TestMigrateBeta_SQLiteToPostgres|TestSkillStore_CreateAgainstOldSchemaWithVisibilityColumn_Postgres)$$' \
		./pkg/ent/entc/ > /tmp/test-launch-store-postgres-entc.log 2>&1; \
	status=$$?; \
	cat /tmp/test-launch-store-postgres-entc.log; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	if grep -qE '^[[:space:]]*--- SKIP' /tmp/test-launch-store-postgres-entc.log; then \
		echo "ERROR: one or more pkg/ent/entc Postgres tests were skipped -- see '--- SKIP' lines above." >&2; \
		exit 1; \
	fi; \
	if ! grep -qE '^--- PASS: TestMigrateBeta_SQLiteToPostgres ' /tmp/test-launch-store-postgres-entc.log; then \
		echo "ERROR: the SQLite-to-Postgres server migrate test did not run." >&2; \
		exit 1; \
	fi; \
	if ! grep -qE '^--- PASS: TestSkillStore_CreateAgainstOldSchemaWithVisibilityColumn_Postgres ' /tmp/test-launch-store-postgres-entc.log; then \
		echo "ERROR: the skill-visibility old-schema Postgres test did not run." >&2; \
		exit 1; \
	fi
	@go test -tags integration -count=1 -timeout 10m -v \
		-run '^TestDelegate_ConcurrentLiveCapHolds_Postgres$$' \
		./pkg/testinfra/ > /tmp/test-launch-store-postgres-testinfra.log 2>&1; \
	status=$$?; \
	cat /tmp/test-launch-store-postgres-testinfra.log; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	if grep -qE '^[[:space:]]*--- SKIP' /tmp/test-launch-store-postgres-testinfra.log; then \
		echo "ERROR: the pkg/testinfra Postgres test was skipped -- see '--- SKIP' lines above." >&2; \
		exit 1; \
	fi; \
	if ! grep -qE '^--- PASS: TestDelegate_ConcurrentLiveCapHolds_Postgres ' /tmp/test-launch-store-postgres-testinfra.log; then \
		echo "ERROR: the offline delegation live-cap Postgres test did not run." >&2; \
		exit 1; \
	fi
	@st=$$(mktemp) || exit 1; \
	{ $(HUB_TEST_GOGC) go test -tags integration -count=1 -timeout 20m -v \
		-run '^(TestMaintenanceCommandsSchemaShadowGuard_Postgres|TestServerPreflight_Postgres)$$' \
		./cmd/ 2>&1; echo $$? > "$$st"; } \
		| tee /tmp/test-launch-store-postgres-cmd.log; \
	status=$$(cat "$$st" 2>/dev/null); \
	rm -f "$$st"; \
	case "$$status" in ''|*[!0-9]*) status=1;; esac; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	if grep -qE '^[[:space:]]*--- SKIP' /tmp/test-launch-store-postgres-cmd.log; then \
		echo "ERROR: one or more cmd maintenance-command Postgres cases were skipped -- see '--- SKIP' lines above." >&2; \
		exit 1; \
	fi; \
	if ! grep -qE '^--- PASS: TestMaintenanceCommandsSchemaShadowGuard_Postgres ' /tmp/test-launch-store-postgres-cmd.log; then \
		echo "ERROR: the maintenance-command search_path guard Postgres test did not run." >&2; \
		exit 1; \
	fi; \
	if ! grep -qE '^--- PASS: TestServerPreflight_Postgres ' /tmp/test-launch-store-postgres-cmd.log; then \
		echo "ERROR: the server preflight Postgres test did not run." >&2; \
		exit 1; \
	fi

## test-launch-store-postgres: test-store-postgres plus the pkg/hub Postgres tests in HUB_INTEGRATION_POSTGRES_TESTS (every shard holding one)
# CI runs the two halves in different jobs: test-store-postgres in the T1
# Launch Store PostgreSQL Tests jobs, and the pkg/hub tests in the pkg/hub
# shard Postgres (integration) jobs (test-hub-postgres-integration
# SHARD=K).
test-launch-store-postgres: test-store-postgres
	@./hack/hubshard/shard.sh postgres all integration $(HUB_INTEGRATION_POSTGRES_TESTS)

# HUB_INTEGRATION_POSTGRES_TESTS are the pkg/hub Postgres tests that need
# -tags integration (the integration TestMain sets up the ent test
# database): the project-delete lock-order test, the two-replica scheduled
# send test, the membership loss and test identity concurrency tests, and
# the broker presence registry test (its conduit registry runs on the ent
# test database).
# Each must print a PASS line, and any skip fails (shard.sh postgres).
HUB_INTEGRATION_POSTGRES_TESTS := TestProjectDeletionService_LockOrderNoDeadlock \
	TestScheduledSend_TwoHubReplicasPostgres_OneDelivery \
	TestBrokerPresence_LiveRegistrySession \
	$(MEMBERSHIP_LOSS_POSTGRES_TESTS) \
	$(TEST_IDENTITY_POSTGRES_TESTS) \
	TestHubInternal_ReplayAcrossHubReplicas

## test-hub-postgres: Run the pkg/hub Postgres tests of shard SHARD=K (SHARD=all: every shard holding one)
# Requires SCION_TEST_POSTGRES_URL (integration tests) and
# SCION_TEST_POSTGRES_DSN (web chat and conduit tests). Runs
# WEBCHAT_POSTGRES_TESTS and CONDUIT_AUTHZ_POSTGRES_TESTS (default tags)
# and HUB_INTEGRATION_POSTGRES_TESTS (-tags integration) that live in the
# shard; hack/hubshard -locate fails if a listed test no longer exists.
# CI runs the two halves in different jobs. test-hub-postgres-default runs
# in each pkg/hub shard job, after the shard's SQLite run, so the
# default-tag build comes from the cache. test-hub-postgres-integration
# needs its own pkg/hub compile (-tags integration: a different TestMain
# and extra test files), so it runs in its own job, in parallel with the
# shard jobs, for each shard hub-postgres-integration-shards prints.
# The two halves run one after another through sub-makes (not as
# prerequisites), so make -j never runs two pkg/hub compiles at once.
test-hub-postgres:
	@$(MAKE) --no-print-directory test-hub-postgres-default
	@$(MAKE) --no-print-directory test-hub-postgres-integration

# test-hub-postgres-env-check fails unless both Postgres variables are set.
test-hub-postgres-env-check:
	@if [ -z "$$SCION_TEST_POSTGRES_URL" ] || [ -z "$$SCION_TEST_POSTGRES_DSN" ]; then \
		echo "ERROR: SCION_TEST_POSTGRES_URL and SCION_TEST_POSTGRES_DSN must be set -- the Postgres tests would silently skip instead of running." >&2; \
		exit 1; \
	fi

## test-hub-postgres-default: The default-tag half of test-hub-postgres (WEBCHAT_POSTGRES_TESTS, CONDUIT_AUTHZ_POSTGRES_TESTS) for SHARD=K
test-hub-postgres-default: test-hub-postgres-env-check
	@./hack/hubshard/shard.sh postgres $(SHARD) "" $(WEBCHAT_POSTGRES_TESTS) $(CONDUIT_AUTHZ_POSTGRES_TESTS)

## test-hub-postgres-integration: The -tags integration half of test-hub-postgres (HUB_INTEGRATION_POSTGRES_TESTS) for SHARD=K
test-hub-postgres-integration: test-hub-postgres-env-check
	@./hack/hubshard/shard.sh postgres $(SHARD) integration $(HUB_INTEGRATION_POSTGRES_TESTS)

## hub-postgres-integration-shards: Print the pkg/hub shards that hold a HUB_INTEGRATION_POSTGRES_TESTS test, one per line (CI's shard plan)
# Fails if a listed test is in no test file (hubshard -locate).
hub-postgres-integration-shards:
	@located=$$(./hack/hubshard/hubshard.sh -tags integration -locate $(HUB_INTEGRATION_POSTGRES_TESTS)) || exit 1; \
	echo "$$located" | awk '{print $$2}' | sort -nu

## test-webchat-postgres: Run the pkg/hub web chat store Postgres tests against a real server
# Requires SCION_TEST_POSTGRES_DSN (a pgx connection string). The selected
# tests self-skip without it, so the target fails if the variable is unset,
# if any selected test skips, or if any test listed in
# WEBCHAT_POSTGRES_TESTS reports no PASS line. Each test runs in its
# pkg/hub shard (hack/hubshard/shard.sh postgres); CI runs them in the
# pkg/hub shard jobs (test-hub-postgres-default). The tests drop and recreate the
# webchat_* tables, so point the DSN at a scratch database. go test output
# is streamed with tee (shard.sh runs under bash pipefail) so a step killed
# mid-run still shows what ran (ptone/scion#4083).
WEBCHAT_POSTGRES_TESTS := TestListTopicsByProjects_Postgres \
	TestC4Fix_Postgres_FreshDB \
	TestC4Fix_Postgres_PreExistingDB \
	TestC4Fix_Postgres_Idempotent \
	TestC4Fix_Postgres_PreExistingDB_Idempotent \
	TestUnreadMentionKeys_Postgres \
	TestScheduledStore_Postgres \
	TestWebChatStore_Postgres_MessagesInOtherSchema \
	TestWebChatStore_Postgres_ThreadMigrationsWaitForMessagesTable \
	TestWebChatStore_Postgres_ThreadMigrationsRunOnce \
	TestUserPrefsThreadGroupsMarker_Postgres \
	TestUserPrefsThreadGroupsMigratedAt_Postgres_PreExistingDB

test-webchat-postgres:
	@if [ -z "$$SCION_TEST_POSTGRES_DSN" ]; then \
		echo "ERROR: SCION_TEST_POSTGRES_DSN is not set -- the Postgres tests would silently skip instead of running." >&2; \
		exit 1; \
	fi
	@./hack/hubshard/shard.sh postgres all "" $(WEBCHAT_POSTGRES_TESTS)

## test-conduit-authz-postgres: Run the conduit user-stream re-check tests (criterion 16b, P tier) against a real Postgres server
# Requires SCION_TEST_POSTGRES_DSN (a pgx connection string). Two hub nodes
# share one database through their own stores and LISTEN/NOTIFY event
# publishers; each test creates and drops its own schema. The tests
# self-skip without the DSN, so the target fails if the variable is unset,
# if any selected test skips, or if any listed test reports no PASS line.
CONDUIT_AUTHZ_POSTGRES_TESTS := TestConduitAuthzPostgres_NotifyAcrossNodes \
	TestConduitAuthzPostgres_UserSuspendDeleteAndTokenRevoke \
	TestConduitAuthzPostgres_ListenGapResync \
	TestConduitAuthzPostgres_BulkRevocation \
	TestSeparateRelayListenerReceivesHubEvents

test-conduit-authz-postgres:
	@if [ -z "$$SCION_TEST_POSTGRES_DSN" ]; then \
		echo "ERROR: SCION_TEST_POSTGRES_DSN is not set -- the Postgres tests would silently skip instead of running." >&2; \
		exit 1; \
	fi
	@./hack/hubshard/shard.sh postgres all "" $(CONDUIT_AUTHZ_POSTGRES_TESTS)

## test-artifacts-postgres: Run the pkg/artifacts tests: store tests on SQLite and a real Postgres (service tests on SQLite)
# Requires SCION_TEST_POSTGRES_DSN (a pgx connection string). The artifact
# store is not an Ent store (design D3: own tables, created by Init), so like
# the web chat store it has its own Postgres SQL and its own Postgres tests.
# Every store test runs once per dialect as a /sqlite and a /postgres
# subtest; the Postgres subtests are added only when the DSN is set, so the
# target fails if the variable is unset, if any test skips, or if no
# /postgres subtest passed. Each test works in its own throwaway schema.
# CI runs this in the T1 Launch Store PostgreSQL Tests (other packages) job.
test-artifacts-postgres:
	@echo "Running pkg/artifacts tests (store tests on SQLite and Postgres)..."
	@if [ -z "$$SCION_TEST_POSTGRES_DSN" ]; then \
		echo "ERROR: SCION_TEST_POSTGRES_DSN is not set -- the Postgres tests would silently be left out." >&2; \
		exit 1; \
	fi
	@go test -count=1 -timeout 10m -v ./pkg/artifacts/... > /tmp/test-artifacts-postgres.log 2>&1; \
	status=$$?; \
	cat /tmp/test-artifacts-postgres.log; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	if grep -qE '^[[:space:]]*--- SKIP' /tmp/test-artifacts-postgres.log; then \
		echo "ERROR: an artifact test was skipped -- see '--- SKIP' lines above." >&2; \
		exit 1; \
	fi; \
	if ! grep -qE '^[[:space:]]*--- PASS: [^ ]+/postgres ' /tmp/test-artifacts-postgres.log; then \
		echo "ERROR: no Postgres subtest passed." >&2; \
		exit 1; \
	fi

## test-tz-contract: Run the real-binary timestamp contract test (SQLite; Postgres too when SCION_TEST_POSTGRES_URL is set)
# It builds cmd/scion, starts `scion server start --foreground` under non-UTC TZ values
# and checks every timestamp on the wire is the written instant in UTC ("Z").
# SQLite always runs; Postgres runs when SCION_TEST_POSTGRES_URL is set, and
# the target then fails if the Postgres cases did not pass.
test-tz-contract:
	@echo "Running the timestamp contract test..."
	@go test -tags tzcontract -count=1 -timeout 15m -v \
		./pkg/hub/tzcontract/... > /tmp/test-tz-contract.log 2>&1; \
	status=$$?; \
	cat /tmp/test-tz-contract.log; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	if [ -n "$$SCION_TEST_POSTGRES_URL" ] && \
		! grep -qE '^[[:space:]]*--- PASS: TestTimestampContract/postgres ' /tmp/test-tz-contract.log; then \
		echo "ERROR: SCION_TEST_POSTGRES_URL is set but the Postgres contract cases did not run." >&2; \
		exit 1; \
	fi

## vet: Run go vet
vet:
	@go vet ./...

## lint: Run go vet (no SQLite, memory-safe)
lint:
	@go vet -tags no_sqlite ./...

## vet-integration: Compile-check integration-tagged code (go vet -tags 'integration volume_test tzcontract')
# Catches build breaks in integration-tagged files that other vet/lint
# targets skip (ptone/scion#2348).
vet-integration:
	@go vet -tags 'integration volume_test tzcontract' ./...

## lint-ci: lint without the pkg/hub package itself (CI vets it per shard: vet-hub-shard)
lint-ci:
	@pkgs=$$($(call non_hub_pkgs,no_sqlite)) || exit 1; \
	go vet -tags no_sqlite $$pkgs

## vet-integration-ci: vet-integration without the pkg/hub package itself (CI vets it per shard: vet-hub-shard)
vet-integration-ci:
	@pkgs=$$($(call non_hub_pkgs,integration volume_test tzcontract)) || exit 1; \
	go vet -tags 'integration volume_test tzcontract' $$pkgs

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

## check-setenv-guard: Flag silenced os.Setenv of security-sensitive env vars
check-setenv-guard:
	@./hack/check-setenv-guard.sh

## check-conversation-upsert-guard: Verify UpsertConversationByExternalRef is only called from pkg/messaging and pkg/store
check-conversation-upsert-guard:
	@./hack/check-conversation-upsert-guard.sh

## check-hub-store-reassign: Flag new direct srv.store reassignments in pkg/hub tests (baseline: hack/hub-store-reassign-baseline.txt)
check-hub-store-reassign:
	@./hack/check-hub-store-reassign.sh

## check-store-delete-callers: Flag direct store DeleteAgent/DeleteProject calls outside hack/store-delete-callers-allowlist.txt
check-store-delete-callers:
	@./hack/check-store-delete-callers.sh

## check-security-marker-gates: Verify security symbols (authenticatedSender, validateDefaultAgent, ActionAttach) remain in handler code
check-security-marker-gates:
	@./hack/check-security-marker-gates.sh

## cli-time-zones: Verify every absolute time layout in CLI code (cmd/, pkg/agent/list.go) has a zone token
# NOTE: same caveat as check-authz-guards above -- make collapses the
# script's exit 1 (violations) and exit 3/4 (nothing was analysed) into one
# code. CI invokes the script directly to tell those apart.
cli-time-zones:
	@./hack/check-cli-time-zones.sh

## time-literals: Verify server-side times are formatted in UTC and bound in each column family's canonical form
# NOTE: same caveat as check-authz-guards above -- make collapses the
# script's exit 1 (violations) and exit 3/4 (nothing was analysed) into one
# code. CI invokes the script directly to tell those apart.
time-literals:
	@./hack/check-time-literals.sh

## check-harness-coverage: Verify every harnesses/<name>/Dockerfile has a build step in each full-catalog cloudbuild-*.yaml
# NOTE: same caveat as check-authz-guards above -- make collapses the
# script's exit 1 (a harness is out of sync) and exit 2 (could not run) into
# one code. CI invokes the script directly to tell those apart.
check-harness-coverage:
	@./image-build/scripts/check-harness-coverage.sh

## check-image-go-dirs: Verify every Dockerfile that builds the root Go module copies all of its top-level Go dirs (ptone/scion#5453)
# NOTE: same caveat as check-authz-guards above -- make collapses the
# script's exit 1 (violations) and exit 2 (could not run) into one code.
# CI invokes the script directly to tell those apart.
check-image-go-dirs:
	@./hack/check-image-go-dirs.sh --self-test
	@./hack/check-image-go-dirs.sh

## check-authorization-catalog: Validate authorization operation catalog, permission coverage, and generated report
check-authorization-catalog:
	@./hack/check-authorization-catalog.sh

## check-route-authz-manifest: Verify all routes declare authorization posture (#598)
check-route-authz-manifest:
	@./hack/check-route-authz-manifest.sh

## check-method-not-allowed: Flag 405 responses without Allow (bare MethodNotAllowed(w) in pkg/hub and pkg/runtimebroker; direct StatusMethodNotAllowed writes in pkg/sciontool and extras)
# NOTE: same caveat as check-authz-guards above -- make collapses the
# script's exit 1 (violations) and exit 3/4 (nothing was analysed) into one
# code. CI invokes the script directly to tell those apart.
check-method-not-allowed:
	@./hack/check-method-not-allowed.sh

## check-debug-defaults: Verify setup scripts, deploy templates and hosted setup docs keep debug off by default
check-debug-defaults:
	@./hack/check-debug-defaults.sh --self-test
	@./hack/check-debug-defaults.sh

## check-sqlite-coverage: Verify every package with a !no_sqlite test file is tested with SQLite by a blocking CI job
# NOTE: same caveat as check-authz-guards above -- make collapses the
# script's exit codes. CI invokes the script directly.
check-sqlite-coverage:
	@./hack/check-sqlite-coverage.sh --self-test
	@./hack/check-sqlite-coverage.sh

## check-custom: Run all custom CI lint checks (see hack/LINT-CONVENTIONS.md)
check-custom: hubshard-parity testpart-selftest compat-literals check-annotation-prefix check-authz-guards check-setenv-guard check-conversation-upsert-guard check-hub-store-reassign check-store-delete-callers check-security-marker-gates check-image-go-dirs check-authorization-catalog check-route-authz-manifest cli-time-zones time-literals check-method-not-allowed check-debug-defaults check-sqlite-coverage
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

## web-budget: Check the built web bundle against web/bundle-budget.json (run after `make web`)
web-budget:
	@echo "Checking web bundle budget..."
	@cd web && node scripts/check-bundle-budget.mjs
	@cd web && node --test scripts/check-bundle-budget.test.mjs
	@echo "Web bundle budget OK."

## web-typecheck: Run TypeScript type checking on the web frontend
web-typecheck:
	@echo "Type-checking web frontend..."
	@cd web && npm run typecheck
	@echo "Type check passed."

## web-lint: Run ESLint on the web frontend sources and Playwright e2e specs
web-lint:
	@echo "Linting web frontend..."
	@cd web && npm run lint
	@cd web && npm run lint:e2e
	@echo "Web lint passed."

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
ci-full: fmt-check web web-budget web-typecheck web-lint web-test lint vet-integration vet-integration-extras check-custom golangci-lint test-fast test-fixture-coverage build
	@echo ""
	@echo "CI (full) passed."

## proto: Generate Go code from .proto files
proto:
	@echo "Generating protobuf Go code..."
	@protoc \
		--proto_path=proto \
		--go_out=. --go_opt=module=github.com/GoogleCloudPlatform/scion \
		--go-grpc_out=. --go-grpc_opt=module=github.com/GoogleCloudPlatform/scion \
		proto/broker/v1/broker.proto proto/conduit/v1/conduit.proto
	@echo "Proto generation done."

## ent-check: Verify generated ent code (pkg/ent) matches pkg/ent/schema
# Regenerates in place, then fails if anything under pkg/ent (tracked diff
# or new untracked files -- a new schema adds new directories, which
# git diff alone does not see) or go.mod/go.sum changed. The generator is
# declared as a module tool (the tool directive in go.mod) and runs through
# go tool ent, so go mod tidy keeps the go.sum lines it needs (see tidy-check;
# ptone/scion#4799). Run on a clean pkg/ent tree:
# uncommitted pkg/ent edits read as drift (ptone/scion#2746).
# Exit codes: 1 = drift; 3 = go generate itself failed (not drift), shown by
# make as "Error 3". Besides a schema compile or module download error, exit 3
# also covers a go.sum entry the generator needs but is missing: go tool runs
# read-only, so it fails instead of adding it (run make tidy-check).
# Keep in sync with the ent-generate-check step in .github/workflows/ci.yml:
# it greps for the "go generate ./pkg/ent failed (not drift)" line below.
ent-check:
	@echo "Checking ent generated code is up to date..."
	@go generate ./pkg/ent || { echo "go generate ./pkg/ent failed (not drift)"; exit 3; }
	@untracked="$$(git status --porcelain --untracked-files=all -- pkg/ent | grep '^??' || true)"; \
	if ! git diff --exit-code --stat -- pkg/ent go.mod go.sum || [ -n "$$untracked" ]; then \
		[ -z "$$untracked" ] || { echo "Untracked generated files:"; echo "$$untracked"; }; \
		echo "ent generated code is out of date. Run 'go generate ./pkg/ent' and commit the result."; \
		exit 1; \
	fi; \
	echo "ent generated code is up to date."

## tidy-check: Verify go.mod/go.sum are tidy (go mod tidy would change nothing)
# Uses go mod tidy -diff, which prints the edits tidy would make and changes no
# files, so it does not depend on the state of the working tree. Catches a
# go.mod/go.sum that disagrees with tidy, for example go.sum lines that a tool
# or generator needs but tidy would drop (ptone/scion#4799).
# Exit codes: 1 = not tidy; 3 = go mod tidy itself failed (not drift), shown by
# make as "Error 3".
# Keep in sync with the tidy-check step in .github/workflows/ci.yml: it greps
# for the "go mod tidy -diff failed (not drift)" line below.
tidy-check:
	@echo "Checking go.mod/go.sum are tidy..."
	@out="$$(mktemp)"; \
	go mod tidy -diff >"$$out"; rc=$$?; \
	if [ "$$rc" -eq 0 ]; then rm -f "$$out"; echo "go.mod/go.sum are tidy."; exit 0; fi; \
	if [ ! -s "$$out" ]; then rm -f "$$out"; echo "go mod tidy -diff failed (not drift)"; exit 3; fi; \
	cat "$$out"; rm -f "$$out"; \
	echo "go.mod/go.sum are not tidy. Run 'go mod tidy' and commit the result."; \
	exit 1

## proto-check: Verify generated protobuf code is up to date
proto-check:
	@echo "Checking protobuf generated code is up to date..."
	@TMP=$$(mktemp -d) && \
	protoc \
		--proto_path=proto \
		--go_out=$$TMP --go_opt=module=github.com/GoogleCloudPlatform/scion \
		--go-grpc_out=$$TMP --go-grpc_opt=module=github.com/GoogleCloudPlatform/scion \
		proto/broker/v1/broker.proto proto/conduit/v1/conduit.proto && \
	diff $$TMP/proto/broker/v1/broker.pb.go proto/broker/v1/broker.pb.go && \
	diff $$TMP/proto/broker/v1/broker_grpc.pb.go proto/broker/v1/broker_grpc.pb.go && \
	diff $$TMP/proto/conduit/v1/conduit.pb.go proto/conduit/v1/conduit.pb.go && \
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
