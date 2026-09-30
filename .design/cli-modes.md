# Design: CLI Modes (Human / Assistant / Agent)

## Status: Proposal

## 1. Problem Statement

The `scion` CLI exposes a broad surface area of commands covering agent lifecycle management, infrastructure operations (hub, broker, server), grove administration, template management, and more. All of these commands are available to every caller regardless of context, which creates two problems:

1. **AI assistants used by humans** (e.g., Claude Code, Gemini CLI acting as a coding assistant) have access to commands that are impractical or dangerous to invoke without a graphical or web-based interface — server installation, interactive authentication flows, and infrastructure administration. An AI assistant that discovers these commands may attempt to use them, leading to confusion or partial state.

2. **Agents running inside containers** currently hit a coarse "no Hub endpoint" error gate (the existing `checkAgentContainerContext` check), but when they *do* have a Hub endpoint, they get the entire CLI surface. Agents should see commands relevant to their role: orchestrating sibling agents within their grove, communicating with the orchestrator, inspecting status, and coordinating work. They should not be able to manage infrastructure, administer the Hub, or perform operations outside their grove scope.

The solution is a tiered CLI mode system that progressively restricts the available command set.

## 2. Mode Definitions

### 2.1. `human` (Default)

The unrestricted mode. All commands are available. This is the mode used when a person directly invokes the CLI from a terminal.

### 2.2. `assistant`

Used when a human drives the CLI through an AI coding assistant (Claude Code, Gemini CLI, Cursor, etc.). The assistant is acting on behalf of a human operator who has access to a web UI or terminal for operations that require interactivity or complex interactive setup.

**Removed relative to `human`:**

