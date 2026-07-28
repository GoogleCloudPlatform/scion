---
name: scion-cli-operations
description: >-
  Operational constraints for Scion agents running in containerized sandboxes.
  Covers non-interactive mode, prohibited commands, hub-only API access, and
  system message format. Complements the scion CLI reference and messaging skills.
---

# Scion CLI Operating Constraints

You are an autonomous Scion agent running inside a containerized sandbox. Your workspace is managed by the Scion orchestration system.

## Core Rules (DO NOT VIOLATE)

- **Non-Interactive Mode**: You MUST use the `--non-interactive` flag with the Scion CLI, ALWAYS. This flag implies `--yes` and will cause any command that requires user input to error instead of blocking. Failure to use `--non-interactive` can result in you getting stuck at an interactive prompt indefinitely.
- **Structured Output**: To get detailed, machine-readable output from nearly all commands, use the `--format json` flag.
- **Prohibited Commands**: DO NOT use the `sync` or `cdw` commands.
- **Agent State**: Do not attempt to resume an agent unless you were the one who stopped it. An 'idle' agent may still be working.
- **Hub API Only**: Do not use the `--no-hub` option to work around issues; you only have access to the system through the hub.
- **Don't Relay Instructions**: The agents you start are informed by these instructions — you don't need to tell them to use things like sciontool.
- **Do Not Use Global**: Never use the `--global` option; you are operating in a grove workspace and it is set implicitly by default.
- **Do Not Interact with Settings or Login Commands**.

## Shell Safety for Task Prompts

The `scion start` task prompt is embedded in a shell command. **Do not use backticks, `$variables`, or other shell metacharacters** in the inlined prompt — they cause the shell to exit before the agent starts, with no visible error. For long or formatted briefs, write the content to a file and pass a filepath reference instead:

```bash
scion start <name> --non-interactive \
  "Read your brief at /path/to/brief.md and follow it."
```

**Use absolute paths** in task prompts and briefs. A sub-agent's working directory may differ from yours — relative paths resolve against the sub-agent's `/workspace`, not yours.

## Working Tree Reset Safety

When cleaning a working tree, **use `git clean -fd`, not `git clean -fdx`**.

- `git clean -fd` removes untracked files but **respects `.gitignore`** — `.scion/`, agent state, and ignored directories survive.
- `git clean -fdx` deliberately defeats `.gitignore` and **deletes everything** not tracked by git, including `.scion/`, `downloads/`, and any local state.

The `-x` flag is not a stronger clean — it is a different operation. Default to `-fd`; use `-x` only with a specific reason and after verifying nothing irreplaceable is in an ignored directory.

**`downloads/` is an inbox, not storage.** Files downloaded into your container are visible only inside it and invisible to every other agent. Move anything worth keeping to `/scion-volumes/scratchpad/` (shared across all agents) promptly. A `downloads/` file that has not been drained to the scratchpad is one command from gone.

## Recommended Commands

- **Inspect an Agent**: `scion look <agent-id>` — inspect the recent output and current terminal-UI state of any running agent.
- **Full CLI Details**: `scion --help` — for specific details on all hierarchical commands.
- **Focused Usage**: Use the scion CLI as needed for your task. Do not pre-emptively explore `.scion` folders, read agent-template files, etc. — focus only on what you need.

## System Message Format

You may be sent messages via the system. These will include markers:

```
---BEGIN SCION MESSAGE---
---END SCION MESSAGE---
```

They will contain information about the sender and may be instructions, or a notification about an agent you are interacting with (for example, it completed its task or needs input).

See scion-messaging skill for more information on messages