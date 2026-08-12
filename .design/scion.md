# Scion Local Orchestration Design

This document covers the core design principles for the local `scion` CLI and its orchestration of containerized agents. For a comprehensive overview of the entire Scion platform (including Hosted architecture), see [AGENTS.md](../AGENTS.md).

## Architecture

Scion uses a Manager-Worker architecture where the `scion` CLI (Manager) orchestrates isolated containerized environments (Workers) for LLM agents.

### Key Design Pillars

1. **Strict Isolation**: Each agent operates in a dedicated container with its own home directory, credentials, and configuration.
2. **Concurrent Workspaces**: Uses `git worktree` to provide each agent with an isolated, concurrent view of the codebase on its own feature branch.
3. **Grove Contexts**: Groups agents into "Groves" (projects) for organizational and resource management.
4. **Harness Agnostic**: Supports multiple LLM harnesses (Gemini, Claude, etc.) through a standardized interface.

## Implementation Details

- **Worktree Management**: Worktrees are created in `../.scion_worktrees/` to avoid polluting the main project tree.
- **Agent Status**: Agents report state via a JSON status file in their home directory, enabling the Manager to monitor progress and detect when an agent is awaiting user input.
- **Interactivity**: The `attach` command connects the host TTY to the agent's container, facilitating human-in-the-loop debugging and confirmation.

For detailed operational guides and technical specifications of other components (Hub, Runtime Broker, etc.), refer to the respective files in `.design/hosted/`.
