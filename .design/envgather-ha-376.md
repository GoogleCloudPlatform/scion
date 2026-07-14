# Design #376: Stateless Env-Gather Finalize (pendingEnvGather HA Fix)

**Architect:** bf-arch-376
**Date:** 2026-07-14
**Status:** FINAL — all open questions resolved with user on thread 4524
**Issue:** https://github.com/ptone/scion/issues/376
**Investigation:** `/scion-volumes/scratchpad/projects/broker-fixes/investigation-376.md`
**Handoff sizing:** single developer, one PR, two commits (see §7). No EM needed.

---

## 1. Problem Summary

The env-gather protocol is two-phase: the broker's `createAgent` returns HTTP 202 with missing env keys and stores a `pendingAgentState` in the in-memory `pendingEnvGather` map (`pkg/runtimebroker/server.go:215`); the later `finalize-env` call looks that state up (`pkg/runtimebroker/handlers.go:2361-2381`). In a Hub HA deployment (Cloud Run, min-instances ≥ 2, co-located hub+broker per instance), the finalize request routinely lands on a different instance than the create, the lookup misses, and the broker returns 404 — the agent is stuck in `provisioning` forever. The broker's file-based persistence (`state_store.go`) only covers same-instance crash recovery; Cloud Run instances have independent filesystems.

Key architectural fact: the hub already models co-located brokers as **replica-independent** — `SetStatelessLocalBrokers` (`pkg/hub/broker_routing.go:114-128`) routes any lifecycle op for the embedded broker ID to whichever replica receives it, by design. `pendingEnvGather` is the one piece of state that violates this contract.

## 2. Chosen Approach: F — Replay-Based Stateless Finalize

> Confirmed by user on thread 4524, 2026-07-14: "Moving to F as the design seems to be sound."

On env submission, the hub does **not** call the broker's `finalize-env` action. Instead it rebuilds the full create request from durable shared state (exactly what `buildCreateRequest` already does on every dispatch), merges the CLI-gathered env into `ResolvedEnv` at highest precedence, and dispatches a normal create with `GatherEnv=true`. The broker's env evaluation finds all keys satisfied and falls through to its standard `buildStartContext` + `Manager.Start` path. No pending state is ever written or read; the entire stateful finalize path is **removed in the same PR** (user decision, Q2).

### Rationale

1. **No side effects to duplicate.** The broker's 202 return (`handlers.go:551`) happens *before* `buildStartContext` (`handlers.go:604`) — no worktree provisioning, secret projection, or container work has occurred when pending state is stored. A replayed create performs the first (and only) provisioning pass.
2. **Established pattern.** `CreateWithGatherDispatchArgs` is intentionally empty — "the owner rebuilds the full RemoteCreateAgentRequest from the shared store" (`pkg/hub/dispatch_args.go:58-60`). Env/secret resolution is already re-derived from the shared store + secret backend on every dispatch (`dispatch_args.go:22-26`). Replay extends this to finalize rather than introducing a new state store.
3. **Fixes both topologies.** DB-backed pending state (Approach D) fixes only brokers that can reach the hub's Postgres. Replay is stateless for co-located *and* remote multi-instance brokers — broker-local env sources are re-evaluated on whichever homogeneous replica receives the replay.
4. **Eliminates path drift.** Today `finalizeEnv` maintains a second, divergent start path — it already silently drops `WorkspaceMode` (`handlers.go:2396-2414` vs `handlers.go:604-623`). Replay leaves exactly one start path.
5. **Forward compatible.** New hub + old broker: the replay is a plain create request old brokers already handle. (The reverse skew — old hub + new broker — is explicitly not supported; see §6. User decision: nearly all installations are co-located single-binary, so hub and broker upgrade in lockstep.)

### Idempotency & duplicate-submit protection

- `buildCreateRequest` generates a fresh `RequestID` per call (`httpdispatcher.go:356`), so the replay never collides with the phase-1 202 cached in the broker's `dispatchAttempts`.
- `Manager.Start` dedupes by agent slug + project against the runtime (`pkg/agent/run.go:79-107`): if the container is already running it returns it. Since replicas of a stateless local broker share the runtime backend, this dedupe is cross-replica.
- The hub's phase guard in `submitAgentEnv` (`handlers_agents_core.go:1134-1138`) rejects submits once the agent is `running`. The narrow concurrent-double-submit window is bounded by the `Manager.Start` dedupe above and is no worse than the pre-existing race for plain creates.

