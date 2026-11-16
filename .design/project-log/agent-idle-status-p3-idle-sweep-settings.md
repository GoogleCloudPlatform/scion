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
    `last_live_process_at IS NULL OR last_live_process_at < liveWorkCutoff`, bounded by `limit`
    (`autoSuspendQueueLimit`).
  - Added `ClaimIdleSuspend` (conditional CAS under the row lock before `DispatchAgentStop` that
    swaps `run_intent` to `stopped` and sets `idle_suspend_claim_until` only while `phase=running`,
    `run_id`, `activity`, and `activity_since` are unchanged and past grace, `last_seen` is within
    `agentHeartbeatRecency`, `!HasRecentLiveWork`, no start/delete/reincarnation claim is held, and
    no delivery landed at or after `activity_since`), `ClearIdleSuspendClaim`, `MarkAgentDelivery`,
    and `IfIdleSuspend` on `UpdateAgentStatus` so the post-stop status write enforces the same
    state preconditions.
- **Idle sweep, generalized auto-suspend worker, and delivery-side handshake (`pkg/hub`):**
  - Registered recurring singleton `agent-idle-suspend` (every 5 ticks, bounded by
    `stalledDetectionTimeout`) next to `agent-stalled-detection`, sharing `agentHeartbeatRecency`
    (`2m`).
  - Generalized `autoSuspendWorker` items to carry a `state.SuspendReason` (`idle` or `stalled`),
    preserving retained pre-suspend activities (`idle`, `completed`, `harness_error`) via
    `syncAgentAfterStopRecord` and emitting `SUSPENDED`-only notifications. Both `idle` and
    `stalled` worker items now also skip soft-deleted rows, rows with active deletion or
    reincarnation, and rows with an in-flight lifecycle operation (`tryBeginLifecycleOp`).
  - Every inbound delivery path to a running agent (user DM, agent DM, mention fan-out, web chat
    primary and secondaries, live/retry notification dispatch, scheduled message fire, and
    `group[]`) calls `MarkAgentDelivery` before dispatching; when an idle-suspend claim is in
    flight, `autoWakeModeOf` / `shouldQueueAutoWake` routes the item onto the existing
    `wakeOrAwaitStart` / `queueAutoWake` path to wait until suspension settles, then wake and
    deliver.
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
