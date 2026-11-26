# Muse Code hook-payload fixtures

## `api-error-400-1.4.20.jsonl`

Muse Code 1.4.20 (`dev.meta.ai` installer), `muse --yolo --base-url <mock>
--model muse-spark 'Say hello'` with the hook registration of
`harnesses/muse-code/home/.config/muse/settings.json`. The mock served a
one-model catalog at `/muse-code/models` and answered model calls with 400. A
second prompt was typed.

A failed turn fires **no `Stop`**: `UserPromptSubmit`, `PreLLMCall`,
`PostLLMCall` with `error: "API error 400: ..."` (one more
`PreLLMCall`/`PostLLMCall` pair comes from muse's internal `skill-reminder`
sub-agent). The hook path has a model-end error but no failed turn end, so it
does not set `harness_error`.

### How `api-error-400-1.4.20.jsonl` was captured

Captured 2026-11-14 for ptone/scion#7937. The CLI ran in its interactive TUI
inside tmux (as Scion runs it), from a throwaway home and project, with the
harness's own hook registration pointed at a script that appended each stdin
payload to a file. The model endpoint was a local mock that answered every
request with HTTP 400 and the body `Permission 'aiplatform.endpoints.predict'
denied on resource (or it may not exist).` in the provider's error shape. No
real credentials were used. A follow-up prompt was typed into the TUI. The
capture scripts for the API-error fixtures are kept with the evidence in the
project's scratchpad (`evidence/b2a-hook-fixtures/scripts`).

Scrubs (`scrub.py`): capture-host paths become `/workspace` and `/home/user`;
UUIDs become synthetic `0000000N-0000-4000-8000-...` values; timestamps are
zeroed; bulky fields with no status signal (`stack`, `options`, `messages`,
`tools`, `system_prompt`, `llm_request`, `workspacePaths`) are removed. Records
are re-serialised with sorted keys. Everything else is as captured.

## `session-usage-1.4.25.jsonl`

Muse Code 1.4.25 (`1.4.25-R9361.1`, `dev.meta.ai` installer), `muse exec --yolo
--base-url <mock> --model muse-spark 'Say hello'` with the hook registration of
`harnesses/muse-code/home/.config/muse/settings.json`.

### How `session-usage-1.4.25.jsonl` was captured

Captured 2026-11-26 for ptone/scion#9247 and ptone/scion#2236 against a
throwaway local Python HTTP server implementing `/muse-code/models` and
OpenAI Responses SSE `POST /responses` (no real credentials or Meta API calls;
the throwaway mock/capture script was deleted after capture and is not stored
in the repository). Six `PostLLMCall` records from four live runs (UUIDs
scrubbed to `0000000N-0000-4000-8000-...`, keys sorted; all wire fields and
token counts verbatim):

1. Single streaming call with cached and reasoning tokens (`input_tokens: 1000`
   inclusive of `cached_tokens: 250`, `output_tokens: 300` inclusive of
   `reasoning_tokens: 80`).
2. Two-step tool loop on one `turn_id` (`step: 0` tool call `input=800,
   cached=200, output=120, reasoning=40`; `step: 1` final answer reporting
   **turn-cumulative** totals `input=2000, cached=800, output=270,
   reasoning=70`, whereas `session.jsonl` `model_completed` recorded the
   per-step `1200/600/150/30`).
3. Two-step turn where `step: 0` succeeds and `step: 1` fails with HTTP 400:
   `step: 1` repeats `step: 0`'s cumulative `usage` (`800/200/120/40`) with
   `status: "failed"`, `error: "API error 400: ..."`, `response_id: null`.
4. Subagent (`goal-reminder`) `PostLLMCall` carrying its own `session_id` and
   `turn_id`.
