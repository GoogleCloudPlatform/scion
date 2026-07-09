---
title: Glossary
description: Standardized terminology for the Scion project.
---

This glossary defines key terms used throughout the Scion documentation and ecosystem. It is a projection of the project's canonical [`GLOSSARY.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/GLOSSARY.md); when the two disagree, the root glossary wins.

:::note[Two naming rules run throughout]
- The concept formerly called *grove* is now **Project**.
- Bare **"broker"** is never used on its own — it is ambiguous across **Runtime Broker**, **Message Broker**, and the **Event Bus**, so it must always be qualified.
:::

## Orchestration

### Agent
An isolated worker: one LLM-plus-harness loop in its own container with its own identity, credentials, and workspace. The fundamental unit of execution in Scion.

### Sub-agent
An agent spawned by another agent; "sub" only from the orchestrating user's view, since it is a full agent in capability.

### Project
A namespace and collection of agents and configuration, represented by a `.scion` directory and usually one-to-one with a git repository. Not the same as a **Group**.

### Template
A harness-agnostic folder resource defining a generic agent — its system prompt, agent instructions, skills, services, and more — containing nothing specific to any one harness. The harness-agnostic counterpart to a **Harness-config**.

### Harness
The external, vendor-supplied agent software that Scion drives, such as Claude Code, Gemini CLI, Codex, or OpenCode. Provided outside Scion; Scion only configures and runs it. A harness is **not** a plugin.

### Harness-config
A named, reusable, harness-specific resource that configures a particular harness — which harness, plus its image, auth, secrets, model settings, and skills. The harness-specific counterpart to the (harness-agnostic) **Template**.

### Container-script provisioner
The script-based provisioning model (`provisioner.type: container-script`) by which a harness-config extends agent setup with a container-side `provision.py`, making harness provisioning extensible — as opposed to a compiled-in (built-in) provisioner.

### Skill
A reusable, harness-agnostic instruction snippet contributed by a template and mounted into the harness's skills directory at provisioning. Follows the open [Agent Skills](https://agentskills.io/home) convention.

### Skill Bank
The Hub-backed system for publishing, versioning, resolving, caching, and federating **Skills**. Encompasses the `scion skills` CLI, the Hub **Skill Registry**, semver + content-hash resolution, and the skills web UI — as distinct from a plain template-mounted skill file.

### Skill Registry
The Hub-side store and resolver for the **Skill Bank**: it holds published skills and their immutable versions, resolves **Skill Reference URIs**, and can federate resolution to external registries (including GitHub and GCP Vertex AI sources).

### Skill Reference URI
The addressing scheme for a **Skill**: a bare name or a `skill://<registry>/<scope>/<scopeId>/<name>@<version>` URI (with `gh://` and `gcp-skill://` variants for federated sources). Versions may be exact, a semver range, `latest`, or a `sha256:` content hash.

### Plugin
An out-of-process extension built on `hashicorp/go-plugin` (gRPC) that supplies a **Message Broker** implementation without modifying Scion core. **Harness implementations are *not* offered as plugins**; additional plugin types may be added in the future.

### sciontool
The helper utility injected into every agent container for status reporting, metadata access, and task management.

## Runtime & Workspace

### Runtime
The container technology that executes an agent's container: Docker, Podman, Apple Container, or Kubernetes.

### Workspace
The working directory mounted into a single agent's container at `/workspace`. How it is provisioned across a project's agents is set by the project's **workspace sharing mode**.

### Workspace sharing mode
How a project's workspace is provisioned across its agents — one universal set of three modes intended for both local and Hub-managed projects: **Shared-plain**, **Worktree-per-agent**, and **Clone-per-agent**.

### Shared-plain
A workspace sharing mode where one workspace directory is mounted into every agent with no per-agent isolation — the model used for plain (non-git) projects.

