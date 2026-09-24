# Choosing a Deployment Mode

Scion offers several deployment tiers for running a hosted Hub. Each uses
different infrastructure, auth mechanisms, and storage backends. This page
compares them to help you pick the right one.

> For a broader overview of all Scion run modes (including Local and
> Workstation), see the
> [Choosing a Mode](https://scion-ai.dev/scion/choosing-a-mode/) page on the
> docs site.

## Deployment Comparison

| | Single-Node VM | Cloud Run Instance | Developer Hub |
|---|---|---|---|
| **What it is** | GCE VM running released binaries with IAP auth | Cloud Run Instance with sandbox-based agents and IAP auth | GCE VM that builds from source with GCS + Secret Manager |
| **Scripts** | `scripts/single-node-vm/` | `scripts/single-node/` | `scripts/starter-hub/` |
| **Provisioning** | `deploy.sh` (interactive wizard) | `deploy.sh` (CLI flags) | `gce-demo-deploy.sh` (six-step pipeline) |
| **Binary source** | GitHub Release download | Container image you supply | Built from source on the VM |
| **Storage** | Local filesystem (SQLite + disk) | Local filesystem (SQLite) | GCS + Cloud SQL (optional) |
| **Secrets** | Local (`hub.env` on disk) | Environment variables | GCP Secret Manager |
| **Auth** | Cloud Run IAP proxy | Cloud Run IAP (native) | OAuth (Google/GitHub) + custom domain |
| **DNS/TLS** | None required (Cloud Run URL) | None required (Cloud Run URL) | Required (custom domain + Let's Encrypt) |
| **Public IP** | No (VM has no public IP) | N/A (managed by Cloud Run) | Yes (VM has public IP via Caddy) |
| **Teardown** | `deploy.sh --delete` | `teardown.sh` | `gce-demo-provision.sh delete` |

## Single-Node VM

The simplest path to a persistent, SSH-accessible Hub. A single `deploy.sh`
command provisions a GCE VM, installs the Scion binary from a GitHub Release,
starts the Hub via systemd, and sets up a Cloud Run IAP reverse proxy for
browser access.

**Best for:** Operators who want a persistent VM with SSH access, minimal setup,
and no container builds. Good for evaluation, small teams, and environments
where building from source is not desired.

**What you get:**
- GCE VM with no public IP, running the Hub as a systemd service
- Cloud Run reverse proxy with IAP authentication
- Local storage (SQLite database, workspace files on disk)
- Optional chat plugins (Telegram, Discord, Slack, Teams) installed from release
  artifacts
- Interactive wizard for configuration

**Trade-offs:**
- No external database — data lives on the VM's disk
- No GCS — workspace storage is local
- No custom domain — access is via the Cloud Run proxy URL

See: [Single-Node VM Deployment Guide](single-node-vm.md)

**Adding a GKE target?** The hybrid tier extends this VM with a second,
Kubernetes-based place to run agents, sharing scratchpads over an NFS export
served from the VM itself. See: [Hybrid Deployment Tier](hybrid-tier.md)
(`ptone/scion#1777`).

## Cloud Run Instance

A single Cloud Run Instance running the Scion Hub container with IAP
protection. Agents execute in sandboxed containers managed by Cloud Run.

**Best for:** Users who want a managed container runtime with minimal
infrastructure to operate. Good when you already have a container image and
prefer Cloud Run's managed lifecycle over a VM.

**What you get:**
- Cloud Run Instance with IAP authentication
- Sandbox-based agent execution
- Managed container lifecycle (no VM to maintain)

**Trade-offs:**
- You must supply a container image
- No persistent SSH access to the host
- IAP access bindings are region-scoped and survive teardown (must be cleaned up
  manually)

See: [`scripts/single-node/README.md`](../../scripts/single-node/README.md)

## Developer Hub

A full-featured GCE deployment that clones the repository and builds from
source. Uses GCS for storage, GCP Secret Manager for secrets, and supports
custom domains with Let's Encrypt TLS certificates. Previously known as
"Starter Hub."

**Best for:** Developers and contributors who want to run a Hub built from the
latest source, with production-grade storage (GCS) and secrets (Secret Manager).
Good for development, testing, and contributing to Scion.

**What you get:**
- GCE VM with the full source repository
- Built from source (Go binary + web assets)
- GCS for workspace storage
- GCP Secret Manager for secrets
- Custom domain with wildcard TLS via Let's Encrypt
- Optional GKE cluster for agent workloads
- OAuth authentication (Google and/or GitHub)
- Caddy reverse proxy with automatic TLS

**Trade-offs:**
- Requires a registered domain with DNS delegated to Cloud DNS
- Longer initial setup (builds from source, provisions certificates)
- More moving parts (Caddy, Let's Encrypt, Cloud DNS, GCS, Secret Manager)

See: [`scripts/starter-hub/README.md`](../../scripts/starter-hub/README.md)

## How to Choose

- **"I want the fastest path to a running Hub"** — **Single-Node VM**. One
  command, no domain setup, no container builds.

- **"I have a container image and prefer managed infrastructure"** — **Cloud Run
  Instance**. No VM to maintain; Cloud Run handles the lifecycle.

- **"I'm developing Scion and need to build from source"** — **Developer Hub**.
  Full source checkout, GCS storage, custom domain, and Secret Manager.

- **"I need high availability"** — None of these are HA. For a replicated Hub
  with Cloud SQL, see the Cloud Run HA deployment in `scripts/cloudrun/`.
