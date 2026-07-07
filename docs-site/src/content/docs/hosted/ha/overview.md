---
title: HA Overview
description: What the HA hosted tier provides, when to use it, and how it differs from Single-node hosted.
---

**HA hosted** is the durable, always-on [hosted](/scion/choosing-a-mode/) availability tier. Its
control plane — the **Hub** — is **replicated across multiple instances behind a load balancer**,
backed by an **external managed database (Cloud SQL Postgres)** and **object storage (GCS)**, with
stateless proxy/hosted brokers. It survives node loss and redeploys without downtime, at the cost
of running and paying for that external infrastructure.

Use this page to understand what the tier gives you and whether it is the right target before you
follow the admin guides in this section.

## What this tier provides

- **High availability.** Multiple Hub replicas behind a load balancer mean the control plane
  stays up through restarts, redeploys, and node loss.
- **Durable, external state.** State lives in an external Postgres database and object storage,
  not on a single volume — the store is independently managed and backed up.
- **Horizontal scale.** Stateless [proxy](/scion/glossary/) and hosted brokers can be replicated
  alongside Hub instances without broker-specific scheduling.
- **Single- or multi-user.** Like every hosted tier, [tenancy](/scion/choosing-a-mode/#tenancy)
  is orthogonal: an HA Hub can serve one user or many, with OAuth, Groups, and access policies for
  multi-user deployments.

The `postgres` database driver (`SCION_SERVER_DATABASE_DRIVER=postgres`) is what enables
replication — it provides durable, cross-instance compare-and-set for lifecycle-hook
deduplication, versus SQLite's single-instance in-memory approach.

## The trade-off: more infrastructure

HA hosted is more to run and pay for than [Single-node](/scion/hosted/single-node/overview/). You
provision and operate:

- An external managed database (Cloud SQL Postgres).
- Object storage (GCS) for durable artifacts.
- Load-balanced Hub replicas (e.g. Cloud Run with min-instances ≥ 2).
- Stateless proxy/hosted brokers, and typically identity providers, IAP, RBAC, and observability.

## When to use it

Choose HA hosted when: *"A durable, always-on platform for a team."*

- The Hub must stay available through restarts, redeploys, and node loss.
- State durability must not depend on a single volume.
- You are running a shared, multi-user platform where downtime is not acceptable.

If occasional restart downtime and single-volume durability are acceptable and you want the
lowest cost, use [Single-node hosted](/scion/hosted/single-node/overview/) instead.

## How it differs from Single-node hosted

| | HA hosted | [Single-node hosted](/scion/hosted/single-node/overview/) |
|--|--|--|
| Control plane | Hub replicated behind a load balancer | One Hub instance on one node |
| Database driver | External `postgres` (Cloud SQL) | Embedded `sqlite` |
| State & durability | External DB + object storage; highly available | Single-volume; non-HA |
| Downtime on restart/redeploy | No | Yes |
| Typical realization | Cloud Run (min-instances ≥ 2) + Cloud SQL | One VM or one Cloud Run instance + SQLite |
| Cost & complexity | Higher | Low |
| Tenancy | Single- or multi-user | Single- or multi-user |

The two tiers are distinguished purely by the **availability tier** dimension — see
[Choosing a Mode](/scion/choosing-a-mode/) for the full picture.

## Next steps

The HA admin surface is the fullest of any mode:

- [Runtime Brokers & Profiles](/scion/hosted/ha/runtime-broker/) — proxy/hosted brokers and profiles.
- [Managed Agents](/scion/hosted/single-node/managed-agents/) — cloud-managed agents that bypass brokers.
- [Kubernetes Runtime](/scion/hosted/ha/kubernetes/) — running agents on Kubernetes.
- [Identity & Access (RBAC)](/scion/hosted/ha/permissions/) and [Proxy Auth (IAP)](/scion/hosted/ha/auth-proxy-iap/).
- [Lifecycle Hooks](/scion/hosted/ha/lifecycle-hooks/) and [Multi-Broker Setup](/scion/hosted/ha/multi-broker/).
- [Connecting to a Hub](/scion/hosted/user/hosted-user/) — the shared user-facing journey.
