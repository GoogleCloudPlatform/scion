---
title: Glossary
description: Standardized terminology for the Scion project.
---

This glossary defines key terms used throughout the Scion documentation and ecosystem.

### Agent
An isolated worker instance running an LLM harness. Each agent has its own identity, workspace, and configuration.

### Project
A project-level grouping of agents and configuration, typically corresponding to a git repository and a `.scion` directory.

### Harness
An adapter that allows an underlying LLM tool (like Gemini CLI or Claude Code) to run within the Scion orchestration layer.

### Hub
The centralized control plane in a hosted Scion deployment. It manages identity, project registration, and dispatches tasks to Runtime Brokers.

### Profile
A set of configuration overrides that define how a runtime should execute an agent (e.g., resource limits, environment variables).

### Runtime
The underlying technology used to execute agent containers (e.g., Docker, Podman, Apple Virtualization, Kubernetes).

### Runtime Broker
A compute node that executes agents. It connects to a Hub to receive instructions and reports agent status.

### sciontool
A helper utility bundled with Scion that is injected into agent containers to provide status reporting, metadata access, and task management.

### Template
A versioned blueprint for an agent, defining its base image, system prompt, tools, and initial state.

### Project ID
A unique identifier for a project. Git-backed projects use deterministic **UUID v5** identifiers derived from the normalized git URL. Hub-managed projects use random **UUID v4** identifiers.

### Plugin
An extension module built on `hashicorp/go-plugin` that provides additional capabilities (e.g., message broker or agent harness implementations) without modifying the Scion core.

### Shared Directory
A persistent, mutable storage volume shared between agents within a single project. Backed by host filesystem directories (local) or Kubernetes PersistentVolumeClaims (K8s).

### Workspace
The working directory mounted into an agent container, typically managed as a Git worktree (local mode) or provisioned via `git init` + `git fetch` (Hub mode) to ensure isolation from other agents.

### Phase
The infrastructure lifecycle stage of an agent, controlled by the platform: `created`, `provisioning`, `cloning`, `starting`, `running`, `stopping`, `stopped`, `suspended`, or `error`.

### Activity
What a running agent is doing within the `running` phase (e.g. `thinking`, `executing`, `waiting_for_input`, `blocked`, `completed`, `stalled`, `offline`). Activity is only meaningful while the phase is `running`.

### Suspend / Resume
**Suspend** tears down an agent's container while recording the intent to resume it later (phase `suspended`). **Resume** brings the agent back and *continues* its previous harness conversation (e.g. `--continue` for Claude Code, `--resume` for Gemini CLI) rather than starting fresh. Distinct from `stop`/`start`, which always begin a new session. Requires a harness that supports session resume.

### Error (crash)
The phase an agent enters when its process or container exits non-zero (a crash, OOM, or `SIGKILL`), carrying a message like `Agent crashed with exit code N`. The `error` phase is restartable: `scion start` clears it and runs a fresh session. A clean exit goes to `stopped` instead.

### Crashed
A value in the activity enum referring to an agent whose process exited non-zero. Note that a real crash now surfaces as the `error` *phase* (with the activity cleared and the detail in the agent's message), not as a `crashed` activity.

### Stalled
A platform-set activity for an agent whose heartbeat is still arriving (the process is alive) but that has produced no activity events within the stall threshold (default 5 minutes). Indicates a hung agent. Agents that have declared themselves `blocked` are excluded.

### Auto-Suspend
A Hub behavior that automatically suspends an agent which has remained `stalled` past a grace period (~10 minutes of inactivity), reclaiming its container. The agent resumes automatically on the next message, provided its harness supports session resume and the container is still alive.