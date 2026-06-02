# Fix: Test suite leaking Hub credentials (issue #123)

**Date**: 2026-06-02  
**PR**: #125  
**Issue**: #123

## Problem

When `go test` runs inside an agent container, tests inherit live Hub env vars. `TestInitCommand_Integration` builds and spawns a real sciontool binary that inherits these vars, causing the child process to report status to the real Hub and corrupt agent state (resetting phase to "starting"). This is how dev-issue-71b got stuck.

## Fix

1. Added `scrubHubEnv(t)` helpers using `t.Setenv` for automatic cleanup in:
   - `cmd/sciontool/commands/init_test.go` (primary subprocess fix)
   - `pkg/sciontool/hooks/handlers/hub_test.go` (env var hygiene)
   - `pkg/sciontool/hub/client_test.go` (env var hygiene)

2. Added `filterHubEnv(env)` to explicitly strip Hub vars from subprocess environments.

3. Converted all `os.Setenv`/`os.Unsetenv` patterns to `t.Setenv` in hub-related test files for crash-safe env isolation.

## Observations

- The Hub env var list (`SCION_HUB_ENDPOINT`, `SCION_HUB_URL`, `SCION_AUTH_TOKEN`, `SCION_AGENT_ID`, `SCION_AGENT_MODE`) is defined in `pkg/sciontool/hub/client.go:45-56`. The `scrubHubEnv` helpers are inlined in each test file rather than shared, to avoid importing `testing` into production code.
- Pre-existing CI issue: `pkg/hub/resource_import_handler_test.go` has an undefined `mockRoundTripper` symbol that causes `go vet ./...` to fail — not related to this change.
