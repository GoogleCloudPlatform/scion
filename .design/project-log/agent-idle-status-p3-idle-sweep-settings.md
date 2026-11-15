# Idle status phase 3: idle auto-suspend sweep and settings

**Date:** 2026-11-15
**Branch:** scion/agent-idle-status-p3
**Issue:** ptone/scion#7947 (phase 3 of ptone/scion#7826)

## What changed

- **Suspend eligibility (`pkg/agent/state`):** added `ActivityHarnessError` (`harness_error`,
  introduced by ptone/scion#7858) to `(Activity).IsSuspendEligible()`, alongside `idle` and
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
- **Store candidate query and advisory lock (`pkg/store`, `pkg/store/entadapter`):** allocated
  singleton advisory lock `LockAgentIdleSuspend` (`0x5C10001C`) and added
  `ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency)`, selecting non-deleted `running`
  agents with a suspend-eligible activity, non-null `activity_since < cutoff`, and non-null
  `last_seen >= heartbeatRecency` (so offline agents and rows with a null `activity_since` are
  never touched).
- **Idle sweep and generalized auto-suspend worker (`pkg/hub`):**
  - Registered recurring singleton `agent-idle-suspend` (every 5 ticks, bounded by
    `stalledDetectionTimeout`) next to `agent-stalled-detection`. The tick exits immediately unless
    both the `lifecycle.idle_suspend` experiment and `server.hub.auto_suspend_idle` are enabled.
  - Generalized `autoSuspendWorker` items to carry a `state.SuspendReason` (`idle` for the idle
    sweep, `stalled` for stalled detection).
  - Generalized `autoSuspendAgent` to re-read the agent and enforce all guards on the fresh row:
    1. For `idle`, both the experiment and `auto_suspend_idle` are still on, and the agent is
       still `running` in the same `RunID`, with the same suspend-eligible activity and
       `activity_since` still before `now - idle_suspend_after`.
    2. `!agent.HasRecentLiveWork(now)` (`pkg/store/live_work.go` from `GoogleCloudPlatform/scion#4874`,
       reading `last_live_process_at` within `store.LiveWorkFreshness` = 90 s).
    3. `harnessSupportsResume` (by underlying harness type, logging `INFO` skips).
    4. No active deletion, reincarnation, or in-flight lifecycle operation (`tryBeginLifecycleOp`).
    It then stops the container and writes `suspend_reason=idle` (or `stalled` for the stalled
    path).
- **Phase 2b review follow-ups folded into phase 3:**
  - **Hub-to-user suspension conversion (`convertHubSuspendToUser`):** `POST /api/v1/agents/{id}/suspend`
    on an already-suspended agent whose `suspend_reason` is `idle` or `stalled` rewrites
    `suspend_reason` to `user` in place (`IfPhase: suspended`, no runtime broker action), stopping
    future auto-wake while keeping existing lifecycle permission checks and keeping suspend on an
    already user-suspended agent as a `400` validation error.
  - **Group-send auto-wake hint (`groupMemberDeliverableError`):** when a `group[]` message
    encounters a suspended member for which `shouldAutoWake` holds (`lifecycle.idle_suspend` on and
    `suspend_reason` in `{idle, stalled}`), the per-recipient refusal points the sender at a
    direct message without `--wake` ("message it directly to wake and deliver") instead of
    telling them to pass `--wake`.

## Live-work guard (`GoogleCloudPlatform/scion#4874` / `ptone/scion#7855`)

`autoSuspendAgent` calls `(*store.Agent).HasRecentLiveWork(now)` (`pkg/store/live_work.go`,
merged upstream in `GoogleCloudPlatform/scion#4874`), skipping both stalled and idle auto-suspends
when `last_live_process_at` is within `store.LiveWorkFreshness` (90 s). Table tests in
`pkg/hub/idle_suspend_test.go` exercise recent, stale, and absent `last_live_process_at` values
across all suspend-eligible activities.

## Notes

- Based on `main` after phase 2b (`GoogleCloudPlatform/scion#4904`) and live-work detection
  (`GoogleCloudPlatform/scion#4874`) merged upstream, so auto-wake and `HasRecentLiveWork` are
  both in place.
- No database schema change in phase 3; `DowngradeFloor` is unchanged.
