# M6: Lifecycle Hooks — Integration Tests, Docs, and Hardening

**Date**: 2026-06-08
**Agent**: lh-port-m6
**Branch**: scion/lifecycle-hooks-port

## Summary

Completed the final milestone (M6) for the lifecycle hooks feature port
(issue #35). Three tasks: end-to-end integration test, admin docs, and
CAS hardening.

## Changes

### Task 1: End-to-end integration test
- **File**: `pkg/hub/lifecycle_hook_integration_test.go`
- Two test cases: `TestLifecycleHookIntegration_RegisterDeregisterFlow` and
  `TestLifecycleHookIntegration_SuspendedAndErrorDeregister`
- Wires real `LifecycleHookEvaluator` + `HTTPExecutor` + ent-based test store
  + `ChannelEventPublisher` + httptest mock registry
- Validates register-on-running (POST), deregister-on-stopped/suspended/error
  (DELETE), bearer token injection, body variable substitution, audit events
- Adapted from reference: uses `newTestStore` (ent/enttest) instead of the
  removed `sqlite.New`; reuses existing `mockTokenGenerator` from executor test

### Task 2: Admin documentation
- **File**: `docs/lifecycle-hooks.md`
- Ported from reference with status updated: HA de-duplication is now
  **implemented** (was "pending" in reference)
- Documents: Postgres auto-selects durable store-backed CAS deduper
  (detected from PostgresEventPublisher) for exactly-once firing;
  SQLite/dev uses in-memory deduper
- Covers: CRUD API, triggers, action types, execution identity, variable
  trust model, SSRF policy, audit-no-body invariants, selector, examples

### Task 3: CAS hardening
- **File**: `pkg/store/entadapter/lifecyclehook_store.go`
- On Postgres, concurrent first-insert race in `CompareAndSetHookPhase`
  previously returned a constraint error to the losing instance (safe but noisy)
- Now catches `ent.IsConstraintError` and returns `changed=false, nil`
- Added `TestCompareAndSetHookPhase_ConcurrentFirstInsertRace` documenting
  the contract (with note that true PG concurrency can't be reproduced on
  SQLite unit tests)

## Verification

- `go build ./...` — clean
- `go test ./pkg/lifecyclehooks/... ./pkg/store/... ./pkg/hub/ -run "LifecycleHook|SSRF|IsBlocked|HookPhase"` — all pass
- Same with `-race` — all pass, no data races

## Deviations

- Used `mockTokenGenerator` (from `lifecycle_hook_executor_test.go`) instead
  of `mockGCPTokenGenerator` (from `handlers_gcp_identity_test.go`) for the
  integration test because it supports configurable access tokens needed for
  bearer-token assertions. Both types already exist in the same package;
  no new mock types were defined.
