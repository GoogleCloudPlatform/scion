# harness_error activity, vertical slice (ptone/scion#7936, part of ptone/scion#7858)

**Date:** 2026-11-14
**Branch:** scion/status-accuracy-7858

## Problem

A harness that cannot reach its model stays alive at its prompt. Its turn-end hook carries the error
(`Data.Error`), but the hook handlers ignored it and reported `working`/"Ready". About 5 minutes later the
agent showed as STALLED, with the stale "Ready" message and no reason.

## What B1 ships

- `state.ActivityHarnessError` (`harness_error`): sticky, implies running, label "harness error".
- `StatusHandler`/`HubHandler`: on the parent's `agent-end` with `Data.Error`, they set `harness_error` with
  reason `model API: <text>`. The text is single-lined, redacted with `logging.SanitizeText` and capped at
  300 runes. completed, limits_exceeded and crashed are kept.
- Clearing (`clearHarnessError` sticky policy): prompt-submit, agent-start and a new session-start (these
  already clear every sticky activity), tool-start, model-start, and an **error-free model-end**. End-of-turn
  working, readiness, an errored model-end and sub-agent events keep it. When a write replaces harness_error
  and leaves the reason in place, `StatusHandler.update` drops the local reason.
- Hub: `stalledExcluded += harness_error` (not `staleOfflineExcluded`); `HARNESS_ERROR` in the default notify
  triggers (Go and web), with a formatter branch. The STALLED message no longer appends the stale message;
  it appends `no activity for <age>` instead.
- CLI: `scion list` LAST ACTIVITY shows `harness error: <reason ≤40 runes>` and `stalled (was X)`. JSON
  output carries `detail.message`, `launch` and `stalledFromActivity`. `hubclient.Agent.StalledFromActivity`
  is added; `hubclient.AgentLaunch` becomes an alias of the new `api.AgentLaunch`.

## Findings and deviations from the design

- **model-end clears.** OpenCode emits no prompt-submit, agent-start or model-start on a later turn, so a
  text-only follow-up turn would never clear harness_error. Accepted by the EM (16:06Z).
- **Redactor.** Design §4.1 names the `pkg/sciontool/telemetry` redactor, but that one is key-based
  (attribute names). Free-text redaction is `pkg/util/logging.SanitizeText`.
- **Token isolation.** `pkg/sciontool/hub.NewClient` reads the token file from the `scion` user's home when
  that user exists, not from `$HOME`. Inside an agent container, a test that only sets `HOME` therefore reads
  the real agent token. The B1 tests and `hubRecorderSetup` call `hub.SetTokenHome(t.TempDir())`.
- The pending-error file for errored model-end/tool-end (§4.2(1)) is only needed by the first-turn watchdog,
  so it is left to B2.

## Validation

- A real `opencode-ai@1.19.5` run with the real bridge and the branch `sciontool`, against a mock 400 model
  and a recording fake Hub, produced harness_error with the reason, and a follow-up turn cleared it.
  sciontool built from main reported working/"Ready" instead. Evidence:
  `/scion-volumes/scratchpad/projects/status-accuracy/evidence/b1-opencode-e2e/`.
- The in-process end-to-end test is `pkg/hub/harness_error_e2e_test.go`.
