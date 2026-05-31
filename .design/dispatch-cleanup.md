# Hub -> Broker Dispatching and Provisioning Review

## Scope
This review covers Hub-to-Broker agent dispatch and provisioning paths, including broker selection, create/start/provision/finalize-env flows, auth between Hub and Broker, and operational lifecycle concerns.

Primary files reviewed:
- `pkg/hub/handlers.go`
- `pkg/hub/httpdispatcher.go`
- `pkg/hub/brokerclient.go`
- `pkg/hub/server.go`
- `pkg/runtimebroker/handlers.go`
- `pkg/runtimebroker/server.go`
- `pkg/runtimebroker/hub_connection.go`
- `pkg/runtimebroker/heartbeat.go`
- `pkg/runtimebroker/brokerauth.go`

## Current Flow Summary
1. Hub accepts create request, resolves grove + runtime broker, and writes `store.Agent` in `created` phase.
2. Hub dispatches to Broker via `DispatchAgentCreateWithGather` (for both gather and non-gather flows).
3. Broker merges env/secrets/settings, optionally returns `202` env requirements, or provisions+starts the container.
4. Hub applies broker response fields to local agent model and persists phase/runtime metadata.
5. For env-gather, Hub receives user-supplied env and calls Broker `finalize-env`.

This is functionally rich, but there are several reliability and debuggability gaps.

## Findings

### 1. Authenticated dispatch can silently downgrade to unauthenticated
- Evidence: `pkg/hub/brokerclient.go` `doRequest` logs signing failure and continues request anyway.
- Risk: Security policy bypass and confusing partial failures when strict auth is expected.
- Cleanup: Fail closed when signing fails for `AuthenticatedBrokerClient`; keep explicit fallback only in an explicitly named “permissive” client.

### 2. Broker endpoint fallback to `http://localhost:9800` is unsafe
- Evidence: `pkg/hub/httpdispatcher.go` `getBrokerEndpoint` falls back when endpoint is empty.
- Risk: Misroutes dispatches to wrong local broker; difficult to diagnose.
- Cleanup: Remove localhost fallback and return hard validation error; require endpoint during broker registration/join.

### 3. Env-gather pending state is in-memory, keyed by mutable name
- Evidence: `pkg/runtimebroker/server.go` `pendingEnvGather map[string]*pendingAgentState` keyed by agent name; `pkg/runtimebroker/handlers.go` finalize uses route id.
- Risk: Collision across groves/brokers, loss on broker restart, no retry safety.
- Cleanup: Key by immutable agent ID, persist pending state (store-backed), add TTL cleanup + explicit status transitions.

### 4. Finalize-env deletes pending state before start succeeds
- Evidence: `pkg/runtimebroker/handlers.go` finalize removes pending entry before `mgr.Start`.
- Risk: transient start failure causes unrecoverable flow requiring full recreate.
- Cleanup: mark pending as `finalizing`, delete only after successful start; on failure keep retryable state.

### 5. Existing-agent cleanup can orphan runtime resources
- Evidence: `pkg/hub/handlers.go` `handleExistingAgent` ignores `DispatchAgentDelete` errors, then deletes DB agent.
- Risk: orphaned containers/worktrees with no Hub record.
- Cleanup: perform compensating strategy:
  - hard fail delete path by default
  - add `force` option for explicit orphan-tolerant behavior
  - write orphan marker/audit event if DB delete proceeds without broker delete.

### 6. Duplicate create flows diverge and increase regression risk
- Evidence: near-duplicate create/dispatch logic at `pkg/hub/handlers.go:340+` and `:2540+`.
- Risk: bugfixes applied to one path but not the other.
- Cleanup: extract shared `createAgentInGrove(ctx, groveID, req)` service function; keep handlers thin.

### 7. Dispatch protocol hides decode failures
- Evidence: `StartAgent` response decode returns `nil, nil` on decode error in both HTTP and authenticated broker clients.
- Risk: successful HTTP but invalid payload appears as success; phase drift and hidden incompatibility.
- Cleanup: return explicit protocol error with response snippet; never treat invalid JSON as success.

### 8. Broker selection strategy can choose offline singleton provider
- Evidence: `pkg/hub/handlers.go` `resolveRuntimeBroker` case 3 uses single provider “regardless of online status”.
- Risk: immediate dispatch failure path where Hub could fail earlier with clearer error.
- Cleanup: require online for auto-selection, unless caller opts into `allowOffline=true` diagnostic mode.

### 9. Weak request correlation/idempotency in dispatch path
- Evidence: `RemoteCreateAgentRequest.RequestID` exists but is not used for dedupe/trace.
- Risk: retries can duplicate side effects; difficult cross-component tracing.
- Cleanup: enforce `requestId` generation at Hub, propagate through all broker calls/logs/events, add idempotency cache/table on broker.

### 10. Reliability model is “single try, best effort” for critical operations
- Evidence: no retry/circuit policy around create/start/finalize/delete dispatch calls.
- Risk: transient network failures convert to user-facing hard failures and state skew.
- Cleanup: introduce retry policy for retriable classes (timeouts, connection reset, 502/503/504), with bounded attempts + jitter; annotate attempt counts in logs/events.

### 11. Restart API on broker is incomplete
- Evidence: `pkg/runtimebroker/handlers.go` restart only stops then returns accepted with TODO.
- Risk: latent behavior mismatch if future callers use it directly.
- Cleanup: implement full stop+start or remove endpoint until complete.

### 12. Observability gaps for debugability
- Evidence: no explicit dispatch attempt object/state; no phase transition audit trail tied to broker response.
- Risk: postmortem requires log spelunking across services.
- Cleanup: add structured lifecycle events:
  - `dispatch_requested`
  - `dispatch_sent`
  - `dispatch_ack`
  - `dispatch_failed`
  - `phase_transition`
  Include requestId, brokerId, endpoint, HTTP status, latency, error class.

