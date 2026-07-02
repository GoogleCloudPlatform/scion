# Scion Glossary

Scion is a container-based orchestration platform for running multiple LLM "deep agents" concurrently, each isolated in its own container, workspace, and credentials. This document fixes the preferred term for each domain concept so that code, docs, UI, and prompts share one vocabulary.

> Two naming rules run throughout: the concept formerly called *grove* is now **project**, and bare **"broker"** is never used on its own — it is ambiguous across three distinct concepts (**Runtime Broker**, **Message Broker**, and the **Event Bus**), so it must always be qualified (see the disambiguation rule under [Hub & Hosted](#hub--hosted)). The codebase does not yet fully match either rule; the known gaps are tracked as GitHub issues.

## Orchestration

**Agent**:
An isolated worker: one LLM-plus-harness loop in its own container with its own identity, credentials, and workspace. The fundamental unit of execution in Scion.
_Avoid_: worker, bot, instance, process

**Sub-agent**:
An agent spawned by another agent; "sub" only from the orchestrating user's view, since it is a full agent in capability.
_Avoid_: helper, thread, worker thread

**Project**:
A namespace and collection of agents and configuration, represented by a `.scion` directory and usually one-to-one with a git repository.
_Avoid_: grove, group, repo, workspace

**Template**:
A harness-agnostic folder resource defining a generic agent — its system prompt, agent instructions, skills, services, and more — containing nothing specific to any one harness. A default harness-config may optionally be named, but is not required.
_Avoid_: role, blueprint, profile, config
_See also_: Harness-config (its harness-specific counterpart), Skill

**Harness**:
The external, vendor-supplied agent software that Scion drives, such as Claude Code, Gemini CLI, Codex, or OpenCode. Provided outside Scion; Scion only configures and runs it.
_Avoid_: model, backend, driver, tool
_See also_: Harness-config

**Harness-config**:
A named, reusable, harness-specific resource that configures a particular harness — which harness, plus its image, auth, secrets, model settings, and skills. The harness-specific counterpart to the (harness-agnostic) **Template**, and extensible the same way via a container-script provisioner rather than compiled-in logic.
_Avoid_: harness, harness adapter, integration, plugin
_See also_: Template, Harness, Container-script provisioner

**Container-script provisioner**:
The script-based provisioning model (`provisioner.type: container-script`) by which a harness-config extends agent setup with a container-side `provision.py`, making harness provisioning extensible — as opposed to a compiled-in (built-in) provisioner.
_Avoid_: built-in provisioner, plugin provisioner, install script, provision hook

**Skill**:
A reusable, harness-agnostic instruction snippet contributed by a template and mounted into the harness's skills directory at provisioning. Follows the open [Agent Skills](https://agentskills.io/home) convention.
_Avoid_: prompt snippet, macro, plugin
_See also_: Template, Plugin

**Plugin**:
An out-of-process extension built on `hashicorp/go-plugin` (gRPC) that supplies a Message Broker implementation without modifying Scion core. Harness implementations are *not* offered as plugins; additional plugin types may be added in future.
_Avoid_: extension, addon, module, skill
_See also_: Broker plugin, Message Broker

**sciontool**:
The helper utility injected into every agent container for status reporting, metadata access, and task management.
_Avoid_: agent tool, scion-tool

## Runtime & Workspace

**Runtime**:
The container technology that executes an agent's container: Docker, Podman, Apple Container, or Kubernetes.
_Avoid_: backend, engine, executor, environment
_See also_: Runtime Broker, Profile

**Workspace**:
The working directory mounted into a single agent's container at `/workspace`. How it is provisioned across a project's agents is set by the project's **workspace sharing mode**.
_Avoid_: project, repo, mount

**Workspace sharing mode**:
How a project's workspace is provisioned across its agents — one universal set of modes intended for both local and Hub-managed projects: **Shared-plain**, **Worktree-per-agent**, and **Clone-per-agent**.
_Avoid_: workspace mode, isolation mode

**Shared-plain**:
A workspace sharing mode where one workspace directory is mounted into every agent with no per-agent isolation — the model used for plain (non-git) projects.
_Avoid_: shared mount, plain workspace

**Worktree-per-agent**:
A workspace sharing mode where each agent gets its own git worktree over a shared checkout, isolating working trees while sharing one clone's history. Supported in local mode today; not yet on Hub-managed projects.
_Avoid_: worktree mode, shared checkout

**Clone-per-agent**:
A workspace sharing mode where each agent gets its own full git clone of the repository.
_Avoid_: clone mode, per-agent clone

**Shared directory**:
A persistent, mutable volume shared by the agents within one project.
_Avoid_: shared mount, shared volume, common dir

**Agent home**:
The directory mounted as the container user's home folder, holding that agent's unique config and history.
_Avoid_: home mount, config dir

## Hub & Hosted

**Hub**:
The control plane of Scion — it owns identity, auth, project registration, and state, exposes the APIs and notifications that agents and users interact with, and dispatches commands to runtime brokers. Present in both workstation and hosted mode, not only hosted.
_Avoid_: server, master, coordinator

**Runtime Broker**:
A service that manages the lifecycle of containerized agents on behalf of the Hub — provisioning workspaces, hydrating templates, and delegating container operations to a pluggable runtime. Brokers vary along two dimensions: whether they require host-level access to a container runtime (*node-bound* vs. *proxy*) and whether they run as a standalone process or are *embedded* in the Hub. Always write in full; "broker" alone is forbidden because it collides with Message Broker. See also *Managed Agent*, which bypasses the broker entirely via a direct cloud API integration.
_Avoid_: broker, node, runner, worker
_See also_: Node-Bound Broker, Proxy Broker, Embedded Broker, Hosted Broker, Managed Agent, Runtime, Profile, Message Broker (distinct concept, same word)

**Node-Bound Broker**:
A Runtime Broker that runs on the same compute node as the containers it manages. Required for runtimes that need direct host access, such as Docker (via the daemon socket) or Apple Container (via the Virtualization framework). A node-bound broker is inherently stateful — its identity is tied to the machine it runs on, and it connects to the Hub via a persistent control channel and periodic heartbeats.
_Avoid_: local broker, host broker
_See also_: Runtime Broker, Proxy Broker, Embedded Broker

**Proxy Broker**:
A stateless Runtime Broker that delegates container operations to an API-mediated service such as Cloud Run or Kubernetes. Because it communicates over a network API rather than a local daemon, a proxy broker is not tied to any particular compute node and can be replicated for high availability.
_Avoid_: remote broker, API broker, cloud broker
_See also_: Runtime Broker, Node-Bound Broker, Hosted Broker

**Embedded Broker**:
A Runtime Broker running inside the same process as the Hub server, eliminating control-channel overhead. Both node-bound and proxy brokers can be embedded. Contrast with a standalone broker, which runs as its own process and connects to the Hub remotely.
_Avoid_: inline broker, in-process broker
_See also_: Runtime Broker, Combo server, Hosted Broker

**Hosted Broker**:
An embedded proxy broker that serves as the platform's default compute backend. Because it is both stateless and co-located with the Hub, it can be replicated alongside Hub instances for high availability without broker-specific scheduling. Agents dispatched without an explicit broker target are routed to it automatically.
_Avoid_: default broker, platform broker, cloud broker
_See also_: Runtime Broker, Proxy Broker, Embedded Broker

**Managed Agent**:
An agent whose lifecycle is managed directly by the Hub via a cloud provider API (e.g. Google Managed Agents), bypassing the Runtime Broker layer entirely. Managed agents share the same `Manager` interface and agent-label system as containerized agents, but have no container, no workspace mount, and no broker involvement. The choice between a managed agent and a brokered agent is a deployment-time decision controlled by a broker profile, not a property of the agent template.
_Avoid_: cloud agent, API agent, serverless agent
_See also_: Runtime Broker, Profile

**Profile**:
A named bundle of runtime broker settings selected as a unit — a runtime plus its execution settings (env, volumes, resources), default harness-config and template, image registry, secrets, and harness overrides. A runtime-broker-scoped concept; long form **Runtime Broker Profile**.
_Avoid_: environment, runtime config, preset, runtime profile
_See also_: Runtime Broker, Runtime

**Message Broker**:
The pluggable system that brokers messages between Scion actors (agents and users) and messaging surfaces — built-in brokers such as the web UI Messages view, and broker plugins to external systems like Telegram and Google Chat (Discord and Slack planned). Backs the `scion message` command. Always write in full; "broker" alone is forbidden because it collides with Runtime Broker.
_Avoid_: broker, message bus, queue, pub/sub
_See also_: Broker plugin, Built-in broker, Plugin, Event Bus (distinct), Runtime Broker (distinct, same word)

**Broker plugin**:
A Message Broker implementation for a specific external messaging system (e.g. Telegram, Google Chat), loaded through the broker plugin interface (`PluginTypeBroker`).
_Avoid_: connector, bridge, adapter
_See also_: Message Broker, Built-in broker, Plugin

**Built-in broker**:
A Message Broker implementation shipped with Scion rather than loaded as a plugin — for example the broker that surfaces messages in the web UI's Messages view.
_Avoid_: native broker, internal broker, default broker
_See also_: Message Broker, Broker plugin

> **Disambiguation rule:** Never use bare "broker" in prose, comments, docs, or new identifiers — always qualify it as **Runtime Broker** or **Message Broker**. Note that `pkg/broker` (NATS-style pub/sub) is *not* the Message Broker; it underpins the **Event Bus**. Existing bare usages in code are tracked for cleanup as GitHub issues.

**Event Bus**:
The NATS-style pub/sub system (`pkg/broker`) that brokers and dispatches real-time change events to live web views via server-sent events, supporting the move to a more stateless Hub. Distinct from the Message Broker; currently a latent capability.
_Avoid_: message broker, broker, change feed, live sync, event stream
_See also_: Message Broker (distinct concept)

**Hub-managed project**:
A project whose workspace is created and managed by Scion in the hub-controlled part of the broker filesystem (`~/.scion/projects/<slug>/`), shared across the project's agents — as opposed to a Linked project that points at a pre-existing path. May be plain (no git) or git-backed; git-backed hub-managed projects may share a remote with other projects. The workspace itself is the **Hub-managed workspace**.
_Avoid_: hub-native, hub-native project, hub workspace, hub-project, hosted project, cloud project

**Linked project**:
A project whose workspace is a pre-existing path on a broker machine, linked to a Hub for cross-broker visibility — as opposed to a Hub-managed project. May be plain or git-backed.
_Avoid_: local project, imported project, registered project

**Server**:
The `scion server` command group, and the single combined process it manages — one or more server components run together as a background daemon (or with `--foreground`) via `start`/`stop`/`restart`/`status`.
_Avoid_: daemon, service, backend

**Server component**:
One of the roles a server process can run — the Hub API, the Runtime Broker API, or the Web dashboard. A single server process may run any combination of these.
_Avoid_: service, module, role

**Combo server**:
A server process running both the Hub and Runtime Broker components together (the default in workstation mode).
_Avoid_: hub-broker, all-in-one, standalone, monolith

**Secret**:
A credential made available to an agent at runtime (e.g. API keys, tokens). A harness-config's `secrets` field *declares* which secrets an agent needs; the **Secret Backend** — a pluggable store (local SQLite for development, GCP Secret Manager in production, selected via `SCION_SERVER_SECRETS_BACKEND`) — *stores and resolves* them, scoped by user, project, runtime broker, or hub, and injects them into the container. Also holds the Hub's signing keys.
_Avoid_: credential, vault, secret store, env secret
_See also_: Harness-config, Profile

## Users & Access

**Group**:
A named collection of Hub users (and nested groups) used by the Hub permissions system to assign access. This is the primary meaning of "group" in Scion.
_Avoid_: team, org, role
_See also_: Message Group (different concept — message recipients, not users)

## Messaging

**Message Group**:
A set of recipients addressed by a single send, correlated by a shared `group_id`, as opposed to a direct message to one recipient or a broadcast to all agents in a project.
_Avoid_: group, set, group chat, room, thread
_See also_: Group (different concept — hub users, not recipients)

**Notification**:
An event delivered when an agent reaches a tracked trigger activity (e.g. `completed`, `waiting_for_input`, `limits_exceeded`). Recipients register a **Subscription** — scoped to a single agent or to a whole project, naming which trigger activities fire it and whether an agent or a user receives it. Backs `scion notifications` and the `--notify` flag on `scion message`.
_Avoid_: alert, event (for the notification), watch (for the subscription)
_See also_: Activity (notifications fire on activity values)

## Identity & State

**Project ID**:
A project's unique identifier — always a randomly generated UUID. A git remote is associated metadata, not identity, so multiple projects may share the same remote by design.
_Avoid_: grove ID, project key, repo ID, slug

**Ancestry chain**:
The tracked `root → parent → child` relationship between agents that governs transitive access control.
_Avoid_: lineage, hierarchy, agent tree, family

**Phase**:
The infrastructure lifecycle stage of an agent container, from `created` through `running` to `stopped` or `error`.
_Avoid_: status, stage, lifecycle state
_See also_: Activity (what the agent is *doing*, vs. its container stage)

**Activity**:
What a running agent is currently doing, such as `thinking`, `executing`, `waiting_for_input`, or `blocked`. Distinct from phase.
_Avoid_: status, state, mode
_See also_: Phase (the container stage), Blocked (a specific activity value)

**Blocked**:
The activity an agent assigns to itself when intentionally waiting for an expected event, so it is not mistaken for stalled.
_Avoid_: stalled, stuck, idle, waiting
_See also_: Activity (Blocked is one of its values)

## Modes

The run modes form a spine of increasing infrastructure — **Local → Workstation → Single-node hosted → HA hosted**. Two independent dimensions separate them: the **availability tier** of the control plane (whether the Hub runs as a single instance on an embedded database, or is replicated across an external one), and **Tenancy** (whether it serves one user or many). Tenancy is orthogonal and only opens up once hosted; the availability tier is fixed by the Hub's database driver (`SCION_SERVER_DATABASE_DRIVER`: `sqlite` vs. `postgres`).

| Mode | Control plane | State & durability | Tenancy | Canonical use |
|------|---------------|--------------------|---------|----------------|
| **Local mode** | None (CLI only) | Local machine; git-worktree isolation | Single-user | Agents launched directly via the `scion` CLI, no server |
| **Workstation mode** | Combo server (Hub + Runtime Broker + Web) on loopback | Embedded SQLite on that machine | Single-user | The hosted experience locally, on your own machine |
| **Single-node hosted** | One networked Hub instance on a single node | Embedded SQLite, local/single-volume; non-HA | Single- or multi-user | A cheap, simple networked Hub — a single VM, or a single Cloud Run instance + SQLite |
| **HA hosted** | Hub replicated behind a load balancer | External managed DB (Postgres) + object storage; highly available | Single- or multi-user | A durable, always-on shared deployment — Cloud Run + Cloud SQL |

**Local mode**:
Running Scion with no server at all — agents launched directly via the `scion` CLI, with state on the local machine and isolation via git worktrees.
_Avoid_: solo mode, standalone mode, single-user mode, workstation mode

**Workstation mode**:
Running a single-tenant Scion server (Hub + Runtime Broker + Web combined) on your own machine, giving the hosted experience locally over loopback. A local server, not the no-server CLI workflow.
_Avoid_: local mode, local server, dev mode, single-user mode

**Hosted mode**:
The umbrella term for running against a networked Hub — reachable beyond a single machine — that coordinates state across users, projects, and runtime brokers. Spans two **availability tiers**, **Single-node hosted** and **HA hosted**, distinguished by control-plane durability and cost; the tier is fixed by the Hub's database driver (embedded `sqlite` vs. external `postgres`). Orthogonal to the tier is **Tenancy** (single- vs. multi-user).
_Avoid_: hub mode, cloud mode, distributed mode, production mode
_See also_: Single-node hosted, HA hosted, Tenancy, Workstation mode

**Single-node hosted**:
A hosted deployment whose control plane — the Hub — runs as a single instance on one compute node, keeping state in an embedded SQLite database on local or single-volume storage, with no external database. Non-HA: it accepts restart/redeploy downtime and single-volume durability in exchange for low cost and operational simplicity — there is no separate database to provision, secure, back up, or pay for. Realized as a single VM (e.g. the starter-hub scripts, `scripts/starter-hub/`) or a single Cloud Run instance backed by SQLite. "Single-node" scopes the control plane only — agents may run on other nodes. The `sqlite` driver pins the Hub to one instance (a single DB connection and in-memory lifecycle-hook deduplication).
_Avoid_: single-instance hosted, standalone hosted, lite hub, sqlite mode
_See also_: HA hosted, Hosted mode, Tenancy, Node-Bound Broker (broker-to-node binding — a different subject, same word "node")

**HA hosted**:
A hosted deployment whose control plane is replicated across multiple Hub instances behind a load balancer, backed by an external managed database (Cloud SQL Postgres) and object storage (GCS), with stateless proxy/hosted brokers. Highly available and durable — it survives node loss and redeploys without downtime — at the cost of running and paying for that external infrastructure. Realized by the Cloud Run deployment (`scripts/cloudrun/`: Cloud Run with min-instances ≥ 2 plus Cloud SQL). The `postgres` driver is what enables replication — durable cross-instance compare-and-set for lifecycle-hook deduplication, versus SQLite's single-instance in-memory approach.
_Avoid_: clustered hosted, distributed hosted, ha mode, production mode
_See also_: Single-node hosted, Hosted mode, Tenancy, Proxy Broker, Hosted Broker

**Tenancy**:
Whether a deployment serves a single identity or many — orthogonal to the availability tier. **Single-user**: one principal, with simple auth (a workstation dev token, or one OAuth identity). **Multi-user**: many principals authenticated through an OAuth identity provider (Google or GitHub), with Hub **Groups** and access policies governing who can see and act on what. Local and Workstation modes are single-user by construction; either hosted tier can be single- or multi-user.
_Avoid_: multi-tenancy (for org isolation), single-tenant / multi-tenant (prefer single-/multi-user)
_See also_: Group, Hosted mode, Single-node hosted, HA hosted

## Operations

**Attach**:
Connecting an interactive terminal to a running agent's tmux session for human-in-the-loop interaction; the agent keeps running once detached.
_Avoid_: connect, join, ssh in

**Dispatch**:
The Hub handing an agent lifecycle command to the appropriate runtime broker for execution.
_Avoid_: schedule, route, assign, delegate

**Schedule**:
A time-based trigger that fires an action — sending a message or dispatching (starting) an agent from a template — either once (a one-shot *scheduled event*, via `--at <time>` or `--in <duration>`) or repeatedly (a recurring *schedule* on a 5-field cron expression). Backs `scion schedule` and the `--at`/`--in` flags on `scion message`.
_Avoid_: cron job (recurring only), scheduled message (too narrow), reminder, timer
_See also_: Dispatch

## Observability

Scion produces two distinct families of metrics. They serve different audiences, use different prefixes, and flow through different pipelines — but both export to the same Cloud Monitoring backend.

**Infrastructure metrics**:
Operational health metrics for Scion as a system — the Hub process, its database connections, dispatch pipeline, broker authentication, and GCP token minting. These answer "is Scion itself healthy?" and are consumed by platform operators. Prefixes: `scion.hub.*`, `scion.db.*`, `scion.dispatch.*`. Produced by the Hub process; exported directly to Cloud Monitoring via an OTel MeterProvider with a GCP exporter.
_Avoid_: system metrics, platform metrics, server metrics
_See also_: Agent metrics (the other family)

**Agent metrics**:
Telemetry about what agents and their harnesses are doing — token usage, tool calls, model API latency, session counts, and cost signals. These answer "what are the agents doing and what do they cost?" and are consumed by users and project owners. Prefixes: `gen_ai.*`, `agent.*` (following OpenTelemetry Generative AI semantic conventions). Produced inside agent containers by the harness and sciontool; exported to Cloud Monitoring via the telemetry pipeline (`pkg/sciontool/telemetry`).
_Avoid_: harness metrics, user metrics, LLM metrics
_See also_: Infrastructure metrics (the other family), Telemetry pipeline

**Telemetry pipeline**:
The in-container OTLP receiver and forwarding pipeline (`pkg/sciontool/telemetry`) that collects traces, metrics, and logs from the harness and exports them to a cloud backend (GCP Cloud Monitoring, Cloud Trace, Cloud Logging). Requires the `scion-telemetry-gcp-credentials` secret for cloud export; runs in local-only mode without it.
_Avoid_: metrics pipeline, collector, OTel collector
_See also_: Agent metrics

## Potential Future Additions

Terms that recur in the codebase and may warrant canonical entries, but are **not yet defined** here. Listed so they aren't lost; promote to full entries (verified against the code) as the glossary matures.

- **Task** — the unit tracked by sciontool's task management; referenced operationally but not yet given a canonical definition.
- **Capability** — a declarative harness feature flag (e.g. `max_turns`, `session_resume`, `api_key`, `stdio`) describing what a harness supports.
- **Resource Spec** — per-agent CPU/memory/disk request and limit (Kubernetes-style), applied per agent or via a Profile.
- **Label** — `scion.*` key/value metadata attached to agents and containers for identification and filtering.
- **Access Policy / scope** — the Hub authorization model: allow/deny rules at `hub`, `project`, or `resource` scope (the mechanism that grants a Group its access).
- **Image registry** — the container image registry agents pull from (a Profile field).
- **Session / tmux session** — the per-agent tmux session an Attach connects to.
- **Service** — a background/sidecar process declared by a Template alongside the agent (currently referenced in the Template entry but undefined).
- **Content Hash / Manifest** — SHA-based identity for template and harness-config content, used for cache validation and transfer between Hub and runtime brokers.
- **Transfer** — the signed-URL mechanism that moves templates and harness-configs between the Hub and runtime brokers.
- **Service Account** — a GCP identity an agent can assume for Google Cloud auth, registered with the Hub.
- **Webhook / GitHub App** — external event triggers (e.g. GitHub webhooks) that can dispatch agents.
- **OAuth provider** — a configurable identity provider (Google, GitHub) for CLI and web authentication.