| Command | Reason |
|---------|--------|
| `hub auth login` | Interactive browser-based OAuth flow |
| `hub auth logout` | Session management — use web UI or direct terminal |
| `hub token` (all subcommands) | Token lifecycle management — security-sensitive, use web UI |
| `hub secret migrate-names` | GCP Secret Manager name migration (ptone/scion#2152) — a maintenance operation with IAM/rollback implications, use direct terminal |
| `grove reconnect` | Infrastructure recovery — use direct terminal |
| `config migrate` | Configuration migration — use direct terminal |
| `config cd-config` | Shell-level directory change — not useful from an AI assistant |
| `config cd-grove` | Shell-level directory change — not useful from an AI assistant |
| `cdw` | Shell-level directory change — not useful from an AI assistant |
| `clean` | Destructive grove removal — use direct terminal |

### 2.3. `agent`

Used inside agent containers. Agents can orchestrate sibling agents within their grove (create, start, stop, look, etc.) and coordinate work through messaging and scheduling. They cannot manage infrastructure, administer the Hub, or modify grove-level configuration.

**Removed relative to `assistant`:**

| Command | Reason |
|---------|--------|
| `server` (all subcommands) | Infrastructure administration — not relevant inside a container |
| `broker` (all subcommands) | Runtime broker administration — not relevant inside a container |
| `hub` (all subcommands) | Hub interaction — agents communicate via the messaging and notification systems, not direct Hub commands |
| `init` | Grove initialization — infrastructure concern |
| `grove` (all subcommands) | Grove administration — infrastructure concern |
| `templates` (all subcommands) | Template management — managed by the operator |
| `harness-config` (all subcommands) | Harness configuration — managed by the operator |
| `config` (all subcommands) | Configuration inspection/mutation — managed by the operator |
| `doctor` | Diagnostic tool — not relevant inside a running container |
| `messages` (all subcommands) | Agents receive messages directly; inbox polling is unnecessary |
| `completion` | Shell completion generation — not useful inside a container |
| `shared-dir create/remove` | Shared directory lifecycle — managed by the operator |
| `sync` | Workspace syncing — managed by the operator |
| `clean` | Already removed in `assistant` |
| `cdw` | Already removed in `assistant` |
| `restore` | Agent restoration — managed by the operator |
| `attach` | Interactive terminal attachment — not meaningful from inside a container |
| `schedule create` | Schedule lifecycle management — managed by the operator |
| `schedule create-recurring` | Schedule lifecycle management — managed by the operator |
| `schedule delete` | Schedule lifecycle management — managed by the operator |
| `schedule pause/resume` | Schedule lifecycle management — managed by the operator |

> **Note on scheduling:** Schedule mutation commands are excluded for now to keep the initial agent surface simple. A future revision may selectively allow these for agents with appropriate permissions.

## 3. Mode Selection Mechanism

### 3.1. Environment Variable: `SCION_CLI_MODE`

The mode is selected via the `SCION_CLI_MODE` environment variable:

| Value | Mode |
|-------|------|
| *(unset or empty)* | `human` |
| `assistant` | `assistant` |
| `agent` | `agent` |

Any unrecognized value is treated as `human` with a stderr warning.

### 3.2. Agent Mode Injection

When the runtime provisions an agent container, it sets `SCION_CLI_MODE=agent` in the container environment alongside the existing `SCION_AGENT_NAME`, `SCION_HOST_UID`, etc. This makes mode restriction automatic and invisible to the agent.

### 3.3. Assistant Mode Activation

Assistant mode can be activated through several mechanisms, in priority order:

1. **Environment variable:** `SCION_CLI_MODE=assistant` — works when the user can control the assistant's shell environment before launch.

2. **Settings file:** A `cli.mode` key in grove settings (`.scion/settings.json`), versioned settings (`.scion/versioned_settings.json`), or global settings (`~/.scion/settings.json`). This is the most practical approach for teams, since committing `"cli.mode": "assistant"` in versioned settings activates it for all AI assistants working in the project without requiring per-tool environment setup.

3. **`scion config set cli.mode assistant`:** A one-time command the user runs in their terminal before starting the assistant session.

The environment variable takes precedence over settings if both are set.

> **Future consideration:** AI assistants often run in environments where modifying the shell env post-launch is difficult. The settings-file approach avoids this by making the mode a project-level or user-level default. We may also explore auto-detection heuristics (e.g., checking `SCION_HARNESS` or parent process names) as supplementary signals, but these should never override an explicit setting.

### 3.4. No CLI Flag

There is intentionally **no** `--mode` CLI flag. The mode is an environmental property, not a per-invocation choice. This prevents agents from escalating their mode by passing `--mode human`.

## 4. Discoverability and Stealth

### 4.1. Hidden from Help

When commands are removed by mode restrictions, they are **fully removed from the command tree** — not merely hidden. This means:

- `scion --help` does not list restricted commands
- `scion <restricted-command> --help` returns "unknown command"
- Shell completions do not suggest restricted commands
- There is no indication in help text that other modes exist

### 4.2. No Mode Documentation in CLI

The `--help` output, command descriptions, and error messages make no reference to CLI modes. The mode system is a platform-level concern documented in operator/admin documentation, not in agent-facing text.

### 4.3. Error Messages

When a command is blocked by mode restrictions, the error is a generic "unknown command" — identical to the error for a truly nonexistent command. There is no "this command is restricted in your mode" message that would reveal the mode system's existence.

## 5. Implementation Approach

### 5.1. Command Removal at Init Time

In `cmd/root.go`, during `init()` or early in `PersistentPreRunE`, read `SCION_CLI_MODE` and remove disallowed commands from the Cobra command tree using `rootCmd.RemoveCommand()`. For nested subcommands, remove them from their parent.

This approach is preferred over a runtime check because:
- Removed commands disappear from help, completions, and the command tree entirely
- No possibility of bypass through direct invocation
- No leakage of command names in error messages
- Simpler than maintaining per-command annotations

### 5.2. Allow-List vs Deny-List

Each mode is defined as an **allow-list** of permitted command paths (e.g., `"message"`, `"config.list"`, `"schedule.get"`). The implementation iterates over registered commands and removes any not on the allow-list for the current mode. This is safer than a deny-list because new commands are restricted by default until explicitly allowed.

### 5.3. Relationship to `checkAgentContainerContext`

The existing `checkAgentContainerContext` function handles a specific scenario: the CLI is inside a container *without* a reachable Hub. The new mode system is orthogonal — it restricts commands even when a Hub *is* available. The two checks compose:

1. **Mode restriction** runs first and removes commands from the tree
2. **Container context check** runs second (in `PersistentPreRunE`) and gates remaining commands on Hub reachability

Once the mode system is in place, `checkAgentContainerContext` can be simplified or folded into the agent mode logic, since agent mode already restricts the command set to those that require a Hub.

### 5.4. Agent Self-Stop Safety

Agents can stop themselves and other agents via `scion stop`. However, `scion stop --all` poses a risk: an agent may not realize it is included in "all" and inadvertently terminate itself mid-task. The implementation should either:
- Exclude the calling agent from `--all` in agent mode (requiring explicit `scion stop --self` for self-termination), or
- Print a warning and require `--yes` confirmation when `--all` is used from within an agent container

### 5.5. No Debug Logging

The mode system intentionally does **not** produce any debug or diagnostic output, even when `SCION_DEBUG=1` is set. Any log message referencing the mode, the environment variable name, or the number of removed commands could reveal the restriction mechanism to an agent inspecting its own output.

## 6. Affected Existing Behavior

### 6.1. Runtime Container Provisioning

`pkg/agent/run.go` (and harness-specific environment builders) must inject `SCION_CLI_MODE=agent` into the container environment. This is a one-line addition to the existing environment map.

### 6.2. Settings Layer

`pkg/config/` gains support for a `cli.mode` setting key, following the existing settings resolution order (env var > versioned settings > grove settings > global settings). The environment variable `SCION_CLI_MODE` takes precedence.

### 6.3. Test Coverage

The existing `TestCheckAgentContainerContext` tests should be extended to cover mode-based command filtering. A table-driven test can verify that each mode's allow-list produces the expected set of available commands.

## 7. Command Availability Summary

| Command | `human` | `assistant` | `agent` |
|---------|:-------:|:-----------:|:-------:|
| `attach` | Y | Y | - |
| `broker` (all) | Y | Y | - |
| `cdw` | Y | - | - |
| `clean` | Y | - | - |
| `config list` | Y | Y | - |
| `config set` | Y | Y | - |
| `config get` | Y | Y | - |
| `config validate` | Y | Y | - |
| `config migrate` | Y | - | - |
| `config dir` | Y | Y | - |
| `config cd-config` | Y | - | - |
| `config cd-grove` | Y | - | - |
| `config schema` | Y | Y | - |
| `create` | Y | Y | Y |
| `delete` | Y | Y | Y |
| `doctor` | Y | Y | - |
| `grove init` | Y | Y | - |
| `grove list` | Y | Y | - |
| `grove prune` | Y | Y | - |
| `grove reconnect` | Y | - | - |
| `grove service-accounts` (all) | Y | Y | - |
| `harness-config` (all) | Y | Y | - |
| `hub status` | Y | Y | - |
| `hub groves` (all) | Y | Y | - |
| `hub brokers` (all) | Y | Y | - |
| `hub enable` | Y | Y | - |
| `hub disable` | Y | Y | - |
| `hub link` | Y | Y | - |
| `hub unlink` | Y | Y | - |
| `hub auth` (all) | Y | - | - |
| `hub token` (all) | Y | - | - |
| `hub secret` (all) | Y | Y | - |
| `hub secret migrate-names` | Y | - | - |
| `hub env` (all) | Y | Y | - |
| `hub notifications` | Y | Y | - |
| `init` | Y | Y | - |
| `list` | Y | Y | Y |
| `logs` | Y | Y | Y |
| `look` | Y | Y | Y |
| `message` | Y | Y | Y |
| `messages` | Y | Y | - |
| `messages read` | Y | Y | - |
| `notifications` (all) | Y | Y | Y |
| `restore` | Y | Y | - |
| `resume` | Y | Y | Y |
| `schedule list` | Y | Y | Y |
| `schedule get` | Y | Y | Y |
| `schedule cancel` | Y | Y | Y |
| `schedule create` | Y | Y | - |
| `schedule create-recurring` | Y | Y | - |
| `schedule pause` | Y | Y | - |
| `schedule resume` | Y | Y | - |
| `schedule delete` | Y | Y | - |
| `schedule history` | Y | Y | Y |
| `server` (all) | Y | Y | - |
| `shared-dir list` | Y | Y | Y |
| `shared-dir create` | Y | Y | - |
| `shared-dir remove` | Y | Y | - |
| `shared-dir info` | Y | Y | Y |
| `start` | Y | Y | Y |
| `stop` | Y | Y | Y |
| `sync` | Y | Y | - |
| `templates` (all) | Y | Y | - |
| `version` | Y | Y | Y |
| `help` | Y | Y | Y |
| `completion` | Y | Y | - |

## 8. Open Questions

1. **Should `assistant` mode be opt-in or auto-detected?** Auto-detection (e.g., checking if `TERM_PROGRAM` indicates an AI tool) is fragile. The settings-file approach (section 3.3) is the most practical default, but supplementary auto-detection could be explored.

2. **`template` (singular alias):** The `template` command is a convenience alias for `templates`. It should follow the same restriction as `templates`.

3. **Permission-gated schedule management for agents:** Currently excluded for simplicity, but orchestrator agents may benefit from creating and managing schedules. A future revision could allow this based on agent permissions or template configuration.
