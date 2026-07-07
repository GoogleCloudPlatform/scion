---
title: Scion Overview
description: What Scion is, how it is put together, and where to go next.
---

Scion is a container-based orchestration platform for running multiple LLM **deep agents**
concurrently — each isolated in its own container, workspace, and credentials. It lets you run
groups of specialized agents with distinct identities to parallelize work such as research,
coding, auditing, and testing.

The same agents and the same `scion` CLI work whether you are running alone on a laptop with no
server, or operating a durable, always-on platform for a team. What changes is **how much
infrastructure sits behind the agents** — captured by Scion's [run modes](/scion/choosing-a-mode/).

## Choosing a mode

Scion's run modes form a **spine of increasing infrastructure**:

**Local → Workstation → Single-node hosted → HA hosted**

- **Local** — no server; agents launched directly through the CLI with git-worktree isolation.
- **Workstation** — a single-tenant combo server (Hub + Runtime Broker + Web) on your own machine,
  giving the hosted experience locally.
- **Single-node hosted** — a cheap, networked Hub on one node with an embedded SQLite database.
- **HA hosted** — a Hub replicated behind a load balancer, backed by external Postgres and object
  storage, for durable always-on operation.

Start with **[Choosing a Mode](/scion/choosing-a-mode/)** to pick the one that fits your use case.

## Architecture

Scion separates the **control plane** (the Hub) from the **execution layer** (runtime brokers and
the agent containers they manage):

- **`scion` CLI** — a host-side CLI that drives the lifecycle of agents and manages templates
  (`scion templates`), projects, and configuration.
- **Hub** — the control plane. It owns identity, auth, project registration, and state, exposes
  the APIs and notifications that agents and users interact with, and **dispatches** commands to
  runtime brokers. Present in both Workstation and hosted modes.
- **Runtime Broker** — a *service* (not a machine) that manages the lifecycle of containerized
  agents on behalf of the Hub: provisioning workspaces, hydrating templates, and delegating
  container operations to a pluggable runtime (Docker, Podman, Apple Container, Kubernetes).
- **Agents** — isolated containers running vendor-supplied harness software such as Claude Code,
  Gemini CLI, or OpenAI Codex.

In **Local mode** there is no server: the CLI provisions containers directly. In **Workstation
mode** the Hub, a Runtime Broker, and the Web dashboard run together as a *combo server* on
loopback. In **hosted** modes the Hub is networked and dispatches to standalone or embedded
brokers.

```d2
direction: right
User -> Scion CLI: start agent
Scion CLI -> Hub: dispatch
Hub -> Runtime Broker: provision + run container
Runtime Broker -> Agent Container: execute task
Agent Container -> Hub: status + activity
```

Some agents skip the broker layer entirely: a [**Managed Agent**](/scion/hosted/single-node/managed-agents/)
is run directly by the Hub through a cloud provider API, with no container or workspace.

## Configuration

Scion uses a layered configuration system based on **Profiles**, **Runtimes**, and
**Harnesses**, so you can define different environments (e.g. local Docker vs. remote Kubernetes)
and switch between them.

- **Global settings**: `~/.scion/settings.yaml` (machine-wide defaults)
- **Project settings**: `.scion/settings.yaml` (per-project overrides)

Scion accepts settings in either **YAML or JSON**. `scion init` writes `settings.yaml`, and YAML
is preferred when multiple files are present (the loader looks for `settings.yaml`, then
`settings.yml`, then `settings.json`). Both validate against the
[settings JSON schema](https://github.com/GoogleCloudPlatform/scion/blob/main/pkg/config/schemas/settings-v1.schema.json).

For details, see the [Configuration Overview](/scion/reference/scion-config-reference/),
[Orchestrator Settings Reference](/scion/reference/orchestrator-settings/), and
[Agent Configuration Reference](/scion/reference/agent-config/). For the agent tools Scion drives,
see [Supported Harnesses](/scion/supported-harnesses/).

## Getting started

1. **Install** — follow the [Installation Guide](/scion/getting-started/install/).
2. **Initialize** — run `scion init` in your project root to create a `.scion` marker.
3. **Start an agent** — `scion start <agent-name> "<task>"`.
4. **Interact** — `scion attach <agent-name>` to join its session, or `scion look <agent-name>` /
   `scion logs <agent-name>` to view output.
5. **Resume** — `scion resume <agent-name>` to restart a stopped agent, preserving its state.

For a guided setup of Workstation mode, the [Onboarding Wizard](/scion/getting-started/onboarding/)
walks you through machine setup, runtime detection, harness selection, and your first project.

## See also

- [Choosing a Mode](/scion/choosing-a-mode/) — pick the right run mode.
- [Core Concepts](/scion/concepts/) — Agent, Project, Hub, Runtime Broker, and the state model.
- [Glossary](/scion/glossary/) — canonical definitions for every term.
- [Philosophy](/scion/philosophy/) — the "less is more" design principles behind Scion.
