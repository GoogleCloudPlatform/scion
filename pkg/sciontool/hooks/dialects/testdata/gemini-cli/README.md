# Gemini CLI API-error hook-payload fixture

`api-error-400-0.68.0.jsonl`: `@google/gemini-cli` 0.68.0 (npm),
`gemini --prompt-interactive 'Say hello'` with the hook registration of
`harnesses/gemini-cli/home/.gemini/settings.json`, `gemini-api-key` auth
and `GOOGLE_GEMINI_BASE_URL` pointed at the mock.

The turn is `SessionStart`, `BeforeAgent`, `BeforeModel`, `AfterAgent`.
There is no `AfterModel`, and `AfterAgent` has **no `error` field**
(`prompt_response` is `"[no response text]"`). The hook path cannot tell
the turn failed; Gemini CLI relies on the OTel API-error watchdog.

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