### Worktree-per-agent
A workspace sharing mode where each agent gets its own git worktree over a shared checkout, isolating working trees while sharing one clone's history. Supported in local mode today; not yet on Hub-managed projects.

### Clone-per-agent
A workspace sharing mode where each agent gets its own full git clone of the repository.

### Shared directory
A persistent, mutable volume shared by the agents within one project. Backed by host filesystem directories (local) or Kubernetes PersistentVolumeClaims (K8s).

### Agent home
The directory mounted as the container user's home folder, holding that agent's unique config and history.

## Hub & Hosted

### Hub
The control plane of Scion — it owns identity, auth, project registration, and state, exposes the APIs and notifications that agents and users interact with, and dispatches commands to runtime brokers. Present in both workstation and hosted mode, not only hosted.

### Runtime Broker
A service that manages the lifecycle of containerized agents on behalf of the Hub — provisioning workspaces, hydrating templates, and delegating container operations to a pluggable runtime. It is not itself a compute node. Brokers vary along two dimensions: whether they require host-level access to a container runtime (*node-bound* vs. *proxy*) and whether they run as a standalone process or are *embedded* in the Hub. See also *Managed Agent*, which bypasses the broker entirely via a direct cloud API integration.

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

### Profile
A named bundle of runtime broker settings selected as a unit — a runtime plus its execution settings (env, volumes, resources), default harness-config and template, image registry, secrets, and harness overrides. A runtime-broker-scoped concept; long form **Runtime Broker Profile**.

### Message Broker
The pluggable system that brokers messages between Scion actors (agents and users) and messaging surfaces — built-in brokers such as the web UI Messages view, and broker plugins to external systems like Telegram and Google Chat (Discord and Slack planned). Backs the `scion message` command. Distinct from the Runtime Broker despite the shared word.

### Broker plugin
A Message Broker implementation for a specific external messaging system (e.g. Telegram, Google Chat), loaded through the broker plugin interface (`PluginTypeBroker`).

### Built-in broker
A Message Broker implementation shipped with Scion rather than loaded as a plugin — for example the broker that surfaces messages in the web UI's Messages view.

### Event Bus
The NATS-style pub/sub system (`pkg/broker`) that brokers and dispatches real-time change events to live web views via server-sent events, supporting the move to a more stateless Hub. Distinct from the Message Broker; currently a latent capability.

### Hub-managed project
A project whose workspace is created and managed by Scion in the hub-controlled part of the broker filesystem (`~/.scion/projects/<slug>/`), shared across the project's agents — as opposed to a Linked project that points at a pre-existing path. May be plain (no git) or git-backed.

### Linked project
A project whose workspace is a pre-existing path on a broker machine, linked to a Hub for cross-broker visibility — as opposed to a Hub-managed project. May be plain or git-backed.

### Server
The `scion server` command group, and the single combined process it manages — one or more server components run together as a background daemon (or with `--foreground`) via `start`/`stop`/`restart`/`status`.

### Server component
One of the roles a server process can run — the Hub API, the Runtime Broker API, or the Web dashboard. A single server process may run any combination of these.

### Combo server
A server process running both the Hub and Runtime Broker components together (the default in workstation mode).

### Secret
A credential made available to an agent at runtime (e.g. API keys, tokens). A harness-config's `secrets` field *declares* which secrets an agent needs; the **Secret Backend** — a pluggable store (local SQLite for development, GCP Secret Manager in production, selected via `SCION_SERVER_SECRETS_BACKEND`) — *stores and resolves* them, scoped by user, project, runtime broker, or hub, and injects them into the container.

## Users & Access

### Group
A named collection of Hub users (and nested groups) used by the Hub permissions system to assign access. This is the primary meaning of "group" in Scion. Distinct from a **Message Group** (a set of message recipients) and from a **Project**.

## Messaging

### Message Group
A set of recipients addressed by a single send, correlated by a shared `group_id`, as opposed to a direct message to one recipient or a broadcast to all agents in a project. Distinct from **Group** (Hub users).