## Candidate Refactor Plan

### Phase 1: Safety and correctness (small, high ROI) [COMPLETED]
1. Fail closed on auth-signing failure in `AuthenticatedBrokerClient`.
2. Remove `localhost` endpoint fallback.
3. Return hard error on broker response decode failure.
4. Gate single-provider auto-selection on broker online status.
5. Implement broker restart endpoint or delete it.

### Phase 2: State model hardening [COMPLETED]
1. Replace in-memory pending env-gather with persisted pending state keyed by `agentID`.
2. Make finalize-env retry-safe (do not drop state pre-start).
3. Add explicit dispatch attempt model (idempotency + audit).
4. Make cleanup semantics explicit (`strict` vs `force`) and visible in API.

### Phase 3: Maintainability and architecture cleanup [COMPLETED]
1. Deduplicate Hub create-agent handlers into one service path.
2. Consolidate broker clients (`HTTPRuntimeBrokerClient` and `AuthenticatedBrokerClient`) into one transport + auth strategy layer.
3. Extract shared grove path resolution for create/start dispatch paths.

### Phase 4: Observability and operability
1. Propagate `requestId` end-to-end (Hub request -> dispatcher -> broker logs -> events).
2. Add metrics:
   - dispatch success/failure by operation
   - retry counts
   - finalize-env pending count + age histogram
   - orphan cleanup count
3. Add admin/debug endpoints for dispatch-attempt inspection.

## Suggested Test Additions
1. Auth failure path test: signing failure must fail dispatch.
2. Endpoint resolution test: missing broker endpoint returns validation error.
3. Env-gather recovery test: broker restart between create(202) and finalize retains pending state.
4. Finalize retry test: transient start failure can retry finalize without recreating agent.
5. Orphan prevention test: broker delete failure does not silently delete DB record in strict mode.
6. Idempotency test: repeated `requestId` does not duplicate provisioning.
7. Duplicate handler parity test (until dedupe complete): both create paths produce identical state transitions.

## Priority Hotspots
If only a few things are done first, prioritize:
1. Fail-closed auth + endpoint fallback removal.
2. Durable env-gather pending state keyed by immutable ID.
3. Decode failures as hard protocol errors.
4. Consolidate duplicated create/dispatch logic.

These four changes will materially improve robustness, reliability, and future debugability with moderate implementation effort.

## Hub Agent Env Context Flow

### Authoritative Data Path
1. `cmd/server.go` determines Hub/broker wiring and runtime-broker endpoint.
2. `pkg/hub/httpdispatcher.go` dispatches create/start payloads and sets Hub-derived env context.
3. `pkg/runtimebroker/handlers.go` merges request env + broker/grove fallbacks and applies container bridge overrides.
4. `pkg/agent/run.go` applies final agent-level config/env overrides before launching runtime containers.

### Field Ownership
- `SCION_HUB_ENDPOINT` / `SCION_HUB_URL`:
  - authoritative from dispatch/broker input when present
  - falls back to broker config, then grove settings
  - localhost endpoints may be rewritten to `ContainerHubEndpoint` for containerized runtimes (non-kubernetes)
- `SCION_GROVE_ID`, `SCION_AGENT_ID`, `SCION_AGENT_SLUG`:
  - authoritative from Hub dispatch and broker route context
- `SCION_AUTH_TOKEN` and other secret values:
  - authoritative from Hub-resolved secrets/tokens (or explicit broker fallback token)
  - values must be redacted in broker debug logs

### Why Local Settings Are Sometimes Ignored
In Hub-connected container flows, local grove settings may not represent the active Hub endpoint (for example, combo/hub-managed routing or remote broker dispatch). In that mode, dispatch/broker authoritative inputs intentionally take precedence over stale local settings.

## Troubleshooting Matrix

### "Hub is enabled but no endpoint configured"
- Typical cause: Hub mode enabled with no `--hub`, `SCION_HUB_ENDPOINT`, or `hub.endpoint`.
- Check: `scion config get hub.endpoint`, `echo $SCION_HUB_ENDPOINT`.
- Fix: set endpoint explicitly or disable Hub mode for local-only workflows.

### Agent in container cannot reach localhost Hub endpoint
- Typical cause: endpoint resolves to `http://localhost:*` inside containerized runtime.
- Check: broker config `ContainerHubEndpoint` and final env in broker debug logs.
- Fix: configure `ContainerHubEndpoint` bridge host; ensure runtime is non-kubernetes if expecting rewrite.

### Stale remote endpoint overrides combo-mode endpoint
- Typical cause: older grove settings with a persisted endpoint conflicting with active dispatch/broker endpoint.
- Check: dispatcher payload endpoint vs grove `settings.yaml`.
- Fix: rely on dispatch/broker authoritative endpoint; clean stale grove endpoint if no longer valid.

### Hub env unexpectedly stripped
- Typical cause: grove or profile explicitly disables Hub (`hub.enabled=false` / `hub.local_only=true`).
- Check: effective settings and env before `pkg/agent/run.go` final merge.
- Fix: re-enable Hub in settings for that grove/profile or run explicitly with `--no-hub` when local-only is intended.

## Regression Test Matrix
- Broker has `HubEndpoint` configured vs empty.
- Dispatcher sends endpoint in request vs relies on resolved env vs neither.
- `resolvedEnv` includes `SCION_HUB_ENDPOINT` vs legacy `SCION_HUB_URL`.
- Grove settings endpoint present vs absent vs `hub.enabled=false`.
- Runtime type `docker`/`podman` vs `kubernetes` for bridge rewrite behavior.
- Localhost endpoints vs non-localhost endpoints for container override checks.
