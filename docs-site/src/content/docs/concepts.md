---
title: Scion Concepts
---

This document defines the core concepts and terminology used in Scion.

## Core Concepts

### Agent
An **Agent** is an isolated process running an LLM + Harness loop (aka Agent) against a task. It acts as an independent worker with its own identity, credentials, and workspace. An agent is the fundamental unit of execution in Scion.

### Project
A **Project** (or **Group**) is a project workspace where agents live. It corresponds to a `.scion` directory on the filesystem. It can exist at the project level (generally located at the root of a git repository), or globally in the users home folder.

Every project has a unique **Project ID**. Git-backed projects use deterministic **UUID v5** identifiers (derived from the namespace and normalized git URL), ensuring the same repository always maps to the same ID regardless of protocol. Hub-native projects use random **UUID v4** identifiers.

### Hub
The **Hub** is the central control plane of a hosted Scion architecture. It acts as the "brain" of the system, coordinating state across multiple users, projects, and runtime brokers.
- **Identity & Auth**: Manages user identities (via OAuth) and issues tokens for brokers and agents.
- **State Persistence**: Stores the definitive state of agents, projects, and templates in a central database.
- **Orchestration**: Dispatches agent lifecycle commands to the appropriate Runtime Brokers.
- **Collaboration**: Provides a shared view of the system via the Web Dashboard and Hub API.

### Profile
A **Profile** defines a complete execution environment by binding a specific **Runtime** to a set of behavior flags and **Harness** configuration overrides.
- Profiles allow you to switch between different environments (e.g., "Local Docker", "Production Kubernetes") without modifying agent templates.
- They are defined in the global or project `settings.yaml`.

### Harness-Configuration
A **Harness-config** adapts a specific underlying LLM tool or agent software (like Gemini CLI, Claude Code, or OpenAI Codex) into the Scion ecosystem.
- It handles the specifics of provisioning, configuration, and execution for that particular tool inside an OCI container.
- Examples: `GeminiCLI`, `ClaudeCode`, `Codex`, `OpenCode`.
- The harness ensures that the generic Scion commands (`start`, `stop`, `attach`, `resume`) work consistently regardless of the underlying agent software.

### Template
A **Template** is a blueprint for creating an agent. It defines the base configuration, system prompt, and tools that an agent will use.
- Templates are stored in `.scion/templates/` and can be project-level or global (`~/.scion/templates/`).
- Users can manage templates using the `scion templates` command suite (`create`, `clone`, `list`, `show`, `update-default`).
- Scion comes with default templates for supported harnesses (e.g., `gemini`, `claude`, `opencode`, `codex`), but users can create custom templates for specialized roles (e.g., "Security Auditor", "React Specialist").


### Runtime
The **Runtime** is the infrastructure layer responsible for executing the agent containers.
- Scion abstracts the container execution, allowing it to support different backends.
- **Docker**: The standard runtime for Linux and macOS.
- **Podman**: A daemonless, rootless alternative to Docker for Linux and macOS.
- **Apple Container**: Uses the native Virtualization Framework on macOS for improved performance.
- **Kubernetes**: Allows running agents as Pods in a Kubernetes cluster, enabling remote execution and scaling at production scale.

### Runtime Broker
A **Runtime Broker** is a compute node (e.g., a server, laptop, or K8s cluster) that registers with a **Scion Hub** to provide execution capacity.
- It manages the local lifecycle of agents dispatched from the Hub.
- It handles workspace synchronization, template hydration, and log streaming.
- For more details, see the [Runtime Broker Guide](/scion/hub-user/runtime-broker/).

### Agent State Model

Agent state uses a **layered model** with three dimensions:

- **Phase** — The lifecycle stage of the agent container:
  `created` → `provisioning` → `cloning` → `starting` → `running` → `stopping` → `stopped` (or `error`)

- **Activity** — What the agent is doing within the `running` phase:
  `idle`, `thinking`, `executing`, `waiting_for_input`, `blocked`, `completed`, `limits_exceeded`, `offline`

- **Detail** — Freeform context about the current activity (tool name, message, task summary).

This separation allows the UI and API consumers to distinguish between infrastructure lifecycle events (provisioning, stopping) and the agent's cognitive state (thinking, waiting for input). Activities like `completed`, `blocked`, and `limits_exceeded` are "sticky" — they persist until the agent is explicitly restarted or stopped. The `blocked` activity is set by agents themselves when they are intentionally waiting for an expected event (such as a child agent completing), which prevents the system from falsely marking them as stalled.

