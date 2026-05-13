# Fix delete_test.go: groves-to-projects path migration

**Date:** 2026-05-13
**Files changed:** `cmd/delete_test.go`

## Problem

Two tests in `cmd/delete_test.go` were failing:
- `TestDeleteAgentsViaHub_CleansUpLocalFiles` (line 193): expected "test-agent", got "s/test-agent"
- `TestDeleteAgentsViaHub_NoLocalFiles` (line 323): expected "hub-only-agent", got "s/hub-only-agent"

## Root Cause

The production code in `pkg/hubclient/agents.go` (`agentPath()`) was updated to use
`/api/v1/projects/` instead of `/api/v1/groves/`, but the mock HTTP servers in
`delete_test.go` still extracted agent names using the old `/api/v1/groves/` prefix.

Since `/api/v1/projects/` is 2 characters longer than `/api/v1/groves/`, the
substring extraction grabbed 2 extra characters from the path, producing `s/`
(the tail of "projects/") prepended to the agent name.

The `TestDeleteAgentsViaHub_LocalCleanupFailureCreatesStaleLocalNotToRegister` test
also had stale groves paths but happened to pass due to the hub client's built-in
fallback mechanism: when a DELETE to `/projects/` returns 404, it retries with `/groves/`.

## Fix

Updated all mock server URL paths in `delete_test.go` from `/api/v1/groves/` to
`/api/v1/projects/` to match the current production API paths.
