# Design: CLI Modes (Human / Agent)

## Status: Proposal

> **Update (2026-10):** The `assistant` mode described in earlier revisions of this document has been removed; it was superseded and is no longer supported. Two modes remain: `human` (the default) and `agent`. A `SCION_CLI_MODE=assistant` environment variable or `cli.mode: assistant` setting is ignored: the CLI prints one warning to stderr and runs in `human` mode (see section 3.3).

## 1. Problem Statement

The `scion` CLI exposes a broad surface area of commands covering agent lifecycle management, infrastructure operations (hub, broker, server), grove administration, template management, and more. All of these commands are available to every caller regardless of context, which creates a problem:

**Agents running inside containers** currently hit a coarse "no Hub endpoint" error gate (the existing `checkAgentContainerContext` check), but when they *do* have a Hub endpoint, they get the entire CLI surface. Agents should see commands relevant to their role: orchestrating sibling agents within their grove, communicating with the orchestrator, inspecting status, and coordinating work. They should not be able to manage infrastructure, administer the Hub, or perform operations outside their grove scope.

The solution is a CLI mode system that restricts the available command set inside agent containers.

## 2. Mode Definitions

### 2.1. `human` (Default)

The unrestricted mode. All commands are available. This is the mode used when a person directly invokes the CLI from a terminal.

### 2.2. `agent`

Used inside agent containers. Agents can orchestrate sibling agents within their grove (create, start, stop, look, etc.) and coordinate work through messaging and scheduling. They cannot manage infrastructure, administer the Hub, or modify grove-level configuration.

**Removed relative to `human`:**

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
| `clean` | Destructive grove removal — managed by the operator |
| `cdw` | Shell-level directory change — not useful inside a container |
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
| `human` | `human` |
| `agent` | `agent` |

Any unrecognized value is treated as `human` with a stderr warning.

### 3.2. Agent Mode Injection

When the runtime provisions an agent container, it sets `SCION_CLI_MODE=agent` in the container environment alongside the existing `SCION_AGENT_NAME`, `SCION_HOST_UID`, etc. This makes mode restriction automatic and invisible to the agent.

### 3.3. Settings Key and the Removed `assistant` Value

The mode can also be set with a `cli.mode` settings key (`scion config set cli.mode ...`). The environment variable takes precedence over settings if both are set. In practice only agent containers select a non-default mode, and they do so through the injected environment variable.

The `assistant` value selected a mode that has since been removed. If `SCION_CLI_MODE` or `cli.mode` is still set to `assistant`, the CLI ignores it, prints a single warning to stderr per process naming the source, and runs in `human` mode. It is never a hard failure, so an old settings file or shell profile does not break the CLI.

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

| Command | `human` | `agent` |
|---------|:-------:|:-------:|
| `attach` | Y | - |
| `broker` (all) | Y | - |
| `cdw` | Y | - |
| `clean` | Y | - |
| `config list` | Y | - |
| `config set` | Y | - |
| `config get` | Y | - |
| `config validate` | Y | - |
| `config migrate` | Y | - |
| `config dir` | Y | - |
| `config cd-config` | Y | - |
| `config cd-grove` | Y | - |
| `config schema` | Y | - |
| `create` | Y | Y |
| `delete` | Y | Y |
| `doctor` | Y | - |
| `grove init` | Y | - |
| `grove list` | Y | - |
| `grove prune` | Y | - |
| `grove reconnect` | Y | - |
| `grove service-accounts` (all except `show`) | Y | - |
| `project service-accounts show` | Y | Y |
| `harness-config` (all) | Y | - |
| `hub status` | Y | - |
| `hub groves` (all) | Y | - |
| `hub brokers` (all) | Y | - |
| `hub enable` | Y | - |
| `hub disable` | Y | - |
| `hub link` | Y | - |
| `hub unlink` | Y | - |
| `hub auth` (all) | Y | - |
| `hub token` (all) | Y | - |
| `hub secret` (all) | Y | - |
| `hub secret migrate-names` | Y | - |
| `hub env` (all) | Y | - |
| `hub notifications` | Y | - |
| `init` | Y | - |
| `list` | Y | Y |
| `logs` | Y | Y |
| `look` | Y | Y |
| `message` | Y | Y |
| `messages` | Y | - |
| `messages read` | Y | - |
| `notifications` (all) | Y | Y |
| `restore` | Y | - |
| `resume` | Y | Y |
| `schedule list` | Y | Y |
| `schedule get` | Y | Y |
| `schedule cancel` | Y | Y |
| `schedule create` | Y | - |
| `schedule create-recurring` | Y | - |
| `schedule pause` | Y | - |
| `schedule resume` | Y | - |
| `schedule delete` | Y | - |
| `schedule history` | Y | Y |
| `server` (all) | Y | - |
| `shared-dir list` | Y | Y |
| `shared-dir create` | Y | - |
| `shared-dir remove` | Y | - |
| `shared-dir info` | Y | Y |
| `start` | Y | Y |
| `stop` | Y | Y |
| `sync` | Y | - |
| `templates` (all) | Y | - |
| `version` | Y | Y |
| `help` | Y | Y |
| `completion` | Y | - |

## 8. Open Questions

1. **`template` (singular alias):** The `template` command is a convenience alias for `templates`. It should follow the same restriction as `templates`.

2. **Permission-gated schedule management for agents:** Currently excluded for simplicity, but orchestrator agents may benefit from creating and managing schedules. A future revision could allow this based on agent permissions or template configuration.
