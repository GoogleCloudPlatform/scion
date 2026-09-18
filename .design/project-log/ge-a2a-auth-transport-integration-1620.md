# Final Deterministic Integration Harness (#1620)

**Date:** 2026-09-18
**Branch:** `scion/dev-postmerge-integration-harness`
**Issue:** #1620
**Base:** `7c0e3a26856136975a479715724db37fabde1bfa` (exact upstream main tip with PRs #1741, #1742, #1743 merged)

## Summary

Ported the complete, deterministic, real-process integration test harness for Gemini Enterprise A2A (#1620) onto merged `main`. The integration harness executes against real subprocess topologies (Hub, Bridge replicas, Alternator proxy, Fake Google JWKS) with real loopback networking, authenticated gRPC transport, and real PostgreSQL database instances.

Zero production Go code was modified, preserving the post-merge tree integrity. All local deterministic test layers pass cleanly under single-run (16/16 tests), `-race`, and `-count=3` stress loops. External Gemini Enterprise envelope capture remains marked `external-live-only` / `passing: false` in `testdata/acceptance_layers.json`.

## Scope & Boundaries Followed

1. **Zero production Go changes:** No changes to `pkg/`, `cmd/`, or `extras/scion-a2a-bridge/internal/`. No `Milliseconds()` addition to `pgstore.go`.
2. **Diagnostic mock isolation:** Line 166 of `extras/scion-a2a-bridge/internal/bridge/followup_test.go` was kept completely untouched in this branch (managed on `scion/dev-postmerge-mock-dedup`).
3. **Deterministic lease and crash synchronization:** Replaced the legacy sleep in `TestCrashLeaseBoundary` with bounded active PostgreSQL polling on `exec_heartbeat < NOW() - interval '2 seconds'`. Replaced the post-terminal sleep with deterministic observable synchronization (`/__test/janitor-cycle`) across both replicas, with causal regression assertions verifying `reapedCount == 0` and zero Hub message replay before manual retry.
4. **Exact cursor/reconnect verification:** Proved intermediate event absence from snapshot history, verified `a2a_task_events.id > a2a_sdk_tasks.last_event_cursor`, single stream delivery, absence of duplicate replay, and verified `_bridgeEventID` never leaks.
5. **Process & resource hygiene:** Kernel-allocated dynamic ports (`127.0.0.1:0`), process tree termination and wait reaping on test completion, per-test PostgreSQL schema isolation (`harness_run_*`), and verification of undisturbed canary data in `test_canary.sentinel`.
6. **Matrix & documentation reflection:** `testdata/acceptance_layers.json` and docs reflect actual local deterministic proof. Live external Gemini Enterprise capture remains marked `external-live-only` / `passing: false`.
7. **Test-only dependency closure:** The `go.mod`/`go.sum` additions in `extras/scion-a2a-bridge/` are strictly limited to the exact transitive dependency closure required by the test-only modernc SQLite driver (`modernc.org/sqlite`, `modernc.org/libc`, `modernc.org/mathutil`, `modernc.org/memory`, `github.com/dustin/go-humanize`, `github.com/ncruces/go-strftime`, `github.com/remyoudompheng/bigfft`). These are needed solely by `serveHubProcess` to back the Hub ent adapter with a temporary SQLite database (`SCION_TEST_HUB_DATABASE`) without external dependencies. Zero production code references these drivers.

## Acceptance Layers & Scope

| Layer | Focus Area | Status | Evidence |
|---|---|---|---|
| **Layer 1** | Auth, Cache, Transport | LOCAL PASS (Parent partial) | `TestColdReplicaAndRotation`, `TestControlPlanePrincipalIsolation`, `TestCredentialRedaction` (per-replica exchange counts, token rotation, Hub restart over durable SQLite identity, 6 unary gRPC principal isolations). `actual-ge-capture` remains `external-live-only` / `passing: false`. |
| **Layer 2** | HA Lifecycle | PASS | `TestTwoReplicaUserLifecycle` (input-required boundary, continuation across replicas, cancellation with waiter termination, no post-cancel replay, caller/project/agent authorization boundaries). |
| **Layer 3** | Cursor & Reconnect | PASS | `TestCrossReplicaStreamCursor` (intermediate event absent from snapshot payload/history, DB `event_id > last_event_cursor`, streams exactly once, no old replay via `assertNoSSE`, internal `_bridgeEventID` stripped). |
| **Layer 4** | Crash & Lease Boundary | PASS | `TestCrashLeaseBoundary` (observable DB heartbeat lease expiration query without fixed sleep, replica crash/restart, post-terminal janitor observable synchronization across replicas, terminal state transition verified without phantom success or automatic replay). |
| **Layer 5** | Process & Resource Hygiene | PASS | `TestProcessTopologyStartsDistinctProcessesAndCancelsThem`, `TestLoadAlternatorUsesRealProcessesAndPinsSSE`, `TestDatabaseRunNamingAndCleanup`, `TestPostgreSQLSchemaAllocator` (dynamic ports, process reaping, schema isolation, canary survival). |
| **Layer 6** | Matrix & Docs Reflection | PASS | `TestAcceptanceLayersMatchProvenScope`, `TestFixtureMatricesContainApprovedCategories`, `testdata/acceptance_layers.json` (reflects real evidence; external GE live capture remains false). |

## Test Verification Runs

### Full Suite Run (16/16 PASS)
```
$ TEST_DATABASE_URL="postgres://scion:scion@127.0.0.1:5432/a2a_test?sslmode=disable" go test -v -buildvcs=false ./integration
=== RUN   TestHarnessHelperProcess
--- PASS: TestHarnessHelperProcess (0.00s)
=== RUN   TestColdReplicaAndRotation
--- PASS: TestColdReplicaAndRotation (3.55s)
=== RUN   TestGEEnvelopeCompatibility
--- PASS: TestGEEnvelopeCompatibility (2.64s)
=== RUN   TestControlPlanePrincipalIsolation
--- PASS: TestControlPlanePrincipalIsolation (0.26s)
=== RUN   TestCombinedStartupMatrix
--- PASS: TestCombinedStartupMatrix (0.05s)
=== RUN   TestCredentialRedaction
--- PASS: TestCredentialRedaction (2.44s)
=== RUN   TestTwoReplicaUserLifecycle
--- PASS: TestTwoReplicaUserLifecycle (0.92s)
=== RUN   TestCrossReplicaStreamCursor
--- PASS: TestCrossReplicaStreamCursor (1.55s)
=== RUN   TestCrashLeaseBoundary
--- PASS: TestCrashLeaseBoundary (3.13s)
=== RUN   TestProcessTopologyStartsDistinctProcessesAndCancelsThem
--- PASS: TestProcessTopologyStartsDistinctProcessesAndCancelsThem (0.00s)
=== RUN   TestLoadAlternatorUsesRealProcessesAndPinsSSE
--- PASS: TestLoadAlternatorUsesRealProcessesAndPinsSSE (0.42s)
=== RUN   TestCredentialRedactionFoundation
--- PASS: TestCredentialRedactionFoundation (0.00s)
=== RUN   TestFixtureMatricesContainApprovedCategories
--- PASS: TestFixtureMatricesContainApprovedCategories (0.00s)
=== RUN   TestDatabaseRunNamingAndCleanup
--- PASS: TestDatabaseRunNamingAndCleanup (0.00s)
=== RUN   TestPostgreSQLSchemaAllocator
--- PASS: TestPostgreSQLSchemaAllocator (0.01s)
=== RUN   TestAcceptanceLayersMatchProvenScope
--- PASS: TestAcceptanceLayersMatchProvenScope (0.00s)
PASS
ok  	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/integration	15.12s
```

### Race Detector Run
```
$ TEST_DATABASE_URL="postgres://scion:scion@127.0.0.1:5432/a2a_test?sslmode=disable" go test -race -buildvcs=false ./integration
ok  	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/integration	31.287s
```

### Repetition / Stress Run
```
$ TEST_DATABASE_URL="postgres://scion:scion@127.0.0.1:5432/a2a_test?sslmode=disable" go test -count=3 -buildvcs=false ./integration
ok  	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/integration	45.940s
```

### Canary Sentinel Verification
```
$ psql "postgres://scion:scion@127.0.0.1:5432/a2a_test?sslmode=disable" -c "SELECT * FROM test_canary.sentinel;"
    id    |    value
----------+--------------
 canary-1 | must-survive
(1 row)
```

## Files Committed
- `extras/scion-a2a-bridge/go.mod` (transitive test-only closure for modernc sqlite)
- `extras/scion-a2a-bridge/go.sum`
- `extras/scion-a2a-bridge/integration/README.md`
- `extras/scion-a2a-bridge/integration/alternator_harness_test.go`
- `extras/scion-a2a-bridge/integration/auth_transport_process_test.go`
- `extras/scion-a2a-bridge/integration/fixture_harness_test.go`
- `extras/scion-a2a-bridge/integration/ha_final_process_test.go`
- `extras/scion-a2a-bridge/integration/harness_behavior_test.go`
- `extras/scion-a2a-bridge/integration/postgres_harness_test.go`
- `extras/scion-a2a-bridge/integration/redaction_harness_test.go`
- `extras/scion-a2a-bridge/integration/topology_harness_test.go`
- `extras/scion-a2a-bridge/integration/testdata/*`
- `.design/project-log/ge-a2a-auth-transport-integration-1620.md`
