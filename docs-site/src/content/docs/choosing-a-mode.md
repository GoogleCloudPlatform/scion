---
title: Choosing a Mode
description: How to pick the right Scion run mode — Local, Workstation, Single-node hosted, or HA hosted — from the two dimensions that separate them.
---

Scion runs the same agents and the same CLI whether you are working alone on a laptop or operating a durable, always-on platform for a team. What changes between those situations is **how much infrastructure sits behind the agents** — and that is what a *mode* captures.

This page introduces the four run modes and helps you pick the one that fits your use case. The definitions here follow the canonical [`GLOSSARY.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/GLOSSARY.md); when in doubt, the root glossary wins.

## The mode spine

The run modes form a **spine of increasing infrastructure**:

**Local → Workstation → Single-node hosted → HA hosted**

Each step to the right adds capability — a control plane, a durable data store, multi-user access — at the cost of more moving parts to run and pay for. You are not locked in: the same projects and agents move up the spine as your needs grow.

## The two dimensions

Two independent dimensions separate the modes. Understanding them makes the choice straightforward.

### Availability tier

The **availability tier** describes the durability of the **Hub** — Scion's control plane (see [Core Concepts](/scion/concepts/)). It is fixed by the Hub's database driver, set with `SCION_SERVER_DATABASE_DRIVER`:

- **Embedded (`sqlite`)** — the Hub runs as a single instance with state in an embedded SQLite database on local or single-volume storage. Cheap and simple; accepts restart/redeploy downtime. There is no separate database to provision, secure, back up, or pay for.
- **External (`postgres`)** — the Hub is replicated behind a load balancer, backed by an external managed database (Postgres) and object storage. Highly available and durable — it survives node loss and redeploys without downtime — at the cost of running that external infrastructure.

### Tenancy

**Tenancy** describes whether a deployment serves one user or many. It is orthogonal to the availability tier and only opens up once you are hosted:

- **Single-user** — one principal, with simple auth (a workstation dev token, or one OAuth identity).
- **Multi-user** — many principals authenticated through an OAuth identity provider (Google or GitHub), with Hub **Groups** and access policies governing who can see and act on what.

Local and Workstation modes are single-user by construction. Either hosted tier can be single- or multi-user.

## The four modes

| Mode | Control plane | State & durability | Tenancy | Canonical use |
|------|---------------|--------------------|---------|----------------|
| **Local** | None (CLI only) | Local machine; git-worktree isolation | Single-user | Agents launched directly via the `scion` CLI, no server |
| **Workstation** | Combo server (Hub + Runtime Broker + Web) on loopback | Embedded SQLite on that machine | Single-user | The hosted experience locally, on your own machine |
| **Single-node hosted** | One networked Hub on a single node | Embedded SQLite, single-volume; non-HA | Single- or multi-user | A cheap, simple networked Hub — a single VM, or one Cloud Run instance + SQLite |
| **HA hosted** | Hub replicated behind a load balancer | External Postgres + object storage; highly available | Single- or multi-user | A durable, always-on shared deployment — Cloud Run + Cloud SQL |

### Local mode

Run Scion with **no server at all**. Agents are launched directly through the `scion` CLI, state lives on your machine, and isolation between agents comes from git worktrees. There is no Hub, no web dashboard, and no admin role — just you and the CLI.

Choose Local when: *"I just want agents on my machine, no server."*

Start here with the [Installation guide](/scion/getting-started/install/) and the [Tutorial](/scion/getting-started/tutorial/).

### Workstation mode

Run a single-tenant Scion server — the **combo server**, which bundles the Hub, a Runtime Broker, and the Web dashboard into one process — on your own machine over loopback. This gives you the hosted experience locally: a visual dashboard, remote-style dispatch, and project management, without deploying shared infrastructure. It is a local *server*, not the no-server CLI workflow.

Choose Workstation when: *"Give me the hosted experience locally."*

The fastest way in is the [Onboarding Wizard](/scion/getting-started/onboarding/), which `scion server start` opens automatically on first run. For the mechanics of the combo server, network bridges, and lifecycle, see [Workstation Server Mode](/scion/workstation/workstation-server/).

### Single-node hosted

A **hosted** deployment whose control plane runs as a single Hub instance on one compute node, keeping state in an embedded SQLite database. It is non-HA — it accepts restart/redeploy downtime and single-volume durability — in exchange for low cost and operational simplicity. Realized as a single VM (for example the starter-hub scripts) or a single Cloud Run instance backed by SQLite. "Single-node" scopes the *control plane* only; agents may run on other nodes.

Choose Single-node hosted when: *"A cheap, shared Hub for me or a small team."*

Because it is networked, this tier forks into a **user** journey (connect, dispatch, collaborate) and a moderate **admin** journey (provision the node, configure auth and secrets). See [Hub Setup](/scion/hosted/single-node/hub-server/).

### HA hosted

A **hosted** deployment whose control plane is replicated across multiple Hub instances behind a load balancer, backed by an external managed database (Cloud SQL Postgres) and object storage (GCS), with stateless proxy/hosted brokers. It is highly available and durable — surviving node loss and redeploys without downtime — at the cost of running and paying for that external infrastructure. Realized by the Cloud Run deployment (Cloud Run with min-instances ≥ 2 plus Cloud SQL).

Choose HA hosted when: *"A durable, always-on, multi-user platform."*

This tier requires the fullest **user/admin** split: admins provision Postgres, object storage, load-balanced Hub replicas, proxy/hosted brokers, identity providers, and observability; users largely reuse the hosted-user journey. See [Hub Setup](/scion/hosted/single-node/hub-server/) for the deployment tracks.

## How to choose

- **No server, just me** → **Local**. Nothing to run beyond the CLI.
- **The dashboard and hosted workflow, but only on my machine** → **Workstation**. One command (`scion server start`) and the onboarding wizard.
- **A shared Hub on the network, kept cheap and simple** → **Single-node hosted**. One VM or Cloud Run instance with SQLite; accepts downtime.
- **A durable, always-on platform for a team** → **HA hosted**. External Postgres, object storage, and replicated Hub instances.

The two questions that resolve almost every case: *Do I need a networked control plane at all?* (Local vs. everything else), and if so, *Do I need it to stay up through restarts and node loss?* (Single-node vs. HA). Tenancy — single- or multi-user — is a separate switch you set once you are hosted.

## See also

- [Core Concepts](/scion/concepts/) — Agent, Project, Hub, Runtime Broker, and the state model.
- [Glossary](/scion/glossary/) — canonical definitions for every term used here.
- [Onboarding Wizard](/scion/getting-started/onboarding/) — the guided setup for Workstation mode.
- [Workstation Server Mode](/scion/workstation/workstation-server/) — running the combo server locally.