## 3. Alternatives Considered

| # | Approach | Why rejected |
|---|----------|--------------|
| A | Hub-side env injection (skip two-step for co-located) | The hub cannot know the broker's required-key set — it is derived broker-side from harness-config, settings, and broker-local sources (`extractRequiredEnvKeys`, `handlers.go:1914`). The 202 evaluation must stay on the broker; A alone cannot remove the second phase. Replay subsumes A's goal without moving the evaluation. |
| B | Sticky / affinity routing | The breaking hop is CLI→hub LB *plus* hub→local co-located broker; Cloud Run cookie affinity does not apply to the in-process dispatch. Instance scale-down during the minutes-to-hours gather window re-breaks it. Couples correctness to LB behavior. |
| C | Stateless token with embedded pending state | Round-trips a large encrypted blob (a full `CreateAgentRequest` incl. resolved env/secrets) through CLI and hub; new key-management and expiry surface; protocol changes in CLI, hub, and broker. Unnecessary — the hub can already rebuild the request from its DB, so the "token" adds nothing but risk. |
| D | Shared/external pending-state store (issue's first suggestion) | DB-backed: couples the broker to hub Postgres (breaking the broker's HTTP-only independence) or requires a pluggable store injected only in co-located mode — and remote multi-instance brokers stay broken. Redis: new infra dependency. NFS: tmp+rename atomicity not guaranteed on network filesystems (`state_store.go:154-167`). Keeps the stateful two-step alive. |
| E | Hub-mediated pending state | Converges to F: the durable "pending state" the hub needs is the agent row (phase=`provisioning`) it already writes, plus request rebuilding it already does. Broker-side `MergedEnv` extras are re-derived on replay. F is E taken to its logical end without a new hub-side record. |

## 4. Detailed Implementation Plan

### Commit 1 — Hub: replay in `DispatchFinalizeEnv` (the fix; shippable alone)

**`pkg/hub/httpdispatcher.go:926-949` — rewrite `DispatchFinalizeEnv`:**

```
func (d *HTTPAgentDispatcher) DispatchFinalizeEnv(ctx, agent, env) error {
    requireRuntimeBrokerAssigned(agent)
    endpoint := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)

    req, err := d.buildCreateRequest(ctx, agent, "DispatchFinalizeEnv")  // fresh RequestID, full re-resolution
    // handle err
    req.GatherEnv = true                                                // broker re-verifies completeness pre-start
    if req.ResolvedEnv == nil { req.ResolvedEnv = map[string]string{} } // guard nil map before merge
    for k, v := range env {                                             // CLI-gathered values win
        req.ResolvedEnv[k] = v
    }
    req.EnvSources = d.buildEnvSources(ctx, agent, req.ResolvedEnv)     // then label gathered keys as user-provided

    resp, envReqs, err := d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
    // hash-mismatch repair retry — same pattern as DispatchAgentCreateWithGather (httpdispatcher.go:888-892)
    if errors.Is(err, ErrLifecycleDeferred) { return d.deferredFinalizeEnv(ctx, agent, env) }
    if err != nil { return err }
    if envReqs != nil && len(envReqs.Needs) > 0 {
        return &ErrEnvStillMissing{Requirements: envReqs}               // new typed error, carries full response
    }
    if resp != nil { d.applyBrokerResponse(agent, resp) }
    return nil
}
```

Notes:
- `GatherEnv=true` is kept deliberately: if keys are *still* missing, the broker answers 202 instead of launching a container that fails at runtime.
- The deferred cross-node path needs **no change**: `deferredFinalizeEnv` (`httpdispatcher.go:951-954`) still serializes `FinalizeEnvDispatchArgs{Env}` into the durable dispatch row, and the owning node's `execDispatchFinalizeEnv` (`pkg/hub/reconcile.go:233-258`) calls `DispatchFinalizeEnv` — which now replays. `FinalizeEnvDispatchArgs`, `FinalizeEnvResult`, and `execDispatchFinalizeEnv` are hub-to-hub and are **kept**.
- The `AgentDispatcher` interface signature of `DispatchFinalizeEnv` is unchanged; only the implementation changes.