### Notification
An event delivered when an agent reaches a tracked trigger activity (e.g. `completed`, `waiting_for_input`, `limits_exceeded`). Recipients register a **Subscription** — scoped to a single agent or to a whole project, naming which trigger activities fire it and whether an agent or a user receives it. Backs `scion notifications` and the `--notify` flag on `scion message`.

## Identity & State

### Project ID
A project's unique identifier — always a randomly generated UUID. A git remote is associated metadata, not identity, so multiple projects may share the same remote by design.

### Ancestry chain
The tracked `root → parent → child` relationship between agents that governs transitive access control.

### Phase
The infrastructure lifecycle stage of an agent container: `created`, `provisioning`, `cloning`, `starting`, `running`, `stopping`, `stopped`, `suspended`, or `error`.

### Activity
What a running agent is currently doing within the `running` phase, such as `thinking`, `executing`, `waiting_for_input`, `blocked`, `completed`, `limits_exceeded`, `stalled`, or `offline`. Distinct from phase.

### Blocked
The activity an agent assigns to itself when intentionally waiting for an expected event (such as a child agent completing), so it is not mistaken for stalled.

### Suspend / Resume
**Suspend** tears down an agent's container while recording the intent to resume it later (phase `suspended`). **Resume** brings the agent back and *continues* its previous harness conversation (e.g. `--continue` for Claude Code, `--resume` for Gemini CLI) rather than starting fresh. Distinct from `stop`/`start`, which always begin a new session. Requires a harness that supports session resume.

### Error (crash)
The phase an agent enters when its process or container exits non-zero (a crash, OOM, or `SIGKILL`), carrying a message like `Agent crashed with exit code N`. The `error` phase is restartable: `scion start` clears it and runs a fresh session. A clean exit goes to `stopped` instead. The `error` phase also covers setup failures (e.g. a failed git clone) that happen before an agent reaches `running`.

### Stalled
A platform-set activity for an agent whose heartbeat is still arriving (the process is alive) but that has produced no activity events within the stall threshold (default 5 minutes). Indicates a hung agent. Agents that have declared themselves `blocked` are excluded.

### Auto-Suspend
A Hub behavior that automatically suspends an agent which has remained `stalled` past a grace period, reclaiming its container. The agent resumes automatically on the next message, provided its harness supports session resume and the container is still alive.

## Modes

The run modes form a spine of increasing infrastructure — **Local → Workstation → Single-node hosted → HA hosted**. Two independent dimensions separate them: the **availability tier** of the control plane (whether the Hub runs as a single instance on an embedded database, or is replicated across an external one), and **Tenancy** (whether it serves one user or many). Tenancy is orthogonal and only opens up once hosted; the availability tier is fixed by the Hub's database driver (`SCION_SERVER_DATABASE_DRIVER`: `sqlite` vs. `postgres`).

| Mode | Control plane | State & durability | Tenancy | Canonical use |
|------|---------------|--------------------|---------|----------------|
| **Local mode** | None (CLI only) | Local machine; git-worktree isolation | Single-user | Agents launched directly via the `scion` CLI, no server |
| **Workstation mode** | Combo server (Hub + Runtime Broker + Web) on loopback | Embedded SQLite on that machine | Single-user | The hosted experience locally, on your own machine |
| **Single-node hosted** | One networked Hub instance on a single node | Embedded SQLite, single-volume; non-HA | Single- or multi-user | A cheap, simple networked Hub (single VM, or single Cloud Run instance + SQLite) |
| **HA hosted** | Hub replicated behind a load balancer | External managed DB (Postgres) + object storage; highly available | Single- or multi-user | A durable, always-on shared deployment (Cloud Run + Cloud SQL) |

### Local mode
Running Scion with no server at all — agents launched directly via the `scion` CLI, with state on the local machine and isolation via git worktrees.

