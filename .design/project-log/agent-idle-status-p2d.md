# Agent idle status, phase 2d: retained activity across suspend and per-period notification dedupe

**Date:** 2026-11-15
**Branch:** scion/agent-idle-status-p2d
**Issue:** ptone/scion#7946 (parent ptone/scion#7826)

## What changed

- **State model (`pkg/agent/state`):**
  - Added `Activity.IsRetainedOnSuspend()` (`idle`, `completed`, `harness_error`, matched case-insensitively) and `RetainedSuspendActivity(activity, stalledFrom)`.
  - Updated `AgentState.Validate()` to permit `PhaseSuspended` combined with a retained activity alongside `PhaseStopped` with a terminal activity.
  - Kept legacy `DisplayStatus()` returning `"suspended"` for `PhaseSuspended` regardless of retained activity so older CLIs, brokers, and federation remain unaffected.
- **Store & Hub suspend/resume (`pkg/store`, `pkg/hub`):**
  - `UpdateAgentStatus` now computes and persists `RetainedSuspendActivity` in the same atomic write as `suspend_reason` when transitioning to `suspended` without an explicit activity, clearing `blocked`, `waiting_for_input`, `working`, `stalled_from_activity`, and `tool_name` (`activity_since` clears automatically when leaving `running`). Also made `stampActivitySince` compare incoming and stored activities case-insensitively so case-variant status re-reports preserve `activity_since`.
  - Replaced in-memory `agent.Activity = ""` blanking across all three suspend call sites (`suspendAgent`, `autoSuspendStalledAgent`, `stopHeldAgent`) with `syncAgentAfterStopRecord` (preserving `SuspendReason` in memory alongside the retained or cleared activity) so HTTP responses and SSE `AgentStatusEvent` payloads match the stored row.
  - Cleared retained activity across every resume path (including phase 2b auto-wake via `wakeAgentForDM`): `UpdateAgentStatus` (`suspended -> starting` and `suspended -> running`, with `startDispatch` rollback restoring `priorActivity` alongside `suspend_reason` on failed launches while preserving `ClearTerminalRemnants` for mid-dispatch status writes and exit-field rules), async launch `beginLaunch` (`start`/`restart`, preserving terminal activities `crashed` and `limits_exceeded` on `stopped` starts), `handleExistingAgent`'s whole-row resume writes, and `wakeAgentForDM` before `waitForAgentReady` so explicit and auto-wake resumes wait for the new run's activity rather than returning immediately on pre-suspend `completed`/`idle`.
  - Removed `omitempty` from `AgentStatusEvent.Activity` so an explicit empty string reaches the web client whenever activity is cleared on suspend or resume.
- **Notification matching & per-period dedupe (`pkg/hub/notifications.go`):**
  - `notificationEventStatus` returns `evt.Phase` (`"suspended"`) whenever `evt.Phase == "suspended"`, so suspending an agent (including `autoSuspendStalledAgent` with `suspend_reason=stalled`) with retained `completed`, `idle`, or `harness_error` matches `SUSPENDED` only and never fires a false `COMPLETED`, `IDLE`, or `HARNESS_ERROR`.
  - Added `NotificationStore.GetLastNotification` and updated `NotificationDispatcher` so `IDLE` and `COMPLETED` deduplicate per activity period using the stored agent row as the single source of truth (`phase == running`, `activity == matchStatus` case-insensitively, and `activity_since > notification.created_at`), whereas all other triggers (`STALLED`, `WAITING_FOR_INPUT`, `HARNESS_ERROR`, `LIMITS_EXCEEDED`, `ERROR`, `SUSPENDED`, ...) retain status-based deduplication. Also narrowed `NotificationDispatcher.Start()` from `project.>.agent.status` to `project.*.agent.status` so non-status project events do not cross-match.
- **CLI consumers (`cmd/list.go`, `cmd/project_health.go`):**
  - `scion list` displays `"suspended · completed"`, `"suspended · idle"`, and `"suspended · harness error"` in the `PHASE` column, combines retained activity with `suspend_reason` as `"suspended · completed (idle)"`, and excludes suspended rows from unqualified `--activity` filters unless `--phase suspended` is also given.
  - `scion project health` excludes `suspended` rows from active `ProjectHealthSummary` activity counts, fixing the miscount where suspended agents inflated `Completed`, `Idle`, or `Blocked`.
- **Web helpers (`web/src/`):**
  - Added `isRetainedSuspendActivity` (`shared/types.ts`) and `suspendedActivityLabel` (`shared/agent-state-display.ts`), wired into `agentStatusBadge` and `chat-members.ts` tooltips (`TERMINAL_STATUSES` and wobble settle handling also recognise `idle`).
  - Updated `StateManager` (`client/state.ts`) to scope sticky-activity suppression (`waiting_for_input`, `completed`, `limits_exceeded`) to `running` (using a 4-state transition table in `CompactedDelta` to preserve exact monoid equivalence across buffered and seed-epoch replays), so suspend and resume transitions cleanly clear or replace prior sticky activities while `idle -> working` transitions remain immediate.

## Not in this phase

- Idle sweep and settings (phase 3), and dedicated `idle` icon/visual badge styling in `ACTIVITY_DISPLAY` (phase 4).
