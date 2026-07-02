# Workspace Skills

Platform-level skill definitions that are auto-injected into every Scion agent during provisioning. These skills provide the foundational knowledge every agent needs to operate within the Scion ecosystem.

## Skills

| Skill | Description | Injection |
|---|---|---|
| `scion/` | Scion CLI reference — agent management, templates, configuration commands | Unconditional |
| `team-creation/` | Team template creation and extension guide | Unconditional |
| `scion-cli-operations/` | Operational constraints — non-interactive mode, prohibited commands, hub-only access, message format | Unconditional |
| `scion-messaging/` | Messaging patterns — recipient types, timing, flags, coordination patterns | Unconditional |
| `agent-status-signals/` | Status signaling protocol — ask_user, blocked, task_completed | Unconditional |
| `git-sandbox/` | Git workflow protocol for sandbox/worktree environments — local-only ops, conflict resolution | Conditional: `git_workspace` only |

## Auto-Injection

All skills in this directory are automatically injected into every agent at provisioning time. Skills with an `inject_when` field in their SKILL.md frontmatter are conditionally injected based on the agent's workspace context (e.g., `git_workspace` means the skill is only injected when the workspace is a git repository).

## Structure

Each skill directory contains:
- **`SKILL.md`** — The skill definition (YAML frontmatter + markdown content)
- **`scripts/`** — Optional backing shell scripts for common operations
