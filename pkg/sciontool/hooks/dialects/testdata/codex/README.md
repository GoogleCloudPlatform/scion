# Codex API-error hook-payload fixture

`api-error-400-0.169.0.jsonl`: `@openai/codex` 0.169.0 (npm), TUI started as
`codex --sandbox danger-full-access --dangerously-bypass-approvals-and-sandbox
--dangerously-bypass-hook-trust 'Say hello'`, with a custom `responses`
provider pointed at the mock and every hook in `config.toml` registered.

A turn whose model call fails fires **no `Stop` hook and no `notify`**
(`agent-turn-complete`): only `SessionStart` and `UserPromptSubmit`. The
headless `codex exec` run behaved the same. So the hook path has no error
to report and no turn end: the agent stays `thinking`, and it never becomes
`completed` (criterion 15). Codex model errors are left to the OTel
API-error watchdog and the first-turn watchdog.

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
