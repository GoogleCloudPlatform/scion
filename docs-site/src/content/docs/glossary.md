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
A service that manages the lifecycle of containerized agents on behalf of the Hub — provisioning workspaces, hydrating templates, and delegating container operations to a pluggable runtime. Brokers vary along two dimensions: whether they require host-level access to a container runtime (*node-bound* vs. *proxy*) and whether they run as a standalone process or are *embedded* in the Hub. See also *Managed Agent*, which bypasses the broker entirely via a direct cloud API integration.

### Node-Bound Broker
A Runtime Broker that runs on the same compute node as the containers it manages. Required for runtimes that need direct host access, such as Docker (via the daemon socket) or Apple Container (via the Virtualization framework). A node-bound broker is inherently stateful — its identity is tied to the machine it runs on, and it connects to the Hub via a persistent control channel and periodic heartbeats.

### Proxy Broker
A stateless Runtime Broker that delegates container operations to an API-mediated service such as Cloud Run or Kubernetes. Because it communicates over a network API rather than a local daemon, a proxy broker is not tied to any particular compute node and can be replicated for high availability.

### Embedded Broker
A Runtime Broker running inside the same process as the Hub server, eliminating control-channel overhead. Both node-bound and proxy brokers can be embedded. Contrast with a standalone broker, which runs as its own process and connects to the Hub remotely.

### Hosted Broker
An embedded proxy broker that serves as the platform's default compute backend. Because it is both stateless and co-located with the Hub, it can be replicated alongside Hub instances for high availability without broker-specific scheduling. Agents dispatched without an explicit broker target are routed to it automatically.

### Managed Agent
An agent whose lifecycle is managed directly by the Hub via a cloud provider API (e.g. Google Managed Agents), bypassing the Runtime Broker layer entirely. Managed agents share the same `Manager` interface and agent-label system as containerized agents, but have no container, no workspace mount, and no broker involvement. The choice between a managed agent and a brokered agent is a deployment-time decision controlled by a broker profile, not a property of the agent template.

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