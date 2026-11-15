# Claude Code sub-agent hook-payload fixture

`hook-payloads-2.1.296.jsonl` is a real, captured sequence of the stdin
payloads Claude Code `2.1.296` sends to its command hooks. It covers one
print-mode (`claude -p`) turn in which the parent agent launches one
foreground `general-purpose` sub-agent that runs a single shell command.

The capture used a throwaway project directory and loaded project settings
only. A project-level hook copied each payload's stdin to a file. No other
hooks ran.

Hooks were registered for these events only: `SessionStart`, `PreToolUse`,
`PostToolUse`, `SubagentStop`, `Notification` and `Stop`. No `Notification`
fired in this turn (no permission prompt). Claude Code sends other events
too, for example `UserPromptSubmit`, which were not hooked, so these 7
records are not everything Claude Code sends in a turn.

## Contents (7 records, in event order)

1. `SessionStart` (parent)
2. `PreToolUse` `Agent` (parent: launches the sub-agent)
3. `PreToolUse` `Bash` (sub-agent: carries `agent_id` and `agent_type`)
4. `PostToolUse` `Bash` (sub-agent)
5. `SubagentStop` (sub-agent)
6. `PostToolUse` `Agent` (parent: the sub-agent has returned)
7. `Stop` (parent)

The sub-agent's events carry the parent session's `session_id` and name
the child only in `agent_id`. The parent's own events have no `agent_id`.
In this version the parent's sub-agent tool is named `Agent`.

## Scrubs

- `session_id`, `prompt_id`, `agent_id` and `tool_use_id` are replaced with
  synthetic values.
- `cwd` is replaced with `/workspace`.
- `transcript_path` and `agent_transcript_path` are replaced with synthetic
  paths under `/home/user`.
- The shell command, the tool descriptions and the sub-agent prompt are
  replaced with short synthetic text.
- `tool_response` and `last_assistant_message` are removed entirely.

Records are re-serialised with sorted keys. All other fields and values are
as captured.

# Claude Code API-error hook-payload fixtures

Claude Code 2.1.325 (`@anthropic-ai/claude-code` npm), TUI started as
`claude --dangerously-skip-permissions 'Say hello'` with
`ANTHROPIC_BASE_URL` pointed at the mock and every event in
`harnesses/claude/home/.claude/settings.json` plus `StopFailure`
registered.

- `api-error-400-2.1.325.jsonl`: 400 on every call; a second prompt typed.
- `api-error-401-2.1.325.jsonl`: 401 on every call.

A failed turn fires **`StopFailure` instead of `Stop`**, with `error` a bare
category (`"unknown"` for the 400, `"authentication_failed"` for the 401)
and `last_assistant_message` the provider text (`"API Error: 400 ..."`).
(Headless `claude -p` fired no `StopFailure` for the 401; the TUI did.)

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
