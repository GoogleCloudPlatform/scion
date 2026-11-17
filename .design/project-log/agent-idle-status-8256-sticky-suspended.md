# Suspended agents: ignore phase changes from agent and runtime reports on the locked row

**Date:** 2026-11-17
**Branch:** scion/agent-idle-status-8256
**Issue:** ptone/scion#8256 (part of ptone/scion#7826; prerequisite of ptone/scion#7947)

## What changed

- **Authoritative locked-row check in `UpdateAgentStatus` (`pkg/store/entadapter/agent_store.go`):**
  - When `isAgentReport(su)` (`su.ReportSource != ""`, set only by the agent status handler and the broker heartbeat handler) and `!su.Restore`, a locked row in `phase == "suspended"` now drops the report's status fields, mirroring `haltedStickyDrop`:
    - `Phase`: dropped, keeping `phase = "suspended"` so `entc.ClearSuspendReasonHook` never observes a phase mutation and never clears `suspend_reason`, and terminal-phase activity clearing never wipes a retained suspend activity.
    - `Activity`: dropped (both terminal activities such as `crashed`/`limits_exceeded` and live/other activities), preserving any retained suspend activity (`completed`, `idle`, `harness_error`) or empty activity, keeping `ToolName` and `StalledFromActivity` empty, and leaving `LastActivityEvent` and `ActivitySince` untouched (`statusOwned = false`).
    - `ExitReason` and `ExitCode`: dropped, because the hub intentionally stopped the container to suspend it and teardown signals or exit codes do not refine a suspension.
    - `ContainerStatus`: exited/stopped statuses apply to confirm teardown, while live statuses (`"running"`, `"Up ..."`) are held unless `run_intent == "running"`, matching `heartbeatContainerStatusHeld` (ptone/scion#3607).
    - Other harmless report fields (`Message`, `ConnectionState`, `RuntimeState`, `TaskSummary`, `CurrentTurns`, `CurrentModelCalls`, `StartedAt`, `LiveWork` timestamps, `LastSeen`, `Updated`) still apply, matching `haltedStickyDrop` and `Guard 0`.
  - Unlike `haltedStickyDrop`, `suspended` has no start-claim self-report exception: suspend paths release a superseded start claim after recording `phase = "suspended"`, and hub resume paths move the row to `starting` (`ReportSource == ""`) before launching the new container.
  - Reports naming a different `ReportRunID` continue to take the existing run-check early return (`Updated` and `LastSeen` only).
  - Hub-originated writes (`ReportSource == ""`), including resume/start (`suspended -> starting` and `suspended -> running`), failed-start rollback (`starting -> suspended` with `Restore: true`), user conversion of `suspend_reason`, explicit stop of a suspended agent, and deletion, apply unchanged.
- **Handler fast paths (`pkg/hub/handlers_agent_lifecycle.go`, `pkg/hub/handlers_runtime_brokers.go`):**
  - Kept `Guard 0` in `guardAgentPhaseTransition` and `agentSuspended` in `handleRuntimeBrokerHeartbeat` as fast paths (preventing unnecessary quota reconciliation on already-suspended rows), with comments cross-referencing the authoritative locked-row check in `UpdateAgentStatus`.
- **Tests (`pkg/store/entadapter`, `pkg/hub`):**
  - Store level (`agent_suspended_sticky_test.go`): matrix over `user`, `stalled`, and `idle` suspend reasons (each with and without a retained activity) against late agent-source (`stopping`, `stopped`, `error`/`crashed`, `limits_exceeded`, `running`) and runtime-source reports, plus superseded start-claim protection, hub lifecycle transitions (resume, failed-start restore, reason conversion, stop, delete), and different-`run_id` reports.
  - Hub level (`auto_wake_test.go`, `TestSuspend_ShutdownReportsKeepSuspendedAndAutoWakeable`): end-to-end auto-suspend where `DispatchAgentStop` triggers concurrent `sciontool` (`stopping`, `stopped` "Session ended") and broker heartbeat (`stopped`) reports whose handler reads saw `phase = "running"` before `phase = "suspended"` committed; verifies the row ends `suspended` with `suspend_reason = "stalled"` and retained `completed` activity, and remains auto-wakeable on a subsequent agent DM without `--wake`.
