# harness_error hook-path coverage across harnesses (ptone/scion#7937, part of ptone/scion#7858)

**Date:** 2026-11-14
**Branch:** scion/status-accuracy-7858-b2a (merged into scion/status-accuracy-7858)

## What B2a ships

- Captured hook payloads for a turn whose model call gets HTTP 400 (and 401 for claude), from the real
  CLIs against a local mock, committed as dialect fixtures with capture notes under
  `pkg/sciontool/hooks/dialects/testdata/`: claude 2.1.325, codex 0.169.0, gemini-cli 0.68.0,
  copilot 1.0.112 (also a 503-then-200 retry), antigravity 1.5.9, grok 1.0.77, muse-code 1.4.20.
- `hooks.EventAgentError` (`agent-error`): a turn-end error in the B1 harness_error branch and a
  readiness event. A sub-agent's agent-error changes nothing.
- claude: `StopFailure` registered and mapped to agent-error. Reason is the provider text from
  `last_assistant_message`, prefixed with the error category unless it is `unknown`.
- grok-build: `hook_event_name` is tried before `hookEventName`; `StopFailure` -> agent-error with
  `errorDetails`; the `agent_error` Notification (sent before every StopFailure, which would raise a
  false input-needed alert with the provider text) and the `Stop` with reason `shutdown` (sent after
  SessionEnd) are ignored.
- Dialect YAML gains two generic per-mapping keys: `skip_when` (conditions on payload fields; the event
  becomes `ignored`) and `uncounted` (`EventData.Uncounted`: max_model_calls, gen_ai.api.calls, usage and
  session model-call counts skip the event).
- copilot: `errorOccurred` (errorContext `model_call` only) -> uncounted `model-end` with the error
  (was `notification`, which set a sticky `waiting_for_input` with the raw provider body for a fatal 400
  and flickered it, with an input-needed notification, for a retried 503). Uncounted because copilot
  reports no successful calls: counting failures tripped max_model_calls on a retried 503.
- Telemetry: hook `Data.Error` is sanitized (escapes, one line, `logging.SanitizeText`, 300 runes) before
  it becomes a span status or span/log attribute.
- Web `harness error` badge (danger) with the reason as tooltip; sticky in the client store.
- `scion project health` counts `HarnessError`.

## Findings

- **What each harness does on a failed turn** (the docs table in `local/agent-lifecycle.md` is the
  summary): claude fires `StopFailure`, never `Stop`; antigravity fires `Stop` with `error` and no
  `PostInvocation`; grok fires `Notification` (agent_error) then `StopFailure`; codex fires neither `Stop`
  nor notify; gemini-cli fires `AfterAgent` with no error; copilot fires `errorOccurred` with the same
  payload as for a retried 503; muse-code fires no `Stop`, only `PostLLMCall` with `error`.
- **grok-build dialect matched nothing on 1.0.77.** Grok sends `hookEventName` in snake_case
  (`stop_failure`) next to a PascalCase `hook_event_name`; the dialect tried `hookEventName` first.
- **Criterion 15 (codex never completed)** already held on main after #4832 removed the notify
  auto-complete; B2a adds the fixture test.
- **Claude headless vs TUI:** `claude -p` fired no `StopFailure` for a 401; the TUI did. Capture in TUI.
- **Codex hook trust:** codex 0.169 refuses unreviewed hooks; Scion's base args already pass
  `--dangerously-bypass-hook-trust`. Capturing without it shows no hooks at all.
- **Muse model catalog:** muse needs `/muse-code/models` (`{"data":[{"id":...}]}`) and `--model`
  to reach the model with a custom `--base-url`.

## Open

- OpenCode bridge provider text (item 7): Closed (ptone chose B): the bridge keeps carrying only the
  error name and status (`APIError (status 400)`), and its "never carries provider message text" test
  stays.
