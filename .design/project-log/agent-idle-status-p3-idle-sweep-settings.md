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
  - Allocated singleton advisory lock `LockAgentIdleSuspend` (`0x5C100030`) and added
    `ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, after, limit)`,
    selecting non-deleted `running` agents with a suspend-eligible activity, non-null
    `activity_since < cutoff`, non-null `last_seen >= heartbeatRecency`, and
    `last_live_process_at IS NULL OR last_live_process_at < liveWorkCutoff`, ordered oldest-idle
    first (`ORDER BY activity_since ASC, id ASC`) with an optional keyset cursor `after` so the
    idle sweep advances past non-resumable or already-queued rows, and reserves half of
    `autoSuspendQueueLimit` (`1000`) for stalled hand-offs.
  - Added `ClaimIdleSuspend` as the single commit point before `DispatchAgentStop`: under the row
    lock and store clock (`storeNow`), it swaps `run_intent` to `stopped` and sets
    `idle_suspend_claim_until` (`idleSuspendClaimLease() = 226s`) only while `phase=running`,
    `run_intent` is `running` (`NULL` treated as `running`), `run_id`, `activity`, and
    `activity_since` are unchanged and past grace, `last_seen` is within `agentHeartbeatRecency`,
    `!HasRecentLiveWork`, no start/delete/reincarnation/idle-suspend claim is held, and no delivery
    landed at or after `activity_since`. Once `ClaimIdleSuspend` commits and `DispatchAgentStop`
    succeeds, the stop is irreversible: `UpdateAgentStatus` with `IfIdleSuspendCommit` accepts a row
    in `running`, `stopping`, `stopped`, or `error` (including non-zero exits and multi-replica
    post-stop broker heartbeats) conditioned on the same `run_id`, the exact unexpired `claimUntil`
    lease, `run_intent=stopped` (never `stop-queued`), and no delete/reincarnation/foreign start
    claim. Only explicit user lifecycle actions (`SwapRunIntent` for user `stop` / `stop-all` /
    `suspend` / `delete`, explicit user `ClaimAgentStart` for `start` / `restart`, reincarnation,
    and deletion) preempt an active `idle_suspend_claim_until`; every background or system writer
    across replicas (`SwapSystemRunIntent`, `IfNoActiveIdleClaim`, `ClaimAgentStop`,
    `ConvertUnconfirmedToStop`, `ClaimAgentStart(Wake|Recovery)`, and late launch-failure reports)
    preserves or yields to an active claim, restoring `RetainedActivity` (`idle`, `completed`, or
    `harness_error`) alongside `suspend_reason=idle` and never reverting `run_intent` to `running`.
    Post-stop commit failure, hub crash, and stop-timeout recovery are deferred to
    `ptone/scion#8204` (do not set `server.hub.auto_suspend_idle` with `lifecycle.idle_suspend` on
    in production until `ptone/scion#8204` lands; noted in `pkg/experiments/registry.go`,
    `docs-site/src/content/docs/reference/experiments.md`, and the `autoSuspendAgent` commit point).
  - Standardized claim-expiry checks on the store clock (`GetAgent`, `GetAgentBySlug`,
    `ListAgents`, and `MarkAgentDelivery` populating `agent.IdleSuspendClaimActive` from the store
    clock only when `IdleSuspendClaimUntil != nil`, `AgentFilter.OrActiveIdleClaim` gated on
    `lifecycle.idle_suspend`, and `entTxStoreNow` inside `UpdateAgentStatus` skipped when
    `IdleSuspendClaimUntil == nil && IfIdleSuspendCommit == nil`, so off-state queries incur zero
    extra `SELECT now()` round trips).
