# Project Log: Broker Disconnect Reconnect Race Fix (Issue #131)

**Date:** 2026-06-05
**Task:** Unify broker disconnect race fix from two branches into PR #303

## Problem

When a broker disconnects and reconnects rapidly, the stale disconnect callback's
offline stamp can clobber the new connection's online status. The root cause is a
TOCTOU race: `ReleaseRuntimeBrokerConnection` and `UpdateRuntimeBrokerHeartbeat`
were separate calls — the heartbeat update has no session guard and unconditionally
overwrites status. Provider statuses are also clobbered and never restored by
heartbeats, leaving the broker permanently invisible until hub restart.

## Solution

Added `ReleaseAndMarkBrokerOffline` to the store interface — a single CAS write
that atomically clears affinity AND stamps status=offline, only if the session
still matches. If a concurrent reconnect has already claimed the broker with a
new session, the compare fails and the callback is a no-op.

Also added a re-check guard in `server.go` before updating provider statuses:
after the atomic release, re-read the broker to confirm no concurrent
`markBrokerOnline` has re-claimed it before stamping providers offline.

## Branch Unification

Two branches addressed this issue:
- `scion/dev-issue-131` (PR #303): had only a docs/project-log commit, no code fix
- `origin/fix/session-guarded-broker-disconnect` (fork PR #144): had the complete
  code fix with tests

The fork branch's fix was the more complete solution. Rebased PR #303 onto
upstream main and cherry-picked the fork's fix commit to produce a single
unified branch.

## Files Changed

- `pkg/store/store.go` — added `ReleaseAndMarkBrokerOffline` to `RuntimeBrokerStore` interface
- `pkg/store/entadapter/project_store.go` — implemented `ReleaseAndMarkBrokerOffline` with CAS retry loop
- `pkg/hub/server.go` — rewired `SetOnDisconnect` callback to use the atomic method + provider re-check guard
- `pkg/store/entadapter/broker_affinity_test.go` — 4 new tests covering the atomic method

## Verification

- All 10 broker affinity tests pass (4 new + 6 existing)
- Hub package compiles cleanly
- Pre-existing test failures in `pkg/hub` (unrelated to this change) confirmed on upstream main
