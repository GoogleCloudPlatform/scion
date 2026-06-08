# M4 Lifecycle Hook Evaluator Port — Project Log

**Agent:** lh-port-m4  
**Date:** 2026-06-08  
**Milestone:** M4 — Evaluator  

## Summary

Ported the lifecycle hook evaluator from the reference branch (`origin/scion/architect-lifecycle-hooks`) to main's event system, with mandatory adaptations for HA/multi-instance safety.

## Files Changed

- `pkg/hub/lifecycle_hook_evaluator.go` — New file: evaluator, deduper, executor interface, LoggingExecutor
- `pkg/hub/lifecycle_hook_evaluator_test.go` — New file: 30+ tests covering all evaluator behaviors
- `pkg/hub/server.go` — Added `lifecycleHookEvaluator` field, `StartLifecycleHookEvaluator` method, wiring in `StartBackgroundServices`, shutdown in `Shutdown` and `CleanupResources`

## Key Adaptations from Reference

### 1. EventPublisher Interface (CRITICAL)
The reference hard-typed the events field as `*ChannelEventPublisher`. Changed to accept the `EventPublisher` interface so the evaluator works with both:
- `*ChannelEventPublisher` (dev/sqlite single-instance)
- `*PostgresEventPublisher` (HA/production — broadcasts via Postgres NOTIFY)

This is essential because in HA mode, PostgresEventPublisher broadcasts every event to ALL hub instances. Without the store-backed CAS deduper, every instance would fire every hook.

### 2. Backend-Aware Deduplication
- **Postgres** (`WithDBDriver("postgres")`): Uses `storeDeduper` backed by `store.CompareAndSetHookPhase` / `store.DeleteHookPhase` from M1. Exactly one CAS winner per transition across all replicas.
- **SQLite/default**: Uses `memoryDeduper` with in-memory map, seeded from store on Start() to prevent spurious fires after restart.

### 3. Defensive Error Handling
- CAS errors are logged and SKIPPED — never abort/block the transition
- Executor errors are logged, not propagated
- Executor panics are recovered — evaluator continues to next hook
- All error paths include structured logging with agent_id, hook_id, phase

### 4. Event Subjects
Subscribes to `project.*.agent.status` and `project.*.agent.deleted` (confirmed these exist on main via events.go PublishAgentStatus/PublishAgentDeleted). Uses `*` wildcard (not `>`) to avoid cross-matching.

### 5. Executor Boundary for M5
- Defined `LifecycleHookExecutor` interface at top of evaluator file
- `LoggingExecutor` as no-op default (logs hook fires without HTTP action)
- `NewLifecycleHookEvaluator` defaults to `LoggingExecutor` when executor is nil
- M5 will implement `HTTPExecutor` using the same interface — no evaluator changes needed

### 6. Server Wiring
- Mirrors `StartNotificationDispatcher` pattern: guarded by `noopEventPublisher` check, idempotent via nil-check
- Stopped before event publisher in both `Shutdown` and `CleanupResources`
- `StartLifecycleHookEvaluator(opts ...EvaluatorOption)` accepts WithDBDriver for cmd-level callers

### 7. Test Store Adaptation
The reference tests used `sqlite.New(":memory:")` which doesn't exist on main's ent-only store. Adapted to use `newTestStore(":memory:")` from `teststore_test.go`. Renamed test helpers (`seedHookProject`, `seedHookAgent`, `seedLifecycleHook`) to avoid collisions with `dispatch_exec_test.go`'s `seedAgent`.

## Test Results
- `go build ./...` — clean
- `go test ./pkg/hub/ -run LifecycleHook` — 30+ tests pass
- `go test -race ./pkg/hub/ -run LifecycleHook` — clean (no races)

## Deviations from Task Notes
- The task mentioned `s.config.DatabaseDriver` but `ServerConfig` has no such field. The evaluator detects postgres via the `WithDBDriver` option (which cmd/server_foreground.go can pass). The default `StartLifecycleHookEvaluator()` in `StartBackgroundServices` uses the in-memory deduper; callers at the cmd level can pass `WithDBDriver("postgres")` when they know the backend.
- No modifications to cmd/server_foreground.go — that wiring belongs to the integration step after M4/M5 are both in.
