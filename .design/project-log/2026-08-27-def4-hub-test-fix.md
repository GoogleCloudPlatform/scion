# DEF-4: Fix pkg/hub test suite SQLite OOM failures

**Date:** 2026-08-27
**Branch:** scion/ca-msg-em4

## Problem

The `pkg/hub` test suite suffered progressive SQLite "out of memory (7)" failures
when running the full suite (`go test -count=1 ./pkg/hub/`). Individual test files
passed in isolation, but the full package run failed with 17–20 non-deterministic
test failures whose membership changed between runs.

## Root Cause

There were 66+ calls to `newTestStore(":memory:")` across pkg/hub test files. The
function created a fresh in-memory SQLite database with full Ent migration (49+
schemas/tables) for each call. Only ~19 call sites had any `Close()` or
`t.Cleanup()` call. Because unclosed stores kept their in-memory databases alive
for the entire package test run, every leaked database accumulated memory. As the
messaging-v2 branch added new tables (conversations, conversation_participants,
message_addressees), per-database memory cost rose past the point where SQLite
triggered internal OOM errors (error code 7).

## Fix

1. **Refactored `newTestStore`** in `pkg/hub/teststore_test.go` to accept
   `*testing.T` as the first parameter and register `t.Cleanup(func() { _ = s.Close() })`
   internally. This ensures every test store is automatically closed when the
   test completes, preventing future regressions.

2. **Updated all 66 call sites** across 31 test files to pass `t` as the first
   argument: `newTestStore(t, ":memory:")`.

3. **Removed redundant close calls** in files that already had explicit
   `defer db.Close()` or `t.Cleanup(func() { _ = s.Close() })` after
   `newTestStore`, since the function now handles cleanup internally. Affected
   files: `brokerclient_test.go` (6), `stalled_detection_test.go` (4),
   `signing_key_shared_test.go` (2), `chat_notifications_test.go` (1),
   `handlers_policies_test.go` (1), `harness_config_handlers_image_test.go` (1),
   `heartbeat_timeout_test.go` (1), `notifications_test.go` (1),
   `skill_federation_test.go` (1), `skill_registry_handlers_test.go` (1).

## Scope

Test-only change. No production code was modified.

## Verification

All verification runs were green after the fix:

| Run | Command | Result | Duration |
|-----|---------|--------|----------|
| 1 | `go test -count=1 ./pkg/hub/` | PASS | 212.9s |
| 2 | `go test -count=1 -v ./pkg/hub/` | PASS | 213.4s |
| 3 | `go test -count=3 -timeout 30m ./pkg/hub/` | PASS | 655.0s |
| 4 | `go test -count=1 -timeout 15m ./pkg/hub/` | PASS | 213.1s |

### Causal Evidence

After verifying the fix, the `t.Cleanup` was temporarily reverted (removed) from
`newTestStore`. The full suite immediately failed with 20 tests hitting SQLite
`out of memory (7)` errors, confirming the fix is load-bearing:

```
--- FAIL: TestRepairStorage_FixesMissingObjects (0.02s)
--- FAIL: TestSAAudit_AgentCreate_RecordsTheDeny (0.01s)
--- FAIL: TestServer_UserTokenSurvivesRestart (0.01s)
--- FAIL: TestServer_GenerateAgentToken_NoDevAuthDoesNotAutoGrant (0.00s)
--- FAIL: TestServer_GenerateAgentToken_RoleReadOnly (0.01s)
--- FAIL: TestServer_GCPBackendFailureIsFatal (0.01s)
--- FAIL: TestSkillsResolve_GHCacheHitOnSecondResolve (0.00s)
--- FAIL: TestMultipartPublish_HappyPath (0.01s)
--- FAIL: TestBootstrapTemplatesFromDir_MultipleTemplates (0.01s)
... (20 failures total, all with "out of memory (7)")
```

The fix was then restored, and the suite passed again on subsequent runs.

## Files Changed (32 files)

- `pkg/hub/teststore_test.go` — core fix: added `*testing.T` param and `t.Cleanup`
- 31 test files — updated call sites and removed redundant close calls