- **Idle sweep, generalized auto-suspend worker, and unified delivery gate (`pkg/hub`):**
  - Registered recurring singleton `agent-idle-suspend` (every 5 ticks, bounded by
    `stalledDetectionTimeout`) next to `agent-stalled-detection`, sharing `agentHeartbeatRecency`
    (`2m`).
  - Generalized `autoSuspendWorker` items to carry a `state.SuspendReason` (`idle` or `stalled`),
    preserving retained pre-suspend activities (`idle`, `completed`, `harness_error`) via
    `syncAgentAfterStopRecord` and emitting `SUSPENDED`-only notifications, and rejecting
    `managed:*` runtimes in `harnessSupportsResumeMemo`. Both `idle` and `stalled` worker items now
    also skip soft-deleted rows, rows with active deletion or reincarnation, and rows with an
    in-flight lifecycle operation (`tryBeginLifecycleOp`), and the `idle` worker re-reads both
    `lifecycle.idle_suspend` and `server.hub.auto_suspend_idle` immediately before
    `ClaimIdleSuspend`.
  - Routed every `AgentDispatcher.DispatchAgentMessage` reference in `pkg/hub` through one
    choke point (`dispatchMarkedAgentMessage`, enforced by AST guard
    `TestMessageDispatchCallsMarkAgentDelivery` including method-value bypasses). Whenever
    `lifecycle.idle_suspend` is enabled (even while `server.hub.auto_suspend_idle` is `false`, so
    enabling the runtime setting across replicas never opens an unmarked window) OR a target row
    already carries an idle-suspend claim, `markAgentForDispatch` runs `MarkAgentDelivery`
    fail-closed before any broker dispatch, and every validator and settle path honours an active
    row claim regardless of `server.hub.auto_suspend_idle`; when `lifecycle.idle_suspend` is off and
    no claim is present on the row, the delivery mark and claim check are skipped completely so DB
    work and behavior match base (AC 8). Auto-waking paths wait up to the full idle-suspend claim lease (`idleSuspendClaimLease`)
    for an active claim to settle via `wakeOrAwaitStart` / `queueAutoWake` and report `"is suspending"`
    on timeout; non-auto-waking paths (`MessageBrokerProxy`, `broadcastDirect`,
    `handleBrokerInbound`, `dispatchRoutedRecipient`, `reconcile.deliverMessage`,
    `deliverArtifactReview`, choice responses, `delegatedSender` DM, `group[]`, and delivery
    notices) refuse, park, or mark failed just as they do for a suspended target.
  - Integrated with Conduit `awaiting_session` hold and re-drive (`GoogleCloudPlatform/scion#4935`):
    `dispatchClaimedHeldMessage` in `message_hold_redrive.go` routes through `dispatchWithBrokerRetry`
    with `s.deliveryMarker()`, treating an active idle-suspend claim or pre-dispatch
    `errAgentIdleSuspending` / `errDeliveryMarkFailed` as deferrable (keeping or returning rows to
    `awaiting_session` for a later re-drive without waking); `holdOrDispatch` passes pre-dispatch
    `errAgentIdleSuspending` and `errDeliveryMarkFailed` straight through without creating or
    joining a session hold and refreshes `hold_launch_id` after synchronous auto-wake; existing
    `awaiting_session` held rows survive an active idle-suspend claim, post-stop runtime reports,
    and resting `phase=suspended, suspend_reason=idle`, rebind `hold_launch_id` to the resumed
    `RunID` (`RebindHeldMessagesLaunch`) on manual resume or incoming-DM auto-wake so FIFO re-drive
    completes once the resumed session connects; and both `TestMessageDispatchCallsMarkAgentDelivery`
    and `TestHoldOrDispatchCallSites` enforce that persisted dispatches pass through both gates.
- **Phase 2b review follow-ups folded into phase 3:**
  - **Hub-to-user suspension conversion (`convertHubSuspendToUser`):** gated on
    `lifecycle.idle_suspend`, `POST /api/v1/agents/{id}/suspend` on an `idle`- or
    `stalled`-suspended agent rewrites `suspend_reason` to `user` in place (`IfPhase: suspended`,
    `IfNoStartClaim: true`, `IfNoActiveIdleClaim: true`, `IfAutoWakeReason: true`, no runtime
    broker action), expiring any `awaiting_session` held messages preserved across `suspended(idle)`
    and refusing with `409 Conflict` when a start claim, idle-suspend claim, or lifecycle op is in
    flight.
  - **Group-send auto-wake hint (`groupMemberDeliverableError`):** when a `group[]` message
    encounters a suspended member (or one mid-idle-suspend) with `lifecycle.idle_suspend` enabled
    and an auto-waking reason, the refusal directs the sender to message it directly without
    `--wake`.
