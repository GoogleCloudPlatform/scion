# Conduit amendment: the exec control routes (`exec-start`, `exec-status`)

**Status: DRAFT for owner review** (Q5: ptone reviews this prose directly). Ships with Phase B (ptone/scion#7882) of exec-only schedules (ptone/scion#7818).

- Source of the decision: the exec-schedules design (scratchpad `projects/exec-schedules/design/design.md`, status AGREED 2026-11-14), decision **D9 = (b')** (ptone, msg c011a204: "b. we are all in on conduit"), section "Conduit design amendment (records D9)", security dispositions D9-S1, D9-S2 and D9-S3 (`security-review-d9.md`).
- Amends: the Conduit design v2.21 §3.7 handler rules ("control routes are data-only; no generic exec RPC over conduit") and the v2.22 deliver-message design §0 and §5.3 (`v2.22-deliver-message.md` in this directory), which rejected "C2, generic exec over conduit". v2.22's keyed-idempotency amendment of §3.7 is the base this text builds on.
- Implementation: `pkg/agentcontrol/exec.go` (wire contract), `pkg/sciontool/control/exec.go` and `exec_linux.go` (the routes), `pkg/hub/conduit_exec_control.go` (the hub's typed client).

## The rule, and the exception

The Conduit rule stands: control routes on an agent's session carry fixed, data-only schemas, and there is no generic exec RPC over Conduit. ptone, the design owner, approved exactly one exception, for scheduled exec. It is bounded as follows; anything outside these bounds still falls under the rule.

1. **Exactly two routes**, `exec-start` and `exec-status`, used only by the Hub's scheduler for `exec` events (its event handler, run tracker and sweeper). No other Hub caller, no user-facing endpoint, and no user-reachable relay of arbitrary argv. In particular there is no live `/exec` over Conduit, and the broker-facing `/deliver` endpoint stays fixed to `deliver-message` (no route-name, path or body passthrough).
2. **Authorization precedes every `exec-start`.** Each start follows the full fire-time authorization (D1: live `agent.attach` on the target under the schedule's authorization revision, its frozen ceiling and standing). `exec-status` is internal outcome recovery: it is scoped to an already-admitted run, its recorded target and its recorded launch; it cannot start anything; it does not re-check authority (D9-S3). Revoking authority after admission does not cancel a running command (follow-up F8, ptone/scion#7892). The route grants nothing beyond what live `/exec` by the same principal could do.
3. **Hub-originated RPC on the agent's own outbound session only.** sciontool serves the routes as the RPC handler of the session it dials; there is no listener, no loopback route, and no port-forward or tunnel path to them. The user-session RPC surface serves only `/v1/tunnels`; agent sessions have no RPC handler on the Hub. An in-container process gains nothing it does not already have: it can already run commands as the same user.
4. **Keyed idempotency by `run_id`, bounded argv, content-free logs, registration only with the launch-time flag** (details below).
5. **Off unless both Hub flags are on and the agent was launched with the flag.** The routes stay off for every agent unless `hub.conduit` and `hub.exec_schedules` are both on, and the agent was launched (or restarted) while they were. An agent started earlier does not advertise them until it restarts.

## Route contract

Both routes follow the §3.7 rules as amended by v2.22: POST only (405 otherwise), 404 for an unknown path, `application/json` bodies only (the RPC adapter forwards only `Content-Type`), unknown fields and trailing data are 400, no credential in either direction.

### `POST /v1/control/exec-start`

| field | rule |
|---|---|
| `v` | `1` |
| `run_id` | canonical lower-case UUID (the scheduled event id); the idempotency key |
| `schedule_id` | canonical UUID, or absent for a one-shot; the overlap scope |
| `argv` | 1..64 elements; `argv[0]` not empty; no NUL; each element ≤ 4096 bytes; total ≤ 16384 bytes (the Hub's payload bounds, checked again here) |
| `timeout_s` | 1..3600 |
| `not_after_unix_ms` | set by the Hub to its send time + 60 s |

| status | body | meaning |
|---|---|---|
| 202 | `{"state":"running"}` | started now |
| 202 | `{"state":"finished","outcome":"start_failed"}` | admitted, but the spawn failed (argv[0] not found on the run's PATH, privilege drop refused, environment unavailable); nothing ran |
| 200 | `{"state":"running"\|"finished", …, "replayed":true}` | this `run_id` is already known with the same binding; nothing new started |
| 409 | `{"outcome":"overlap"}` | another run of the same `schedule_id` still has a live main process |
| 409 | `{"outcome":"busy"}` | 2 runs already live in this container, or no tombstone slot can be reserved |
| 409 | `{"outcome":"mismatch"}` | `run_id` known but bound to another schedule, command digest, timeout or launch |
| 408 | `{"outcome":"expired_before_start"}` | the start deadline passed (at receipt or before spawn), or the spawn could not begin within 10 s of receipt |
| 400 | `{"outcome":"invalid","field":…}` | schema or bounds violation |

The `start_failed` 202 is a clarification of the design's response table, which listed only `running` for 202: a start that is admitted and reserved but whose spawn fails is recorded as a finished run (so `exec-status` and replays report it), and the start answers what happened rather than a `running` that the first status poll would contradict.

### `POST /v1/control/exec-status`

`{"v":1,"run_id":…}` → 200 `{"state":"running"}`, or 200 `{"state":"finished","outcome":"ok"|"nonzero"|"timeout"|"start_failed","exit_code":N,"duration_ms":N,"output_bytes":N}` (`start_failed` carries no numbers), or 404 `{"state":"unknown"}`. A read; safe to repeat. A status read (and a duplicate `exec-start`) for a run whose spawn decision is still pending waits for the decision, so a caller that waits gets the decided state and never `running` for a start that is then refused. The one exception is a caller that cancels while waiting: it is answered `running` (a replay for `exec-start`) without the decision. That answer goes to a call the Hub has already abandoned (its context ended, which also cancels the RPC), so the Hub never acts on it; the Hub's next status poll waits for the decision as usual.

`exit_code` is the main process's exit status, or 128 plus the signal number when a signal ended it.

### Hub side

The Hub calls both routes through `callAgentControlPinned` (the same call as `callAgentControlJSON`, routed with the run's launch rather than a fresh read of the agent row; only when the session advertises the route) with a 30 s timeout. It makes **one** `exec-start` attempt per run and never resends it; on an ambiguous result (timeout, drop after send) it only polls `exec-status`. Both calls are pinned to the launch the run was admitted under (D9-S3): the client reads the agent row once and sends nothing if its launch is not the run's; it then routes with that validated snapshot's incarnation, not a fresh read, and refuses, before sending, any resolved session that was not admitted with the run's launch id (including a session admitted through the interim generation fallback). A session's incarnation is fixed at admission and a relayed call is delivered to the resolved session id, so the call can reach only that launch's session, locally or through another relay, never a replacement incarnation. On a mismatch the run is abandoned. An answer that does not match the schema is an error, never success.

## Start-time guard (D9-S1, fail closed)

sciontool spawns only while `now ≤ not_after_unix_ms + 10 s` (fixed tolerance), checked at receipt and again immediately before the spawn, and only if the spawn begins within 10 s of receipt. There is no skew inference: an expired deadline is never treated as skew. Clock skew beyond the tolerance makes starts fail with `expired_before_start`, never run late; exec schedules need NTP-synchronised agent clocks. (An agent clock that runs *behind* cannot be detected this way; the receipt-to-spawn bound still applies.)

## Key store (D9-S2)

- **Live runs** sit in a separate, non-evictable map, bounded by the per-container cap of 2. A `run_id` is reserved in it atomically, under the store lock, before the spawn, together with the run's overlap and capacity checks, so concurrent duplicate starts cannot both spawn. A duplicate that arrives while the first start is between reservation and spawn waits for the spawn decision and then answers as a replay (or `expired_before_start` if the first start was refused).
- **Binding.** Each `run_id` is bound to (schedule id, sha256 of argv as a canonical JSON array, timeout, the agent's launch id). A start with the same `run_id` and any difference is `mismatch`.
- **Tombstones.** A finished run moves from the live map to a tombstone atomically (a replay sees one or the other, never neither). Tombstones hold the binding and the outcome only, are kept 60 minutes, at most 256, LRU, and an entry younger than 10 minutes is never evicted.
- **Capacity is reserved before the spawn.** A start that cannot reserve both a live slot and a future tombstone slot (tombstones under the 10-minute floor plus live runs plus this one must fit in 256) gets `busy`. The store never evicts early and never grows past its bound. Because a start cannot be admitted more than about 70 s after its send (`not_after` + tolerance), a tombstone always outlives any possible replay of its start; a `run_id` whose tombstone has been evicted is refused as expired, never started again.
- A sciontool restart is a container restart (sciontool is PID 1), so live processes die with the store, and the Hub sees 404 for the run.

## Execution

- `argv[0]` is resolved on the run's own `PATH` and executed directly; no shell is added (`["sh","-c",…]` is just argv).
- User and supplementary groups are the Conduit PTY user's, with the PTY path's privilege-drop rule: when a drop is required and no agent user is known, the start fails (`start_failed`); a run never executes with PID 1's credentials.
- Environment: the harness child's environment (the one its tmux session has, secret overrides included), built per run; `PWD` set to the working directory. Working directory `/workspace` if it exists, else the user's home. stdin `/dev/null`.
- Each run gets its own process group. On timeout sciontool sends SIGTERM to the group, then SIGKILL to the group 5 s later **even if the main process already exited**, so an ordinary child that ignores TERM still dies. The main process is not reaped until that sequence has finished (it is waited for with `waitid(WNOWAIT)` and registered with sciontool's PID 1 reaper the way the PTY client is), so no reused PGID is ever signalled. A daemonized descendant (`setsid`, double fork) escapes the group; that is a stated residual (SEC-P4), and the docs recommend `flock` for strict exclusion.
- On a normal exit the outcome is recorded at once and the group is left alone, so deliberately backgrounded children survive; `WaitDelay` (2 s) stops a background child that still holds the output pipes from delaying completion.
- stdout and stderr are drained continuously into a counting discard sink: nothing is buffered, memory stays bounded whatever the output size, and only the byte count is kept (D2 = b).
- Logs are content-free: run id, schedule id, status, outcome, exit code, duration and byte count. Never argv, output, or the text of an exec error, which can contain paths or argv; `start_failed` carries no error text.

## Registration

sciontool registers both routes, and so lists them in `Hello.capabilities.rpc`, only when its launch environment carries `hub.exec_schedules` in `SCION_HUB_EXPERIMENTS`. The Hub sends that entry at dispatch only while `hub.conduit` is served and `hub.exec_schedules` is on (`dispatchExperiments`, `agentExperimentNames`), the rule `deliver-message` uses. The executor needs Linux (`waitid(WNOWAIT)`); elsewhere the routes are never registered.
