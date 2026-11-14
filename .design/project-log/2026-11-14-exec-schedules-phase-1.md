# Exec schedules Phase 1: one-shot exec events (API only)

**Date:** 2026-11-14
**Branch:** scion/exec-schedules-1 (stacked on scion/exec-schedules-b, ptone/scion#7921, at fc05a00b; itself on scion/exec-schedules-a, ptone/scion#7916)
**Issue:** ptone/scion#7883 (Phase 1 of ptone/scion#7818)
**Design of record:** scratchpad `projects/exec-schedules/design/design.md` (AGREED, D1-D11), "Implementation Phases" -> Phase 1.

## What this branch adds

- **Storage**: additive Ent table `scheduled_exec_runs` (PK `event_id`, no DB foreign key: the purge deletes runs first, in its own transaction) and `ScheduledExecRunStore`: insert, mark running, a compare-and-set finish (`finished_at IS NULL`, first writer wins) that writes the event in the same transaction only while it is still `fired` or `expired`, a paginated unfinished list, and a by-event lookup for responses. New scheduled-event status `skipped` (the status column is a free string; no enum migration). No destructive change, so no downgrade-floor bump.
- **Authoring** (`handlers_scheduled_exec.go`, `authorize_scheduled_exec.go`): `eventType: "exec"` on `POST /scheduled-events` with `agentId`/`agentName`, `command`, `timeoutSeconds` (or a raw `payload`). It is answered like an unknown type unless `hub.exec_schedules` and `hub.conduit` are both on. The payload is decoded strictly and checked against the bounds again at fire time. The target must exist in the project. Authorization is `agent.attach` through `authorizeAgentTargetAction`, the live `/exec` rule; an unreadable target answers 404, so there is no existence oracle. Managed runtimes get 422. Quotas are 20 per target and 200 per project (pending one-shot events; recurring joins in Phase 2) and answer 429. A per-principal rate limiter allows 1/s with a burst of 10. `/schedules` and `/scheduled-events` create and PATCH bodies are limited to 64 KiB.
- **Fire** (`scheduled_exec_fire.go`): flag check, then the 10-minute missed window, then `authorizeScheduledExecFire` (revision liveness and admission, the target by stored id only, the frozen ceiling allows `agent.attach`, the live attach rule, standing). Then preflight: running, not managed, not reincarnating. Then admission: the tracked-run bound (500, never waits) and the shutdown gate. The run row is inserted, then one `exec-start` goes through the launch-pinned client. A detached tracker starts only after `fireEvent` has recorded the event; this is the new `afterScheduledRecord` hook, so the tracker never races that write. The singleton sweeper runs every minute under its own advisory lock. It polls unfinished runs no local tracker owns, in bounded pages with 32 calls in flight, round-robin across agents, and abandons runs past their lease. Shutdown cancels trackers without recording anything, so the sweeper records the real outcome after a restart.
- **Generic `scheduledSkip`** sentinel in `scheduler.go`: `fireEvent` records `skipped` with the reason (not a failure).
- **AC18 hub gate**: `StartAgentExecRun` refuses (`ErrExecSchedulesDisabled`, nothing sent) unless both flags are on and conduit is served. `callAgentControlPinned` no longer checks `conduitRoutable`, so `AgentExecRunStatus` keeps recovering admitted runs after either flag is turned off. Launch pinning is unchanged.
- **Fail-closed serializer** (`scheduled_response.go`): every handler that writes a schedule or scheduled-event body (create, list, get, history, PATCH, pause, resume and `/admin/scheduler`) builds DTOs here. Exec argv is redacted to `{agentId, agentName, timeoutSeconds, commandRedacted, argc, commandSha256}` unless the caller's current credential holds live `agent.attach` on the target; the creator is not exempt. The admin view never reveals argv. `execSummary` (outcome, exit code, duration, output bytes, times) goes to every reader. List envelopes keep `scope`; only their element types changed.
- **Audit** (`scheduled_exec_audit.go`): `schedule.exec.create`, `delete`, `refused`, `skipped`, `admit` and `outcome` through the AuditLogger, with a slog fallback. Records are content-free: argv0 basename (sanitized, at most 64 bytes), `shell_form`, digest and argc only.
- **authzop**: `schedule.event.fire` gains `fireEvent:exec` and the two exec routes as internal-only call sites.

## Decisions taken while implementing

- **Held target**: `failed` with `exec refused` and a `schedule.exec.refused` audit record. `scheduledFireStanding` checks it inside `authorizeScheduledExecFire`, as for scheduled messages: a hold is a moderation refusal, not a not-running case. exec-schedules-arch confirmed this and corrected AC 4 and the D4 preflight list in design.md, which had listed held under skipped.
- **Unknown-start bound**: a run never seen running whose `exec-status` is 404 is definitive after `started_at + 30 s (call) + 60 s (not_after) + 10 s (tolerance) + 10 s`. The hub cannot read the exact `not_after` that `StartAgentExecRun` set, so the call timeout is added as margin.
- **Recurring exec** stays rejected at authoring (Phase 2). A stored recurring exec event that ever reaches the handler fails with a fixed text.
- Quotas count pending one-shot exec events only. Phase 2 adds active recurring exec schedules.
- Fire-time "relationship removed" is tested by removing the creator's project membership. The owner relationship requires active project access, and `UpdateAgent` does not persist owner changes.

## Tests

- Store (`pkg/store/entadapter`): round trip, one run per event, first-writer-wins under 16 concurrent finishers, the event written only from fired or expired, paging, purge removes runs with events.
- Hub authoring (`scheduled_exec_authoring_test.go`): the user and agent matrix (owner, UAT with and without `agent:attach`, project-admin and hub-admin denied, full-role self, child and sibling allowed, another user's agent, a role without the lifecycle scope and a delegated credential denied), managed 422, either flag off, payload bounds, body limit, quotas, rate limiter, create/delete audit, target deletion cancels pending events.
- Hub fire authz (`scheduled_exec_fire_authz_test.go`): the fire-time matrix and revocations (relationship removed, user suspended, UAT revoked, role downgraded, target held, legacy attribution, missing target id never falls back to the name), preflight skips, either flag off, missed window, invalid stored payloads, capacity.
- Hub real routes (`scheduled_exec_fire_test.go`, in-process sciontool exec routes over a real Conduit session): outcomes (ok, exit status, timeout, start failed) and `execSummary`; the no-output sentinel (rows, API, logs, audit); no session, routes not advertised and agent busy skipped; detached fire; AC18 per flag; hub restart then sweeper; tracker vs concurrent sweepers (one outcome audited); agent stopped, launch changed, run forgotten; unknown start inside and after the window; ambiguous start never resent; purge and target deletion mid-run; sweeper lease and fairness.
- Redaction guard (`scheduled_response_guard_test.go`): enumerates every `authzop` catalog route under `/schedules`, `/scheduled-events` and `/admin/scheduler`, fails on an uncovered route, and asserts no argv sentinel for a reader without attach, the creator with a UAT without `agent:attach`, and the creator after losing attach.

## Limits

- No Postgres run here (no Postgres environment in the agent container); the store code uses only portable predicates.
- Daemonized descendants escape the process group (SEC-P4); revocation does not cancel an admitted run (F8).
- `AuditLogger` emission is best effort; the durable record is the run row (SEC-P6, F9).
