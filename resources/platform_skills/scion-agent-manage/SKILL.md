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

6. **Preserve branches**: When deleting agents whose work might need review, use `--preserve-branch`.

## Model Override

To start an agent with a specific model (overriding the harness default), use `--config` with a flat YAML file:

```bash
printf 'model: claude-sonnet-4-20250514\n' > /tmp/agent-config.yaml
scion start <name> --non-interactive --config /tmp/agent-config.yaml
```

**Do NOT use `--harness-config` for this** — that flag expects a named harness configuration registered in the hub, not a model name or YAML file.

## Troubleshooting and Recovery

Most stuck agents are recoverable without recreation. Always start with `scion look <agent>` to inspect current state before acting.

### Triage Table

| Symptom | Cause | Recovery |
|---|---|---|
| Transient API error in log | LLM provider rate-limit or timeout | `scion message <agent> "continue"` — do NOT recreate |
| `LIMITS_EXCEEDED` state | Context/rate limit hit | `scion message <agent> "continue"` |
| `failed to verify token: error in cryptographic primitive` | Hub regenerated signing keys on restart | `scion message <agent> "continue"` (triggers token refresh) |
| `Token refresh failed … 401` repeating every 30s | Token expired; refresh deadlocked | `scion message <agent> "continue"` — if no effect, recreate |
| Container exit 255 / status `Exited` | Container crash | Recreate the agent |
| Phase `created`, lastSeen zero, persists 5+ min | Broker dispatch exceeded CLI timeout | Wait a few minutes; if stuck, delete and recreate |
| Phase `starting` + activity `completed` | Duplicate sciontool process reset phase via hub API | Kill duplicate process, recreate |
| Context at 100% | Memory/context limit reached | Send raw clear sequence (see below) |
| `gh` CLI returns 401 | Stale `GH_TOKEN` env var, not an agent state issue | Fall back to `curl` with known-good token |
| Rebase fails "unrelated histories" | Shallow clone hides common ancestor | `git fetch --unshallow` then retry rebase |
| Interactive prompt blocking agent | Harness waiting for user input | `scion message <agent> --raw "ENTER"` or appropriate dismissal |

### Context Clear

When an agent's context approaches 100%, clear it manually with raw terminal input:

```bash
scion message <agent> --raw "/"
scion message <agent> --raw "clear"
scion message <agent> --raw "ENTER"
```

Always `scion look <agent>` first to verify screen state before sending raw input.

### Anti-Patterns

- **Recreating on first sign of trouble.** Most stuck states recover with `scion message <agent> "continue"`. Recreation destroys uncommitted work and in-memory state.
- **Sending raw input blind.** Always `scion look` first — raw keystrokes go to whatever is on screen.
- **Treating all 401s the same.** Hub token 401 (agent state) and GitHub token 401 (API auth) have different recovery paths.
