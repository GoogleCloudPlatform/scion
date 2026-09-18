# Final Deterministic Integration Harness (#1620)

**Date:** 2026-09-18
**Branch:** `scion/dev-postmerge-integration-harness-final-fixes`
**Issue:** #1620
**Merge base:** `0c07fdee3eefac5a21f93878211bf507b2e5c6a7` (current merged `main`, including PR #1746)

## Summary

Ported the complete, deterministic, real-process integration test harness for Gemini Enterprise A2A (#1620) onto merged `main`. The integration harness executes against real subprocess topologies (Hub, Bridge replicas, Alternator proxy, Fake Google JWKS) with real loopback networking, authenticated gRPC transport, and real PostgreSQL database instances.

Zero production Go code was modified, preserving the post-merge tree integrity. All 20 local deterministic test cases pass cleanly under the retained single-run, `-race`, and `-count=3` evidence. A dedicated fail-closed CI entry and runner script provision PostgreSQL 15 and fail closed if the database is missing. Example deployment manifests for Cloud Run and Kubernetes are verified via deterministic dry-run parsing tests. External live deployment and external Gemini Enterprise envelope capture remain marked `external-live-only` / `passing: false` in `testdata/acceptance_layers.json`.

## Scope & Boundaries Followed

1. **Zero production Go changes:** No changes to `pkg/`, `cmd/`, or `extras/scion-a2a-bridge/internal/`. No `Milliseconds()` addition to `pgstore.go`.
2. **Resolved diagnostic mock dedup (#1746):** Upstream PR #1746 merged the duplicate mock resolution on `extras/scion-a2a-bridge/internal/bridge/followup_test.go:166` into `main` at `0c07fdee3eefac5a21f93878211bf507b2e5c6a7`. Current `main` is merged cleanly into this branch with zero conflicts, allowing the full bridge package suite (`go test ./...`) to pass cleanly end-to-end.
3. **Deterministic lease and crash synchronization:** Replaced the legacy sleep in `TestCrashLeaseBoundary` with bounded active PostgreSQL polling on `exec_heartbeat < NOW() - interval '2 seconds'`. Replaced the post-terminal sleep with deterministic observable synchronization (`/__test/janitor-cycle`) across both replicas, with causal regression assertions verifying `reapedCount == 0` and zero Hub message replay before manual retry. `/__test/janitor-cycle` executes a deterministic maintenance pass (`ReapStaleTasks` then `RunSweep`). Scoped no-replay evidence is verified as terminal fencing plus post-terminal maintenance pass and unchanged Hub counter (not a global poll/executor callback barrier). Crash behavior is precisely verified as durable stale-task failure/reap plus visible terminal event and no automatic Hub replay (no successful execution reclaim is claimed).
4. **Exact cursor/reconnect verification:** Proved intermediate event absence from snapshot history, verified `a2a_task_events.id > a2a_sdk_tasks.last_event_cursor`, single stream delivery, absence of duplicate replay, and verified `_bridgeEventID` never leaks.
5. **gRPC BrokerService 6 Unary Methods:** `controlRPCErrors` explicitly verifies the six unary methods of `proto/broker/v1` `BrokerService`: `Configure`, `Publish`, `Subscribe`, `Unsubscribe`, `HealthCheck`, and `GetInfo`.
6. **Process & resource hygiene:** Kernel-allocated dynamic ports (`127.0.0.1:0`), process tree termination and wait reaping on test completion, per-test PostgreSQL schema isolation (`harness_run_*`), and verification of undisturbed canary data in `test_canary.sentinel`.
7. **CI fail-closed execution & runner verification:** Integration harness requires PostgreSQL 15 and `psql` when running under `CI=true` or `TEST_REQUIRE_DATABASE=1`, failing closed (`t.Fatal` / exit code 1) rather than silently skipping. Runner redacts all DSN credentials (`Using TEST_DATABASE_URL=[REDACTED]`), requires `psql`, validates exact pre- and post-test canary sentinel identity and value (`canary-1` -> `must-survive`) without modifying unrelated rows, and parses `go test -json` machine-readable output across all test phases (normal, race, repetition count=3) to fail closed on any `Action=skip`. Deliberate negative runner tests verified: absent DB (exit 1), missing psql (exit 1), injected test skip via `TestInjectedSkipFixture` (exit 1), and canary row mutation (exit 1).
8. **Deployment manifests and dry-run validation:** Cloud Run multi-instance (`deploy/cloudrun/service.yaml`) and Kubernetes multi-replica (`deploy/kubernetes/deployment.yaml`) manifests are deterministically validated via `TestCloudRunManifestValidation` and `TestKubernetesManifestValidation`. Documentation covers dual headers (`X-Serverless-Authorization` vs `Authorization`), principal separation (`hub-sa` vs `ge-invoker`), health/rollback/cleanup, and strict no-refresh-token OAuth exchange.
9. **Matrix & documentation reflection:** `testdata/acceptance_layers.json` and docs reflect actual local deterministic proof. Live external Gemini Enterprise capture, Cloud Run live deployment, and Kubernetes live deployment remain marked `external-live-only` / `passing: false`.
10. **Test-only dependency closure:** The `go.mod`/`go.sum` additions in `extras/scion-a2a-bridge/` are strictly limited to the exact transitive dependency closure required by the test-only modernc SQLite driver (`modernc.org/sqlite`, `modernc.org/libc`, `modernc.org/mathutil`, `modernc.org/memory`, `github.com/dustin/go-humanize`, `github.com/ncruces/go-strftime`, `github.com/remyoudompheng/bigfft`). These are needed solely by `serveHubProcess` to back the Hub ent adapter with a temporary SQLite database (`SCION_TEST_HUB_DATABASE`) without external dependencies. Zero production code references these drivers.

## Acceptance Layers & Scope

| Layer | Focus Area | Status | Evidence |
|---|---|---|---|
| **Layer 1** | Auth, Cache, Transport | LOCAL PASS (Parent partial) | `TestColdReplicaAndRotation`, `TestControlPlanePrincipalIsolation`, `TestCredentialRedaction` (per-replica exchange counts, token rotation, Hub restart over durable SQLite identity, 6 unary gRPC principal isolations: Configure, Publish, Subscribe, Unsubscribe, HealthCheck, GetInfo). `actual-ge-capture` remains `external-live-only` / `passing: false`. |
| **Layer 2** | HA Lifecycle | PASS | `TestTwoReplicaUserLifecycle` (input-required boundary, continuation across replicas, cancellation with waiter termination, no post-cancel replay, caller/project/agent authorization boundaries). |
| **Layer 3** | Cursor & Reconnect | PASS | `TestCrossReplicaStreamCursor` (intermediate event absent from snapshot payload/history, DB `event_id > last_event_cursor`, streams exactly once, no old replay via `assertNoSSE`, internal `_bridgeEventID` stripped). |
| **Layer 4** | Crash & Lease Boundary | PASS | `TestCrashLeaseBoundary` (observable DB heartbeat lease expiration query without fixed sleep, replica crash/restart, terminal fencing plus post-terminal maintenance pass via `/__test/janitor-cycle`, reapedCount==0, unchanged Hub message counter before manual retry, durable stale-task failure/reap plus visible terminal event and no automatic Hub replay). |
| **Layer 5** | Process & Resource Hygiene | PASS | `TestProcessTopologyStartsDistinctProcessesAndCancelsThem`, `TestLoadAlternatorUsesRealProcessesAndPinsSSE`, `TestDatabaseRunNamingAndCleanup`, `TestPostgreSQLSchemaAllocator` (dynamic ports, process reaping, schema isolation, canary survival). |
| **Layer 6** | Matrix & Docs Reflection | PASS | `TestAcceptanceLayersMatchProvenScope`, `TestFixtureMatricesContainApprovedCategories`, `testdata/acceptance_layers.json` (reflects real evidence; external GE live capture remains false). |
| **Layer 7** | Cloud Run Deployment Config | LOCAL PASS (Parent partial) | `TestCloudRunManifestValidation`, `deploy/cloudrun/service.yaml`, `docs/deployment.md` (`minScale: 2`, `h2c` port 8080, dual headers, `hub-sa` vs `ge-invoker` isolation, health/rollback/cleanup). External live deployment remains `external-live-only` / `passing: false`. |
| **Layer 8** | Kubernetes Deployment Config | LOCAL PASS (Parent partial) | `TestKubernetesManifestValidation`, `deploy/kubernetes/deployment.yaml`, `docs/deployment.md` (2+ replicas, RollingUpdate, `a2a-postgres-secret`, Service with `kubernetes.io/h2c`, Ingress, health/rollback/cleanup). External live deployment remains `external-live-only` / `passing: false`. |
| **Layer 9** | CI Automated Integration | PASS | `CIAutomatedPostgresIntegration`, `make test-a2a-integration`, `extras/scion-a2a-bridge/scripts/run-integration-ci.sh`, `.github/workflows/extras-ci.yml` (provisions PostgreSQL 15, sets `TEST_DATABASE_URL`, fails closed if DB missing). The workflow is configured and locally validated; no remote GitHub Actions execution is claimed. |

## Test Verification Runs

### Full Suite Run (20/20 PASS)
```
$ make test-a2a-integration
=== Phase 1: Standard Integration Suite ===
=== RUN   TestHarnessHelperProcess
--- PASS: TestHarnessHelperProcess (0.00s)
=== RUN   TestColdReplicaAndRotation
--- PASS: TestColdReplicaAndRotation (3.38s)
=== RUN   TestGEEnvelopeCompatibility
--- PASS: TestGEEnvelopeCompatibility (2.69s)
=== RUN   TestControlPlanePrincipalIsolation
--- PASS: TestControlPlanePrincipalIsolation (0.33s)
=== RUN   TestCombinedStartupMatrix
--- PASS: TestCombinedStartupMatrix (0.06s)
=== RUN   TestCredentialRedaction
--- PASS: TestCredentialRedaction (2.54s)
=== RUN   TestCloudRunManifestValidation
--- PASS: TestCloudRunManifestValidation (0.00s)
=== RUN   TestKubernetesManifestValidation
--- PASS: TestKubernetesManifestValidation (0.00s)
=== RUN   TestTwoReplicaUserLifecycle
--- PASS: TestTwoReplicaUserLifecycle (1.02s)
=== RUN   TestCrossReplicaStreamCursor
--- PASS: TestCrossReplicaStreamCursor (1.66s)
=== RUN   TestCrashLeaseBoundary
--- PASS: TestCrashLeaseBoundary (2.98s)
=== RUN   TestProcessTopologyStartsDistinctProcessesAndCancelsThem
--- PASS: TestProcessTopologyStartsDistinctProcessesAndCancelsThem (0.00s)
=== RUN   TestLoadAlternatorUsesRealProcessesAndPinsSSE
--- PASS: TestLoadAlternatorUsesRealProcessesAndPinsSSE (0.29s)
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
=== RUN   TestSkippedAcceptanceDetectionNegative
--- PASS: TestSkippedAcceptanceDetectionNegative (0.00s)
=== RUN   TestInjectedSkipFixture
--- PASS: TestInjectedSkipFixture (0.00s)
PASS
ok  	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/integration	15.499s
```

### Race Detector Run
```
=== Phase 2: Race Detection Integration Suite ===
ok  	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/integration	31.115s
```

### Repetition / Stress Run
```
=== Phase 3: Repetition Stress Suite (count=3) ===
ok  	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/integration	42.657s
```

### Canary Sentinel Verification
```
=== Phase 4: Canary Sentinel Post-Verification ===
Pre-test canary sentinel 'canary-1' verified: 'must-survive'
Post-test canary sentinel 'canary-1' matches pre-test baseline: 'must-survive'
```

### Full Bridge Package Suite
```
$ cd extras/scion-a2a-bridge && go test ./...
?   	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/cmd/scion-a2a-bridge	[no test files]
ok  	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/integration	10.044s
ok  	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/bridge	35.422s
?   	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/identity	[no test files]
ok  	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state	(cached)

$ cd extras/scion-a2a-bridge && go test -race -count=1 ./internal/...
ok  	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/bridge	36.900s
?   	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/identity	[no test files]
ok  	github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state	1.542s
```

### Upstream Merge DAG
Current merged `main` and the exact branch merge-base are `0c07fdee3eefac5a21f93878211bf507b2e5c6a7` (incorporating PR #1746). That commit entered the accepted candidate's unchanged DAG as parent 2 of merge commit `98c996a5`; the correction starts from immutable candidate `e9b34d2af827cc5bc9ffc5b3d671591f324c0d70`:
```
* e9b34d2a fix(ge-a2a): require TEST_DATABASE_URL and fail closed without fallback (#1620)
* 756f0aa3 docs(ge-a2a): record merged main DAG and full bridge suite pass (#1620)
*   98c996a5 Merge remote-tracking branch 'origin/main' into scion/dev-postmerge-integration-harness
| \
| * 0c07fdee fix(a2a-bridge): remove duplicate mock Messaging declaration (#1746)
* | b86ccc32 test(ge-a2a): redact runner DSN, require psql, enforce canary baseline, and detect test skips (#1620)
* | 6e583e90 test(ge-a2a): add skipped acceptance detection negative test and refine maintenance pass wording (#1620)
* | b93a54ee test(ge-a2a): add fail-closed PG CI runner, deployment manifests, and validation (#1620)
```

## Files Committed
- `.github/workflows/extras-ci.yml` (added `a2a-bridge-postgres-integration` job with PostgreSQL 15 service)
- `Makefile` (added `test-a2a-integration` target)
- `extras/scion-a2a-bridge/deploy/cloudrun/service.yaml` (Cloud Run multi-instance manifest)
- `extras/scion-a2a-bridge/deploy/kubernetes/deployment.yaml` (Kubernetes 2+ replicas manifest)
- `extras/scion-a2a-bridge/docs/deployment.md` (Cloud Run, Kubernetes, OAuth client IDs, no refresh token)
- `extras/scion-a2a-bridge/docs/evidence-template.md` (Live qualification run template)
- `extras/scion-a2a-bridge/go.mod` (transitive test-only closure for modernc sqlite)
- `extras/scion-a2a-bridge/go.sum`
- `extras/scion-a2a-bridge/integration/README.md`
- `extras/scion-a2a-bridge/integration/alternator_harness_test.go`
- `extras/scion-a2a-bridge/integration/auth_transport_process_test.go`
- `extras/scion-a2a-bridge/integration/deployment_manifest_test.go`
- `extras/scion-a2a-bridge/integration/fixture_harness_test.go`
- `extras/scion-a2a-bridge/integration/ha_final_process_test.go`
- `extras/scion-a2a-bridge/integration/harness_behavior_test.go`
- `extras/scion-a2a-bridge/integration/postgres_harness_test.go`
- `extras/scion-a2a-bridge/integration/redaction_harness_test.go`
- `extras/scion-a2a-bridge/scripts/run-integration-ci.sh`
- `extras/scion-a2a-bridge/integration/topology_harness_test.go`
- `extras/scion-a2a-bridge/integration/testdata/*`
- `.design/project-log/ge-a2a-auth-transport-integration-1620.md`

## Final Gate Finding Resolution

| Finding | Resolution |
|---|---|
| Cloud Run `ingress: all` prerequisite | Resolved in the adjacent manifest comment and deployment guide: restrict `roles/run.invoker` to the distinct Hub and GE service accounts; never grant public principals; retain application JWT validation. |
| Mutable example images | Resolved: both manifests use explicit all-zero, nondeployable `@sha256:<64 hex>` placeholders, with substitution steps. Manifest tests reject `:latest` and require digest-form references. |
| Kubernetes TLS and GE principal | Resolved: the Ingress example has a TLS host/secret, tests assert host routing and TLS-host agreement, and comments/docs assign GE-principal enforcement to an identity-aware Ingress/Gateway, service-mesh policy, Workload Identity, or equivalent without claiming live enforcement. |
| Cloud Run GE/Hub separation | Resolved: manifest validation requires a nonempty `GE_INVOKER_SERVICE_ACCOUNT` distinct from every comma-separated `GRPC_AUTH_SUBJECTS` principal. Dual-header wire proof remains attributed to transport integration tests and documentation, not YAML parsing. |
| Project-log history/path accuracy | Resolved: runner path is `extras/scion-a2a-bridge/scripts/run-integration-ci.sh`; merge-base/current-main narrative is `0c07fdee`, with the existing merge DAG preserved exactly. |
| Durable report accuracy | Resolved in `/scion-volumes/scratchpad/projects/ge-a2a/dev-postmerge-integration-harness-report.md` after the correction commit and durability push, including the exact successor SHA and current-main diff count. |
| Security Info findings | Dispositioned with no runtime changes: the `http.DefaultTransport` mutation occurs only in an isolated test helper subprocess, so process isolation is the correctness boundary; ephemeral CI-only `scion/scion` PostgreSQL credentials are not reusable secrets; Kubernetes GE-principal documentation is resolved above. |

## Final Bounded Verification

- RED proof before manifest edits: `go test ./integration -run 'Test(CloudRun|Kubernetes)ManifestValidation' -count=1` failed on both `:latest` image references.
- Focused normal/YAML parse: the same command passed (`ok`, 0.138s); both YAML files were unmarshaled with `gopkg.in/yaml.v3` and all structural assertions passed.
- Focused race: `go test -race ./integration -run 'Test(CloudRun|Kubernetes)ManifestValidation' -count=1` passed (`ok`, 1.452s).
- One normal real-PostgreSQL run: `TEST_DATABASE_URL='postgres://scion:scion@127.0.0.1:5432/a2a_test?sslmode=disable' TEST_REQUIRE_DATABASE=1 go test -v ./integration -count=1` passed all 20 tests (`ok`, 13.753s). The local PostgreSQL 15 process was stopped afterward.
- `make fmt-check`, `make compat-literals`, `git diff --check 0c07fdee...HEAD`, and `git diff --check` all exited 0.
- `git diff --name-only e9b34d2a...` confirms the correction changes only manifests, their focused validation test, deployment documentation, and this project log; runtime topology tests and production Go remain untouched.

## PR #1748 Review-Fix Checkpoint (UNREVIEWED WIP)

**Date:** 2026-09-18  
**Temporary branch:** `scion/dev-postmerge-integration-harness-review-fixes`  
**Exact accepted base:** `29e8a45ccd11b4db77ea1fee04945917a129af94`

This is an urgent-cleanup checkpoint, not an approval or readiness claim. The scoped WIP:

- guarantees fallback closure of `storePre` and the two explicitly disconnected SSE bodies while retaining their intentional early-close timing;
- gives `run-integration-ci.sh` an invocation-owned `mktemp -d` directory with exact-path cleanup on exit and HUP/INT/TERM;
- adds deterministic fake-boundary regressions for success, empty DB URL, missing `psql`, early test failure, forbidden skip, canary mutation, signal interruption, concurrent isolation, unrelated-file preservation, and credential redaction; and
- scopes fail-closed database behavior to `TEST_REQUIRE_DATABASE=1`, so generic `CI=true` bridge jobs may skip PostgreSQL-only tests while the dedicated PostgreSQL runner still rejects every skip.

Completed evidence:

- RED: accepted tip failed the four PostgreSQL-only tests under `CI=true` without `TEST_DATABASE_URL`.
- RED: the new cleanup regressions caught the legacy signal leak `/tmp/ci-test-json.*` and absence of isolated concurrent runner directories.
- GREEN: focused runner regressions passed normally and with `-race`; generic `CI=true` four-test composition passed; `TEST_REQUIRE_DATABASE=1` without a DB still failed closed; `go vet ./integration` and `go test ./integration -count=1` passed.
- Upstream PR #1748 generic extras job `105790395454` failed only the four over-broad `CI=true` database requirements addressed here.
- Upstream dedicated PostgreSQL job `105790361080` passed standard, race, and count=3 phases with zero skips/failures and exact `canary-1=must-survive` preservation; its log redacted the DSN.
- Upstream Build & Test job `105790361187` showed root PersistentStore `unknown driver "sqlite"`, exchange-route permission classification, and other Hub/authz census failures. Per maintenance ownership, these remain the separate unmerged PR #1747 dependency at `22e66f275c6f545aa1694dfd388d5a8f55bd2a9d`; none were duplicated here.

Incomplete at the urgent cleanup boundary:

- Equivalent scrubbed-environment `make test-fast` runs on candidate and exact upstream main `0c07fdee3eefac5a21f93878211bf507b2e5c6a7` were intentionally interrupted with exit 130 after approximately 11 minutes and produced no final verdict.
- `shellcheck` was unavailable locally. The accepted-tip upstream shellcheck job was green, but this delta has not received a local shellcheck run.
- Final `make fmt-check`, `make compat-literals`, build, post-checkpoint `git diff --check`, independent review, and a new real-PostgreSQL run of this descendant are pending.
