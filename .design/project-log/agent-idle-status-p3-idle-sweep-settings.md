# Idle status phase 3: idle auto-suspend sweep and settings

**Date:** 2026-11-15
**Branch:** scion/agent-idle-status-p3
**Issue:** ptone/scion#7947 (phase 3 of ptone/scion#7826)

## What changed

- **Suspend eligibility (`pkg/agent/state`):** added `ActivityHarnessError` (`harness_error`,
  introduced by `ptone/scion#7858`) to `(Activity).IsSuspendEligible()`, alongside `idle` and
  `completed`, and added `SuspendEligibleActivities()` as the single source of truth for store
  queries.
- **Settings (`pkg/config`, `pkg/config/opsettings`, `cmd/server_foreground.go`, `pkg/hub`):**
  registered `server.hub.auto_suspend_idle` (boolean, default `false`,
  `SCION_SERVER_HUB_AUTOSUSPENDIDLE`) and `server.hub.idle_suspend_after` (duration, default
  `15m`, minimum `5m`, `SCION_SERVER_HUB_IDLESUSPENDAFTER`) across the v1 JSON schema, settings
  structs, `HubConfig`, `opsettings` section/koanf/registry definitions (and regenerated the web
  registry fixture), `Layer1Snapshot` live reload, admin settings DB GET/PUT handlers, and the
  test-admin policy deny list. Both `hub.New` and `ApplySnapshot` default an unset/zero
  `idle_suspend_after` to `15m`, accept values `>= 5m`, and fall back to `15m` with a warning for
  values below `5m` (matching `stalled_threshold`).
- **Store schema, candidate query, and claim/delivery handshake (`pkg/ent`, `pkg/store`, `pkg/store/entadapter`):**
  - Added two additive nullable columns on `agents`: `last_delivery_at` and
    `idle_suspend_claim_until` (`json:"-"`; no `DowngradeFloor` bump).
  - Allocated singleton advisory lock `LockAgentIdleSuspend` (`0x5C10001D`) and added
    `ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, limit)`, selecting
    non-deleted `running` agents with a suspend-eligible activity, non-null
    `activity_since < cutoff`, non-null `last_seen >= heartbeatRecency`, and
    `last_live_process_at IS NULL OR last_live_process_at < liveWorkCutoff`, ordered oldest-idle
    first (`ORDER BY activity_since ASC, id ASC`) and bounded by `limit`
    (`autoSuspendQueueLimit = 1000`).
  - Added `ClaimIdleSuspend` as the single commit point before `DispatchAgentStop`: under the row
    lock and store clock (`storeNow`), it swaps `run_intent` to `stopped` and sets
    `idle_suspend_claim_until` only while `phase=running`, `run_id`, `activity`, and
    `activity_since` are unchanged and past grace, `last_seen` is within `agentHeartbeatRecency`,
    `!HasRecentLiveWork`, no start/delete/reincarnation/idle-suspend claim is held, and no delivery
    landed at or after `activity_since`. Once `ClaimIdleSuspend` commits and `DispatchAgentStop`
    succeeds, the stop is irreversible: `UpdateAgentStatus` with `IfIdleSuspendCommit` accepts a row
    in `running`, `stopping`, or `stopped` (handling `sciontool init`'s SIGTERM shutdown reports)
    conditioned only on the same `run_id`, the held `claimUntil` lease, and no delete,
    reincarnation, or foreign start claim, restoring `RetainedActivity` (`idle`, `completed`, or
    `harness_error`) alongside `suspend_reason=idle` and never reverting `run_intent` to `running`.
  - Standardized claim-expiry checks on the store clock (`MarkAgentDelivery` returning
    `(agent, claimActive, err)` and setting `agent.IdleSuspendClaimActive` from `storeNow`, plus
    `entTxStoreNow` inside `UpdateAgentStatus`).
- **Idle sweep, generalized auto-suspend worker, and unified delivery gate (`pkg/hub`):**
  - Registered recurring singleton `agent-idle-suspend` (every 5 ticks, bounded by
    `stalledDetectionTimeout`) next to `agent-stalled-detection`, sharing `agentHeartbeatRecency`
    (`2m`).
  - Generalized `autoSuspendWorker` items to carry a `state.SuspendReason` (`idle` or `stalled`),
    preserving retained pre-suspend activities (`idle`, `completed`, `harness_error`) via
    `syncAgentAfterStopRecord` and emitting `SUSPENDED`-only notifications. Both `idle` and
    `stalled` worker items now also skip soft-deleted rows, rows with active deletion or
    reincarnation, and rows with an in-flight lifecycle operation (`tryBeginLifecycleOp`).
  - Routed every `AgentDispatcher.DispatchAgentMessage` caller in `pkg/hub` through one fail-closed
    choke point (`dispatchMarkedAgentMessage`, enforced by AST guard
    `TestMessageDispatchCallsMarkAgentDelivery`) that calls `MarkAgentDelivery` before any broker
    dispatch. Auto-waking paths (DMs, mentions, web chat, notifications, scheduled messages) wait
    for an active idle-suspend claim to settle via `wakeOrAwaitStart` / `queueAutoWake` and then
    auto-wake and deliver; non-auto-waking paths (`MessageBrokerProxy`, `broadcastDirect`,
    `handleBrokerInbound`, `dispatchRoutedRecipient`, `reconcile.deliverMessage`,
    `deliverArtifactReview`, choice responses, `delegatedSender` DM, `group[]`, and delivery
    notices) refuse, park, or mark failed just as they do for a suspended target, and store errors
    in `MarkAgentDelivery` fail closed without dispatching.
- **Phase 2b review follow-ups folded into phase 3:**
  - **Hub-to-user suspension conversion (`convertHubSuspendToUser`):** gated on
    `lifecycle.idle_suspend`, `POST /api/v1/agents/{id}/suspend` on an `idle`- or
    `stalled`-suspended agent rewrites `suspend_reason` to `user` in place (`IfPhase: suspended`,
    `IfNoStartClaim: true`, `IfAutoWakeReason: true`, no runtime broker action), refusing with
    `409 Conflict` when a start claim, idle-suspend claim, or lifecycle op is in flight.
  - **Group-send auto-wake hint (`groupMemberDeliverableError`):** when a `group[]` message
    encounters a suspended member (or one mid-idle-suspend) with `lifecycle.idle_suspend` enabled
    and an auto-waking reason, the refusal directs the sender to message it directly without
    `--wake`.