The `offline` activity status occurs when an agent heartbeat has not been heard from for some time. Currently, this may be due to an agent being unable to refresh its auth token, which disconnects it from sending its heartbeat and other updates. These agents can be stopped and restarted to be provisioned with a new auth token. They should be able to refresh this token as long as they can maintain a connection to the Hub.

## Detailed Architecture

### A full approach to sub-agents

 Because an agent through its template can contain home folder content, env var definitions, and custom mounts that collectively exposes all configuration available to the harness (e.g., gemini-cli) scion-agents are not limited by the constraints of a harness' built-in sub-agent feature. While they are acting as sub-agents from the point-of-view of the Scion tool user-as-orchestrator, they are full agents in their capabilities.

### Agent Ancestry & Identity Scoping

To support multi-agent workflows, Scion implements **Agent Ancestry Chains**. When an agent spawns a child agent, the system tracks this relationship (`root` → `parent` → `child`). This ancestry chain is critical for **transitive access control**: any principal (user or agent) that exists in an agent's creation chain automatically gains access to manage that descendant agent. 

Furthermore, agent identities are **strictly scoped by their project** (e.g., `project--agent`). This naming convention prevents name collisions across different workspaces and ensures agents can only interact with peers and progeny within their designated boundary. Progeny agents also receive granular secret access controls to prevent privilege escalation.

### Workspace Strategy

Scion uses one of two strategies to give each agent an isolated git workspace, depending on whether a Hub is in use.

**Local mode — Git Worktrees:**
When running without a Hub, Scion uses [Git Worktrees](https://git-scm.com/docs/git-worktree) for isolation.
- A new worktree is created at `../.scion_worktrees/<project>/<agent>` with a dedicated branch.
- The worktree is mounted into the agent's container as `/workspace`.
- Agents operate on the same repository history but have independent working directories.
- Work is merged back to the main branch manually (e.g., `git merge <agent-branch>`).

**Hub mode — Git Init + Fetch:**
When a Hub is enabled, all git-based projects (including locally linked ones) use a robust `git init` + `git fetch` provisioning strategy instead of worktrees.
- The broker injects `SCION_GIT_CLONE_URL`, `SCION_GIT_BRANCH`, and a `GITHUB_TOKEN` into the container.
- `sciontool init` inside the container initializes the workspace and fetches the repo over HTTPS, then checks out a `scion/<agent-name>` branch.
- This approach handles workspaces that already contain `.scion` metadata or `.scion-volumes` directories, clearing stale artifacts before initialization.
- SSH credentials on the host are not used; a `GITHUB_TOKEN` is required.
- This strategy is consistent across all broker machines, whether or not the repo exists locally.

This distinction means a project that was previously used in local mode will switch to clone-based provisioning once it is linked to a Hub. See the [About Workspaces](/scion/advanced-local/workspace/) guide for details.

### Resource Isolation
Scion enforces strict isolation between agents to prevent interference and cross-contamination of credentials or data.
- **Filesystem**: Each agent has a dedicated home directory (host path mounted to container) containing its unique history and configuration.
- **Shadow Mounts (tmpfs)**: Scion uses `tmpfs` shadow mounts to definitively prevent agents from accessing `.scion` configuration data or other agents' workspaces within the same project.
- **Environment**: Environment variables are explicitly projected into the container.
- **Credentials**: Sensitive credentials (like `gcloud` auth) are mounted read-only or injected via environment variables, ensuring they are available only to the specific agent.
- **Externalized Project Data**: Non-git project data and agent home directories are externalized to ensure they cannot be traversed by agents in the workspace.
- **Shared-Workspace Per-Agent Isolation**: In hub-hosted git projects where multiple agents share a single workspace mount, each agent's per-agent state (its task prompt and resolved configuration) is held outside that shared mount, so sibling agents in the same project cannot read each other's state through the workspace view.

### Contextual Agent Instructions
Scion automatically tailors an agent's operational context by appending supplemental instructions based on the workspace environment.
- **`agents-git.md`**: Appended when an agent is running in a Git-backed workspace, providing context on worktree management and branch workflows.
- **`agents-hub.md`**: Appended when an agent is connected to a Scion Hub, providing instructions for interacting with the Hub API and reporting status.
These extensions ensure agents understand their specific execution environment without requiring manual configuration in every template.

### Plugin System

Scion supports a plugin architecture built on `hashicorp/go-plugin` for extending system capabilities. Plugins communicate over gRPC and can provide implementations for:

- **Message Broker Plugins**: Custom message delivery backends for agent notifications and structured messaging.
- **Agent Harness Plugins**: Custom harness implementations that integrate new LLM tools into Scion without modifying the core codebase.

The plugin system is currently in its foundational stage, with reference implementations available for both plugin types.
