# Exec schedules Phase 4: CLI, hubclient, and documentation

**Date:** 2026-11-15
**Branch:** scion/exec-schedules-4
**Issue:** ptone/scion#7885 (Phase 4 of ptone/scion#7818)
**Design of record:** scratchpad `projects/exec-schedules/design/design.md` (AGREED, D1-D11), "CLI", "Read API and redaction", "Migration / Rollout", "Implementation Phases" -> Phase 4, and AC 18 (amended 2026-11-15).

## What this branch adds

- **`pkg/hubclient`** (`scheduled_events.go`, `schedules.go`):
  - Adds `Payload string`, `Command []string`, and `TimeoutSeconds int` (`omitempty`) to `CreateScheduledEventRequest`.
  - Adds `ExecSummary` (`Outcome`, nullable `ExitCode`, `DurationMS`, `OutputBytes`, `StartedAt`, `FinishedAt`) and `ScheduledEvent.ExecSummary`.
  - Adds `AgentID string`, `Command []string`, and `TimeoutSeconds int` (`omitempty`) to `CreateScheduleRequest` and `UpdateScheduleRequest` (plus `AgentName` on `UpdateScheduleRequest`), and documents `target_deleted` on `Schedule.StatusReason`.
- **CLI** (`cmd/schedule.go`):
  - `scion schedule create --type exec --agent <name> (--in|--at) [--timeout 60s] -- <argv...>` and `scion schedule create-recurring --type exec --name <name> --cron <expr> [--time-zone <zone>] --agent <name> [--timeout 60s] -- <argv...>`.
  - Reads `argv` strictly after `--` via Cobra `ArgsLenAtDash` (rejecting missing `--`, stray positional arguments before `--`, empty `argv`, >64 args, NUL bytes, and >4096B / >16384B bounds), rejects message-only flags (`--message`, `--interrupt`, `--wake`) on `exec`, and validates `--timeout` as a positive whole-second duration up to `3600s` (`60m`). Recurring `exec` requests are passed straight to the Hub so the Hub remains authoritative on Phase 2 availability and authorization.
  - No new subcommands; `agentAllowed` (`cmd/cli_mode.go`) and `windowsAllowed` (`cmd/platform_gate.go`) remain unchanged.
  - Detail printers (`scion schedule get <id>`): show target agent (`agentName` or `agentId`), quoted `argv` array or `<redacted, N args, sha256:...>` (and bare `<redacted>` fallback), timeout (defaulting `0` to `60s`), `execSummary` on event detail (`Outcome`, `Exit Code`, `Duration`, `Output`), and `Last Outcome` / `Last Exit Code` on recurring `exec` schedule detail (best-effort latest run from `/history`).
  - `scion schedule history <id>`: adds `OUTCOME`, `EXIT`, `DURATION`, and `OUTPUT` columns when any history event is `exec` or carries `execSummary`, preserving the 4-column layout for message schedules and full JSON output under `--format json`.
- **Documentation and skill**:
  - `docs-site/src/content/docs/hosted/user/scheduling.md`: adds the **Exec Schedules (Experimental)** section covering CLI creation, prerequisites (`hub.conduit` + `hub.exec_schedules`, agents launched/restarted with the flag, NTP-synchronised clocks), execution environment (user `scion`, cwd `/workspace` else home, harness shell launch env, `stdin` `/dev/null`, no stored output), timeouts (`1s`–`60m`, default `60s`), outcomes/overlap/quotas, long-running (>60m) backgrounding and `flock -n` mutual exclusion, `target_deleted` pause reason, `agent.attach` permissions and `argv` redaction (`commandSha256` described without encoding detail pending Phase 2), and documented limitations (no in-flight cancel, AC 18 `hub.conduit`-off outcome recovery only over sessions that stay connected, and `ptone/scion#8096` alpha gating).
  - `docs-site/src/content/docs/reference/experiments.md`: updates the `hub.exec_schedules` entry for CLI support, NTP requirement, AC 18 limitation, and `ptone/scion#8096` alpha gating.
  - `resources/platform_skills/scion-scheduler/SKILL.md`: adds agent guidance for `--type exec`, execution environment, timeouts, no-output / redaction semantics, and `flock`.

## Decisions and scope notes

- Per supervisor (`exec-schedules-em`) guidance: `commandSha256` is documented as "a SHA-256 of the command (`argv`)" without specifying HTML-escaped vs canonical JSON encoding because Phase 4 may land before Phase 2 (`ptone/scion#7884`).
- Strictly zero `pkg/hub/` files touched so Phase 2 proceeds with zero merge conflict.
- Note on Cobra/pflag testing: `pflag.FlagSet.Parse` does not reset `argsLenAtDash` across repeated parses on a singleton `cobra.Command`, so `cmd/schedule.go` registers create flags via helpers (`registerScheduleCreateFlags`, `registerScheduleCreateRecurringFlags`) that tests rebuild cleanly via `c.ResetFlags()`.