**`pkg/hub/handlers_agents_core.go:1148-1151` — `submitAgentEnv` error mapping:**
- On `ErrEnvStillMissing`, respond with `MissingEnvVars(w, needs, s.buildEnvGatherResponse(ctx, agent, err.Requirements))` (`pkg/hub/errors.go:251`) instead of the generic `RuntimeError`. Agent stays in `provisioning`; the CLI can re-submit with the complete set.

**Tests (commit 1):**
- **#376 regression test:** dispatcher finalize against a broker `Server` whose `pendingEnvGather` map is empty (fresh instance, never saw the create) → agent starts.
- Merge precedence: CLI-gathered value overrides storage-resolved value for the same key; `ResolvedEnv == nil` case doesn't panic.
- Still-missing: replay returns 202 with remaining `Needs` → structured missing-env response, agent stays `provisioning`, second submit with complete env succeeds.
- Deferred path: `execDispatchFinalizeEnv` round-trip with the replay implementation.

### Commit 2 — Remove the stateful finalize path (hub client chain + broker)

> User decision (Q2): remove immediately in this PR rather than deprecate — "nearly all installations for now are co-located and single binary installs."

Broker (`pkg/runtimebroker`):
- `handlers.go:1187-1188` — remove `AgentActionFinalizeEnv` route case.
- `handlers.go:2349-2464` — remove `finalizeEnv` handler.
- `handlers.go:504-518` — remove the pending-state write in `createAgent`'s 202 path; the 202 becomes fully stateless. (The `dispatchAttempts` 202-response cache at `handlers.go:546-550` stays; it serves same-request-ID retries.)
- `server.go:215-216, 260-269` — remove `pendingEnvGather` map + mutex and `pendingAgentState`.
- `state_store.go` — remove the pending-env portion: `pendingStateDir`, `pendingStatePath`, `loadPendingState`, `upsertPendingState`, `deletePendingState`, `cleanupExpiredPendingLocked`, `pendingStateTTL`, `pendingStatePending`/`pendingStateFinalizing` constants, and the pending-dir setup in `initStateStore`. `dispatchAttempts` persistence is untouched.
- `types.go:454-458` — remove `FinalizeEnvRequest`.

Hub client chain (`pkg/hub`):
- `server.go:367-369` — remove `FinalizeEnv` from the `RuntimeBrokerClient` interface.
- `httpdispatcher.go:96-97` — remove `HTTPRuntimeBrokerClient.FinalizeEnv`.
- `brokerclient.go:101-104` — remove `AuthenticatedBrokerClient.FinalizeEnv`.
- `controlchannel_client.go:421-…` and `:749-760` — remove `ControlChannelBrokerClient.FinalizeEnv` and `HybridBrokerClient.FinalizeEnv`.
- `broker_http_transport.go:394-413` — remove the transport method.

Shared API (`pkg/api`):
- `agent_actions.go:39` — remove `AgentActionFinalizeEnv`; drop it from the action validation switch at `agent_actions.go:50`.

Tests (commit 2):
- `handlers_envgather_test.go`: drop finalize-handler tests; keep/extend the 202 evaluation tests; add a broker-side replay test — create returns 202, then a second create with complete env starts the agent on a *different* `Server` instance.
- `grep -rn "pendingEnvGather\|pendingAgentState\|FinalizeEnvRequest\|AgentActionFinalizeEnv" pkg/` returns nothing.

## 5. Impact on Other HA-Unsafe State

- **`dispatchAttempts` (`server.go:220`)** — unchanged by this design, and replay does not depend on it: `RequestID` is a fresh UUID per dispatch call (`httpdispatcher.go:356`), so the map already provides no cross-call (let alone cross-instance) idempotency. The effective duplicate-create guard is `Manager.Start`'s runtime-level dedupe (`run.go:79-107`), which is HA-safe when replicas share a runtime backend. **Follow-up issue #A** (to be filed by broker-fixes-lead, see §8).
- **`projectProvisionMu` (`server.go:236`)** — replay adds no provisioning passes (the 202 returns before provisioning; exactly one pass per successful start). The cross-instance provisioning race exists only for shared workspace backends (NFS) and is pre-existing and orthogonal. **Follow-up issue #B** (see §8).

