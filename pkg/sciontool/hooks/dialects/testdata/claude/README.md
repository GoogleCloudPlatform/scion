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
