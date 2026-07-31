---
name: scion-cli-operations
description: >-
  Operational constraints for Scion agents running in containerized sandboxes.
  Covers prohibited commands and hub-only API access.
  Complements the scion CLI reference and messaging skills.
---

# Scion CLI Operating Constraints

You are an autonomous Scion agent running inside a containerized sandbox. Your workspace is managed by the Scion orchestration system.

## Core Rules (DO NOT VIOLATE)

- **Structured Output**: To get detailed, machine-readable output from nearly all commands, use the `--format json` flag.
- **Prohibited Commands**: DO NOT use the `sync` or `cdw` commands.
- **Hub API Only**: Do not use the `--no-hub` option to work around issues; you only have access to the system through the hub.
- **Don't Relay Instructions**: The agents you start are informed by these instructions — you don't need to tell them to use things like sciontool.
- **Do Not Use Global**: Never use the `--global` option; you are operating in a grove workspace and it is set implicitly by default.
- **Do Not Interact with Settings or Login Commands**.

## Shell Safety for Task Prompts

The `scion start` task prompt is embedded in a shell command. **Do not use backticks, `$variables`, or other shell metacharacters** in the inlined prompt — they cause the shell to exit before the agent starts, with no visible error. For long or formatted briefs, write the content to a file and pass a filepath reference instead:

```bash
scion start <name> \
  "Read your brief at /path/to/brief.md and follow it."
```

**Use absolute paths** in task prompts and briefs. A sub-agent's working directory may differ from yours — relative paths resolve against the sub-agent's `/workspace`, not yours.

**Avoid non-ASCII escape sequences in shell strings.** Bash only expands `\x` hex escapes inside `$'...'` quoting — inside double quotes, `\xe2\x80\x94` sends 12 literal characters, not an em-dash. Use plain ASCII punctuation in task prompts and messages.

## Recommended Commands

- **Know Yourself**: `scion whoami` — shows your agent identity, project, template, and configuration. Use `--format json` for structured output.
- **Inspect an Agent**: `scion look <agent-id>` — inspect the recent output and current terminal-UI state of any running agent.
- **Full CLI Details**: `scion --help` — for specific details on all hierarchical commands.
- **Focused Usage**: Use the scion CLI as needed for your task. Do not pre-emptively explore `.scion` folders, read agent-template files, etc. — focus only on what you need.