## 6. Migration / Rollout

- **New hub + old broker:** replay is a standard `CreateAgentRequest` with complete env — old brokers evaluate, find nothing missing, and start. Works without broker changes.
- **Old hub + new broker:** NOT supported — old hubs dispatching `finalize-env` would get 404 from a new broker. Accepted per user decision (Q2): nearly all installations are co-located single-binary, so hub and broker upgrade in lockstep. Standalone remote brokers must not be upgraded ahead of their hub across this release; **release notes must state this**.
- **Co-located builds:** hub and broker version-lock in one binary, so the primary failing deployment (Cloud Run HA) gets both sides atomically.
- **No schema, infra, or CLI/API surface changes.** The CLI-facing 202/submit protocol is untouched.
- **Rollback:** revert the whole PR. Reverting only commit 2 also restores the legacy path, since the hub-side replay (commit 1) does not depend on the removal.
- **Leftover on-disk state:** existing `pending-env/*.json` files from older brokers are never read after upgrade; they are inert. Optional cleanup in `initStateStore` is nice-to-have, not required.

## 7. Implementation Phases (Developer Handoff)

Sized for a **single developer** — two commits in one PR on one branch; no parallel phases, so no EM required.

1. **Commit 1 — hub replay fix** (§4 commit 1): `DispatchFinalizeEnv` replay, `ErrEnvStillMissing`, `submitAgentEnv` mapping, tests including the #376 regression test. This commit alone fixes the bug.
2. **Commit 2 — legacy path removal** (§4 commit 2): broker handler/state removal, hub client-chain removal, `pkg/api` action removal, test updates.
3. Standard process: rebase onto upstream main at start and before compare URL; design doc committed to `.design/` as part of the PR; `go build ./... && go test ./pkg/runtimebroker/... ./pkg/hub/... ./pkg/api/...` green per commit.

## 8. Follow-Up Issues (filed by broker-fixes-lead, out of scope here)

- **#A — `dispatchAttempts` is HA-ineffective:** in-memory + per-instance, and request IDs are per-call UUIDs, so it provides no cross-instance (or even cross-call) create idempotency. Decide: rely on runtime-level dedupe and delete the map, or make request-level idempotency real (durable IDs from the hub).
- **#B — `projectProvisionMu` cross-instance provisioning race:** per-instance mutex cannot serialize `ProvisionShared` across replicas on shared workspace backends (NFS). Needs NFS-safe locking or fully idempotent provisioning.

## 9. Acceptance Criteria

1. **HA regression test:** finalize dispatched to a broker instance whose `pendingEnvGather` is empty (never saw the create) starts the agent successfully.
2. Single-instance env-gather E2E unchanged: create → 202 with `Needs` → submit → agent `running`.
3. Submitting with keys still missing returns a structured missing-env-vars error listing the keys; agent remains `provisioning`; a subsequent complete submit succeeds.
4. CLI-gathered values take precedence over storage-resolved values for the same key.
5. Duplicate submit after success returns 409 `invalid_state` (existing phase guard).
6. The `finalize-env` broker action and all `pendingEnvGather` state are gone: no references to `pendingEnvGather`, `pendingAgentState`, `FinalizeEnvRequest`, or `AgentActionFinalizeEnv` remain; the broker rejects the removed action.
7. Deferred cross-node finalize (`execDispatchFinalizeEnv`) passes with the replay implementation.
8. Manual/integration validation on Cloud Run with min-instances=2: agent requiring a secret-backed env var reaches `running` reliably (the #376 repro).
9. No new DB tables, infra dependencies, or CLI protocol changes.

## 10. Open Questions — Resolved

| # | Question | Resolution (user, thread 4524, 2026-07-14) |
|---|----------|--------------------------------------------|
| 1 | Approach: replay (F, recommended) vs DB-backed pending state (D, issue's first suggestion)? | **F approved** — "Moving to F as the design seems to be sound." |
| 2 | Retain legacy broker `finalize-env` path for one release, or remove immediately? | **Remove immediately (b)** — "nearly all installations for now are co-located and single binary installs." |
| 3 | `dispatchAttempts` / `projectProvisionMu` hardening: follow-up issues or fold in? | **Follow-up issues**, filed by broker-fixes-lead; this design stays scoped to #376. |
