---
name: scion-agent-manage
description: Manage concurrent LLM-based code agents with scion - orchestrate parallel agents with isolated workspaces, troubleshoot and recover stuck agents
---

# Scion Agent Management Skill

Scion is a container-based orchestration tool for managing concurrent LLM-based code agents. It enables parallel execution of specialized sub-agents with isolated identities, credentials, and workspaces.

## Core Concepts

### Projects
A **project** is the grouping construct for agents in scion.

### Agents
An **agent** is an isolated LLM instance running in a container with a mounted workspace, credentials, and configuration.

### Templates
**Templates** are blueprints for creating agents.

### Harnesses
A **harness** is the LLM interface (Gemini CLI, Claude Code, etc.) that the agent uses.

## Command Reference

The best and most current reference for the CLI commands is available from `scion --help`. Some best practices are in the scion-cli-operations skill.

## Tips for Agents

1. **Check existing agents first**: Before starting a new agent, use `scion list` to see what's already running.

2. **Use descriptive names**: Agent names should reflect their purpose (e.g., `refactor-auth`, `test-api`, `audit-security`).

3. **Choose appropriate templates**: Use `--type researcher` for a researcher.

4. **Monitor with logs**: Use `scion logs <agent>` to check progress without interrupting.

5. **Interrupt carefully**: The `--interrupt` flag on messages stops current work - use only when necessary.

6. **Preserve branches**: Use `--preserve-branch` to keep the branch after deletion for later review. The flag does not push — confirm the branch is on the remote first.

## Briefing

Every agent you create needs a brief. Write the brief to a **shared scratchpad file and
pass the filepath** — do not inline a long brief into the creation command.

```bash
scion start <name> --non-interactive \
  "Read your brief at /scion-volumes/scratchpad/briefs/<name>.md and follow it."
```

A brief states:

| Section | Content |
|---|---|
| Task | what to do, in one or two sentences |
| Context | what has already been decided, and where to read it |
| Boundaries | what is explicitly out of scope |
| Deliverable | what artifact is owed, and in what shape |
| Reporting | who to report to, and when — including who to ask when blocked ([see below](#direct-questions-to-the-person-who-can-answer-them)) |

### Direct questions to the person who can answer them

When an agent needs a decision or input, it should ask the person named in the
brief's **Reporting** row — not relay through the coordinator unless the
coordinator *is* that person. An agent created to work with a specific user or
lead already knows who to ask; routing the question through an intermediary
wastes a round trip and risks the question being reframed in transit.

When writing a brief, make the Reporting row explicit enough that the agent
knows who to message for decisions. If different questions go to different
people, say so.

For shell-escaping rules when passing prompts, see the `scion-cli-operations` skill —
do not improvise quoting.

## Model Override

To start an agent with a specific model (overriding the harness default), use the `--model` flag:

```bash
scion start <name> --non-interactive --model claude-sonnet-4-20250514
```

**Do NOT use `--harness-config` for this** — that flag expects a named harness configuration registered in the hub, not a model name.

For troubleshooting agents that are stalled, have hit an error, or are stuck see references/troubleshooting.md

For agent lifecycle rules — when to delete, when to stop, and who may authorize deletion — see references/agent-lifecycle.md
