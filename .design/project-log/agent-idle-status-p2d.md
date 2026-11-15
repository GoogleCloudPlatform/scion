# Agent idle status, phase 2d: retained activity across suspend and per-period notification dedupe

**Date:** 2026-11-15
**Branch:** scion/agent-idle-status-p2d
**Issue:** ptone/scion#7946 (parent ptone/scion#7826)

## What changed

- **State model (`pkg/agent/state`):**
  - Added `Activity.IsRetainedOnSuspend()` (`idle`, `completed`; `#7858` will add `harness_error`) and `RetainedSuspendActivity(activity, stalledFrom)`.
  - Updated `AgentState.Validate()` to permit `PhaseSuspended` combined with a retained activity alongside `PhaseStopped` with a terminal activity.
  - Kept legacy `DisplayStatus()` returning `"suspended"` for `PhaseSuspended` regardless of retained activity so older CLIs, brokers, and federation remain unaffected.
- **Store & Hub suspend/resume (`pkg/store`, `pkg/hub`):**
  - `UpdateAgentStatus` now computes and persists `RetainedSuspendActivity` when transitioning to `suspended` without an explicit activity, clearing `blocked`, `waiting_for_input`, `working`, `stalled_from_activity`, and `tool_name` (`activity_since` clears automatically when leaving `running`).
  - Replaced in-memory `agent.Activity = ""` blanking across all three suspend call sites (`suspendAgent`, `autoSuspendStalledAgent`, `stopHeldAgent`) with `syncAgentAfterStopRecord` so HTTP responses and SSE `AgentStatusEvent` payloads carry the retained or cleared activity.
  - Cleared retained activity across every resume path: `UpdateAgentStatus` (`ClearTerminalRemnants` and `suspended -> running`), async launch `beginLaunch` (`start`/`restart`), `handleExistingAgent`'s whole-row resume writes, and `wakeAgentForDM` before `waitForAgentReady` so DM wake waits for the new run's activity rather than returning immediately on pre-suspend `completed`/`idle`.
  - Removed `omitempty` from `AgentStatusEvent.Activity` so an explicit empty string reaches the web client whenever activity is cleared on suspend or resume.
- **Notification matching & per-period dedupe (`pkg/hub/notifications.go`):**
  - `notificationEventStatus` returns `evt.Phase` (`"suspended"`) whenever `evt.Phase == "suspended"`, so suspending an agent with retained `completed` or `idle` matches `SUSPENDED` only and never fires a false `COMPLETED` or `IDLE`.
  - Added `NotificationStore.GetLastNotification` and replaced status-only deduplication (`GetLastNotificationStatus`) with per-period deduplication combining in-process observed transitions (`lastObservedStatus`) and persisted `agents.activity_since` vs `notification.created_at`. Each `idle` period and each `completed` period notifies once (`idle -> working -> idle` notifies twice), while heartbeats and re-reports within the same period remain deduplicated across both live events and Hub restarts.
- **CLI consumers (`cmd/list.go`, `cmd/project_health.go`):**
  - `scion list` displays `"suspended · completed"` and `"suspended · idle"` in the `PHASE` column and excludes suspended rows from unqualified `--activity` filters unless `--phase suspended` is also given.
  - `scion project health` excludes `suspended` rows from active `ProjectHealthSummary` activity counts, fixing the miscount where suspended agents inflated `Completed`, `Idle`, or `Blocked`.
- **Web helpers (`web/src/`):**
  - Added `isRetainedSuspendActivity` (`shared/types.ts`) and `suspendedActivityLabel` (`shared/agent-state-display.ts`), wired into `agentStatusBadge` and `chat-members.ts` tooltips (`TERMINAL_STATUSES` and wobble settle handling also recognise `idle`).
  - Updated `StateManager` (`client/state.ts`) to scope sticky-activity suppression (`waiting_for_input`, `completed`, `limits_exceeded`) to `running` (using a 4-state transition table in `CompactedDelta` to preserve exact monoid equivalence across buffered and seed-epoch replays), so suspend and resume transitions cleanly clear or replace prior sticky activities while `idle -> working` transitions remain immediate.

## Not in this phase

- `suspend_reason` and auto-wake policy (phase 2b), idle sweep and settings (phase 3), and dedicated `idle` icon/visual badge styling in `ACTIVITY_DISPLAY` (phase 4).
