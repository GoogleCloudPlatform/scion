# Exec schedules Phase B: sciontool exec-start / exec-status control routes

**Date:** 2026-11-14
**Branch:** scion/exec-schedules-b (stacked on scion/exec-schedules-a, ptone/scion#7916, at e9b909f1)
**Issue:** ptone/scion#7882 (Phase B of ptone/scion#7818)
**Design of record:** scratchpad `projects/exec-schedules/design/design.md` (AGREED), section "sciontool exec control routes (D9 = b')"; amendment text in `.design/conduit/exec-control-routes.md`.

## What this branch adds

- **Wire contract** (`pkg/agentcontrol/exec.go`): request types, strict decode, bounds (argc 64, element 4096, total 16384, timeout 1..3600), the argv digest (sha256 of the canonical JSON array), and `ParseExecStartAnswer` / `ParseExecStatusAnswer`, which map every documented status and body and turn anything else into `ErrExecSchema`.
- **sciontool routes** (`pkg/sciontool/control/exec.go`): the key store (non-evictable live map with atomic reservation before spawn; binding to schedule, digest, timeout and launch; tombstones 60 min / 256 / LRU with a 10-minute no-evict floor; capacity reserved before spawn), overlap per schedule, 2 live runs per container, the fail-closed `not_after` guard at receipt and before spawn with the 10 s receipt-to-spawn bound. Duplicate starts that arrive between reservation and spawn wait for the spawn decision.
- **Executor** (`exec_linux.go`, Linux only): argv[0] resolved on the run's own PATH, no shell, PTY user and supplementary groups with the fail-closed privilege drop, harness env, cwd `/workspace` else home, stdin `/dev/null`, `Setpgid`. The main process is watched with `waitid(WNOWAIT)` and registered with the PID 1 reaper, so on timeout the group gets SIGTERM, then SIGKILL 5 s later even after the leader exited, before the leader is reaped. Output goes to a counting discard sink; `WaitDelay` 2 s.
- **Registration**: `hub.exec_schedules` is registered (server layer, alpha, off), dispatched only while conduit is served, and sent to agents in `SCION_HUB_EXPERIMENTS`. sciontool init registers the routes only with it (`conduitExecOptions`). Run env is `supervisor.HarnessShellEnv`, the harness child's env (secret overrides included).
- **Hub client** (`pkg/hub/conduit_exec_control.go`): `StartAgentExecRun` / `AgentExecRunStatus` over `callAgentControlJSON` with a 30 s timeout. The hub sets `v` and `not_after` (send + 60 s), validates before sending, and pins each call to the run's launch id, checked before (nothing sent) and after (answer discarded) the call. Nothing calls it yet; Phase 1 (ptone/scion#7883) wires the scheduler.

## Decisions taken while implementing

- A spawn that fails after admission (argv[0] not found, privilege drop refused, env error) answers **202 `{"state":"finished","outcome":"start_failed"}`**, stored as a tombstone. The design table listed only `running` for 202. This is recorded in the amendment as a clarification.
- A process that a signal ended reports `exit_code` 128 + signal.
- 400 bodies follow Phase A: `{"outcome":"invalid","field":…}`.
- The experiment is registered in Phase B (exec-schedules-em agreed), server layer only; the web layer comes with the web phase.

## Tests

- sciontool unit tests (`exec_test.go`, deterministic fake executor): schema/400 table, lifecycle, concurrent duplicate starts held at the reservation, a duplicate of a start refused at spawn, mismatch (live and tombstone, each binding field, launch), overlap/busy, expired_before_start (receipt, tolerance edge, agent clock ahead/behind, queue delay past deadline and past receipt+10 s), tombstone capacity → busy, replay under churn (60-minute run plus 320 short runs over 53 minutes), a randomized interleaving stress run under `-race`, start_failed classes, privilege drop fail-closed.
- Real-process tests (`exec_linux_test.go`): outcomes and exit codes, env/cwd/stdin, a 200 MiB producer counted exactly with RSS growth under 48 MiB, timeout kills the group, a TERM-ignoring child killed after the leader exits (the leader is checked to be an unreaped zombie during the grace), setsid and same-group background children survive a normal exit within WaitDelay, real replay runs once, privilege drop fails closed, and a sentinel in argv/stdout/stderr never appears in answers or the sciontool log.
- sciontool init: registration only with the flag, Hello advertises the routes only with it, env parity through a real run against `supervisor.ChildEnv`, and a source guard that the control handler is only the session's RPC handler.
- Hub (shard 5): client round trip locally and cross-node, not reached (no session, not advertised), schema mismatch is an error, invalid request not sent, launch pinning, the real 30 s timeout with one RPC, dispatch and launch flag, user-session and agent-session RPC cannot reach the routes, `/deliver` cannot forward an exec body, and a source guard on who calls the client.

## Limits

- Daemonized descendants (`setsid`, double fork) escape the group (SEC-P4; docs to recommend `flock`).
- An agent clock that runs behind is not detectable by the `not_after` guard; only the receipt-to-spawn bound applies.
- Launch pinning checks the agent row before and after the call; `callAgentControlJSON` reads the incarnation itself, so a relaunch in the gap could still deliver a read-only status poll to the new incarnation (its answer is discarded). Closing it needs `callAgentControlJSON` to take expected incarnation facts, which is a Phase A change.
- The privilege drop itself (as opposed to its fail-closed refusal) is not exercised here: the test container is not root.
