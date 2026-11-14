# Agent idle status, phase 1: `idle` end to end

**Date:** 2026-11-14
**Branch:** scion/agent-idle-status-p1
**Issue:** ptone/scion#7944 (parent ptone/scion#7826)

## What changed

- `state.ActivityIdle` ("idle"): agent-reported only, sticky, implies running.
  `Activity.IsSuspendEligible()` (idle, completed) is defined now as the single
  list the later idle-suspend sweep reads; nothing calls it yet.
- `sciontool status idle "<reason>"`: same path as blocked (local sticky write,
  event log, `POST /status` with `activity=idle`; no wire change). The help lists
  idle with the blocked / idle / task_completed rule.
- Hook handlers need no code change: they key on `Activity.IsSticky()`, so idle
  is kept wherever blocked is and is cleared by the same `isNewWorkEvent` events.
  The handler table tests gained an idle row next to every blocked row.
- Store: idle is in `stalledExcluded` and `staleOfflineExcluded`. New nullable
  column `agents.activity_since` (additive, `DowngradeFloor` unchanged).
- Hub: IDLE notification text ("<slug> is IDLE: <message>"); IDLE in the
  create-default subscription triggers and the CLI `defaultTriggers`.
  `scion project health` has an Idle count. `scion list` needed no code change
  (it validates and shows activities generically); a test pins it.

## activity_since: how it is maintained

The column is store-managed and never written from a caller's struct:

- `stampActivitySince` (pkg/store/entadapter/agent_activity_since.go) runs in
  every writer that knows the stored values: `UpdateAgentStatus`,
  `MarkStalledAgents`, `MarkStaleAgentsOffline`, `CreateAgent`, and
  `UpdateAgent`. It sets the column when the activity value changes or the row
  enters running, leaves it alone for an unchanged value, and clears it when the
  phase is not running or the activity is empty.
- `UpdateAgent` is a version CAS with no read of the old activity, so the
  existing `appliedConfigForWrite` read now also selects phase and activity.
  A status report between that read and the CAS (status reports do not bump
  `state_version`) can leave the column one change behind; the whole-row write
  overwrites that report's activity anyway.
- `entc.ClearLaunchResumePhaseHook` also clears the column on any write of a
  phase other than running, which covers the writers that only move phase
  (launch reaper, container missing, delete engine).

## Not in this phase

Web idle badge and trigger list (phase 4), guidance text beyond the sciontool
help (phase 2a), suspend reason, auto-wake, the idle sweep.

## Finding

Notification dedupe (`GetLastNotificationStatus`) skips a notification whose
status equals the subscription's previous one. Working is not a trigger, so an
agent that goes idle, works, and goes idle again notifies its parent only the
first time. COMPLETED already behaves this way. Long-lived leads that cycle
through idle may want this revisited.
