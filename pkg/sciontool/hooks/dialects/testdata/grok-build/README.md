# Grok Build API-error hook-payload fixture

`api-error-400-1.0.77.jsonl`: grok 1.0.77 (x.ai installer), `grok --yolo
--sandbox off --no-ask-user --trust --model mock 'Say hello'` with every
hook event `provision.py` registers, and a `[model.mock]` block in
`config.toml` pointing a `chat_completions` backend at the mock. A second
prompt was typed, then grok was killed (the last `SessionEnd` and `Stop`,
reason `shutdown`).

Each failed turn is `UserPromptSubmit`, `Notification`
(`notificationType: "agent_error"`), `StopFailure` with `error:
"invalid_request"` and `errorDetails: "API error (status 400 Bad Request):
..."`. Every payload carries both `hookEventName` (snake_case, e.g.
`stop_failure`) and `hook_event_name` (PascalCase), which is why the
dialect tries `hook_event_name` first. The dialect ignores (`skip_when`) the
`agent_error` Notification, which is not a question for the user, and the
`Stop` with reason `shutdown`, which ends no turn.

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
