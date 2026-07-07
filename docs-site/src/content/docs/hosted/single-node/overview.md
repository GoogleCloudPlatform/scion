---
title: Single-node Overview
description: What the Single-node hosted tier provides, when to use it, and how it differs from HA hosted.
---

**Single-node hosted** is the cheaper, simpler of the two [hosted](/scion/choosing-a-mode/)
availability tiers. Its control plane — the **Hub** — runs as a **single instance on one compute
node**, keeping state in an **embedded SQLite database** on local or single-volume storage. There
is no external database to provision, secure, back up, or pay for.

Use this page to understand what the tier gives you and whether it is the right target before you
follow the [Hub Setup](/scion/hosted/single-node/hub-server/) guide.

## What this tier provides

- **A networked Hub.** Unlike [Workstation mode](/scion/workstation/workstation-server/) (loopback
  only), a single-node Hub is reachable over the network, so a small team — or you, from multiple
  machines — can share projects, agents, and state.
- **Single- or multi-user.** [Tenancy](/scion/choosing-a-mode/#tenancy) is orthogonal to the
  tier: a single-node Hub can serve one user with a simple token, or many users through an OAuth
  identity provider with Groups and access policies.
- **Low cost and operational simplicity.** One VM or one Cloud Run instance backed by SQLite —
  nothing else to run.

The `sqlite` database driver (`SCION_SERVER_DATABASE_DRIVER=sqlite`) is what pins the Hub to a
single instance: a single DB connection and in-memory lifecycle-hook deduplication.

## The trade-off: non-HA

Single-node is **not** highly available. In exchange for its low cost, it accepts:

- **Restart/redeploy downtime.** There is only one Hub instance; while it restarts, the control
  plane is unavailable.
- **Single-volume durability.** State lives on one volume. Protecting it is your responsibility
  (snapshots/backups of the SQLite volume).

"Single-node" scopes the **control plane** only — agents themselves may run on other nodes via
runtime brokers.

## When to use it

Choose Single-node hosted when: *"A cheap, simple, shared Hub for me or a small team."*

- You want a networked Hub without standing up Postgres, object storage, and load-balanced
  replicas.
- Occasional restart downtime is acceptable.
- You want the lowest-friction, lowest-cost path to a shared deployment.

If you need to survive node loss and redeploys **without downtime**, step up to
[HA hosted](/scion/hosted/ha/overview/) instead.

## How it differs from HA hosted

| | Single-node hosted | [HA hosted](/scion/hosted/ha/overview/) |
|--|--|--|
| Control plane | One Hub instance on one node | Hub replicated behind a load balancer |
| Database driver | Embedded `sqlite` | External `postgres` (Cloud SQL) |
| State & durability | Single-volume; non-HA | External DB + object storage; highly available |
| Downtime on restart/redeploy | Yes | No |
| Typical realization | One VM (starter-hub scripts) or one Cloud Run instance + SQLite | Cloud Run (min-instances ≥ 2) + Cloud SQL |
| Cost & complexity | Low | Higher |
| Tenancy | Single- or multi-user | Single- or multi-user |

The two tiers are distinguished purely by the **availability tier** dimension — see
[Choosing a Mode](/scion/choosing-a-mode/) for the full picture.

## Next steps

- [Hub Setup](/scion/hosted/single-node/hub-server/) — configure and run the Hub.
- [Deploy on a VM (GCE)](/scion/hosted/single-node/hub-setup-gce/) — the starter-hub path.
- [Auth & Tenancy](/scion/hosted/single-node/auth/) — single- vs multi-user access.
- [Connecting to a Hub](/scion/hosted/user/hosted-user/) — the user-facing journey.
