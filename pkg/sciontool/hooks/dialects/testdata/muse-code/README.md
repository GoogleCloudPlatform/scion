# Muse Code API-error hook-payload fixture

`api-error-400-1.4.20.jsonl`: Muse Code 1.4.20 (dev.meta.ai installer),
`muse --yolo --base-url <mock> --model muse-spark 'Say hello'` with the hook
registration of `harnesses/muse-code/home/.config/muse/settings.json`. The
mock served a one-model catalog at `/muse-code/models` and answered model
calls with 400. A second prompt was typed.

A failed turn fires **no `Stop`**: `UserPromptSubmit`, `PreLLMCall`,
`PostLLMCall` with `error: "API error 400: ..."` (one more
`PreLLMCall`/`PostLLMCall` pair comes from muse's internal
`skill-reminder` sub-agent). The hook path has a model-end error but no
failed turn end, so it does not set `harness_error`.

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