### Workstation mode
Running a single-tenant Scion server (Hub + Runtime Broker + Web combined) on your own machine, giving the hosted experience locally over loopback. A local server, not the no-server CLI workflow.

### Hosted mode
The umbrella term for running against a networked Hub — reachable beyond a single machine — that coordinates state across users, projects, and runtime brokers. Spans two **availability tiers**, **Single-node hosted** and **HA hosted**, distinguished by control-plane durability and cost; the tier is fixed by the Hub's database driver (embedded `sqlite` vs. external `postgres`). Orthogonal to the tier is **Tenancy** (single- vs. multi-user).

### Single-node hosted
A hosted deployment whose control plane — the Hub — runs as a single instance on one compute node, keeping state in an embedded SQLite database, with no external database. Non-HA: it accepts restart/redeploy downtime and single-volume durability in exchange for low cost and operational simplicity. Realized as a single VM (e.g. the starter-hub scripts) or a single Cloud Run instance backed by SQLite. "Single-node" scopes the control plane only — agents may run on other nodes.

### HA hosted
A hosted deployment whose control plane is replicated across multiple Hub instances behind a load balancer, backed by an external managed database (Cloud SQL Postgres) and object storage (GCS), with stateless proxy/hosted brokers. Highly available and durable — it survives node loss and redeploys without downtime — at the cost of running and paying for that external infrastructure. Realized by the Cloud Run deployment (Cloud Run with min-instances ≥ 2 plus Cloud SQL).

### Availability tier
Whether the Hub's control plane runs as a single instance on an embedded database or is replicated on an external one. Fixed by `SCION_SERVER_DATABASE_DRIVER` (`sqlite` = Single-node, `postgres` = HA). One of the two dimensions that separate the hosted modes.

### Tenancy
Whether a deployment serves a single identity or many — orthogonal to the availability tier. **Single-user**: one principal, with simple auth (a workstation dev token, or one OAuth identity). **Multi-user**: many principals authenticated through an OAuth identity provider (Google or GitHub), with Hub **Groups** and access policies governing who can see and act on what. Local and Workstation modes are single-user by construction; either hosted tier can be single- or multi-user.

## Operations

### Attach
Connecting an interactive terminal to a running agent's tmux session for human-in-the-loop interaction; the agent keeps running once detached.

### Dispatch
The Hub handing an agent lifecycle command to the appropriate runtime broker for execution.

### Schedule
A time-based trigger that fires an action — sending a message or dispatching (starting) an agent from a template — either once (a one-shot *scheduled event*, via `--at <time>` or `--in <duration>`) or repeatedly (a recurring *schedule* on a 5-field cron expression). Backs `scion schedule` and the `--at`/`--in` flags on `scion message`.

## Observability

Scion produces two distinct families of metrics. They serve different audiences, use different prefixes, and flow through different pipelines — but both export to the same Cloud Monitoring backend.

### Infrastructure metrics
Operational health metrics for Scion as a system — the Hub process, its database connections, dispatch pipeline, broker authentication, and GCP token minting. They answer "is Scion itself healthy?" and are consumed by platform operators. Prefixes: `scion.hub.*`, `scion.db.*`, `scion.dispatch.*`.

### Agent metrics
Telemetry about what agents and their harnesses are doing — token usage, tool calls, model API latency, session counts, and cost signals. They answer "what are the agents doing and what do they cost?" and are consumed by users and project owners. Prefixes: `gen_ai.*`, `agent.*` (following OpenTelemetry Generative AI semantic conventions).

### Telemetry pipeline
The in-container OTLP receiver and forwarding pipeline (`pkg/sciontool/telemetry`) that collects traces, metrics, and logs from the harness and exports them to a cloud backend (GCP Cloud Monitoring, Cloud Trace, Cloud Logging). Requires the `scion-telemetry-gcp-credentials` secret for cloud export; runs in local-only mode without it.
