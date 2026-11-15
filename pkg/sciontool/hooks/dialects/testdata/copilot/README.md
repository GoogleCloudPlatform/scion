# Copilot CLI API-error hook-payload fixtures

GitHub Copilot CLI 1.0.112 (`@github/copilot` npm package), `copilot
--no-auto-update --allow-all -i 'Say hello'` in offline BYOK mode
(`COPILOT_OFFLINE=true`, `COPILOT_PROVIDER_BASE_URL` pointed at an
OpenAI-compatible mock), with the hook file `provision.py` writes. Copilot
names the event on the command line (`sciontool hook <event>`), so each
record has `hook_event_name` set as sciontool merges it.

- `api-error-400-1.0.112.jsonl`: every model call gets 400. The turn is
  `sessionStart`, `userPromptSubmitted`, `errorOccurred`; no `agentStop`.
- `transient-503-then-200-1.0.112.jsonl`: the first call gets 503, the
  retry 200. Copilot retries and the turn ends with `agentStop`.

`errorOccurred` is the same in both (`errorContext: "model_call"`,
`recoverable: true`), so it does not separate a failed turn from a retried
call. The dialect maps it to `model-end` with the error (a failed model
call), not to `agent-error` (a failed turn) and not to a notification
(which showed the agent as waiting for input). It is `uncounted`: copilot
sends no hook for a successful call, so counting failures would make
`max_model_calls` stop an agent for retried errors. Only `errorContext:
"model_call"` was captured; other contexts are ignored.

## How it was captured (all API-error fixtures)

Captured 2026-11-14 for ptone/scion#7937. The CLI ran in its interactive
TUI inside tmux (as Scion runs it; codex also once headless), from a
throwaway home and project, with the harness's own hook registration
pointed at a script that appended each stdin payload to a file. The model
endpoint was a local mock that answers every request with HTTP 400 (or 401)
and the body `Permission 'aiplatform.endpoints.predict' denied on resource
(or it may not exist).` in the provider's error shape. No real credentials
were used. Where a record shows a second turn, a follow-up prompt was typed
into the TUI. The capture scripts are kept with the evidence in the
project's scratchpad (`evidence/b2a-hook-fixtures/scripts`).

Scrubs (`scrub.py`): capture-host paths become `/workspace` and
`/home/user`; UUIDs become synthetic `0000000N-0000-4000-8000-...`
values; timestamps are zeroed; bulky fields with no status signal
(`stack`, `options`, `messages`, `tools`, `system_prompt`, `llm_request`,
`workspacePaths`) are removed. Records are re-serialised with sorted keys.
Everything else is as captured.
