# Choosing a Deployment Mode

Scion offers several deployment tiers for running a hosted Hub. Each uses
different infrastructure, auth mechanisms, and storage backends. This page
compares them to help you pick the right one.

> For a broader overview of all Scion run modes (including Local and
> Workstation), see the
> [Choosing a Mode](https://scion-ai.dev/scion/choosing-a-mode/) page on the
> docs site.

## Deployment Comparison

| | Single-Node VM | Cloud Run Instance | Developer Hub | Multi-Hub HA (Terraform) |
|---|---|---|---|---|
| **What it is** | GCE VM running released binaries with IAP auth | Cloud Run Instance with sandbox-based agents and IAP auth | GCE VM that builds from source with GCS + Secret Manager | Several Cloud Run hubs behind IAP, sharing one project's Cloud SQL, Filestore, and GKE Autopilot |
| **Scripts** | `scripts/single-node-vm/` | `scripts/single-node/` | `scripts/starter-hub/` | `deploy/terraform/` |
| **Provisioning** | `deploy.sh` (interactive wizard) | `deploy.sh` (CLI flags) | `gce-demo-deploy.sh` (six-step pipeline) | `terraform apply` (shared-infra root once, hub root once per hub) |
| **Binary source** | GitHub Release download | Container image you supply | Built from source on the VM | Container image you supply |
| **Storage** | Local filesystem (SQLite + disk) | Local filesystem (SQLite) | GCS + Cloud SQL (optional) | Shared Cloud SQL Postgres (per-hub database) + shared Filestore NFS |
| **Secrets** | Local (`hub.env` on disk) | Environment variables | GCP Secret Manager | GCP Secret Manager, per-hub conditioned IAM |
| **Auth** | Cloud Run IAP proxy | Cloud Run IAP (native) | OAuth (Google/GitHub) + custom domain | Cloud Run IAP (native), per hub |
| **DNS/TLS** | None required (Cloud Run URL) | None required (Cloud Run URL) | Required (custom domain + Let's Encrypt) | None required (Cloud Run URL, per hub) |
| **Public IP** | No (VM has no public IP) | N/A (managed by Cloud Run) | Yes (VM has public IP via Caddy) | N/A (managed by Cloud Run); agents run in GKE Autopilot |
| **Teardown** | `deploy.sh --delete` | `teardown.sh` | `gce-demo-provision.sh delete` | gated `plan -destroy` → `apply`, hub roots first, then shared-infra (see README "Destroy runbook") |

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

## Multi-Hub HA (Terraform)

A declarative Terraform module set that runs several namespaced hubs in one
GCP project, sharing the expensive, fixed-cost infrastructure (Cloud SQL,
Filestore, a GKE Autopilot cluster, Artifact Registry) while keeping each
hub's database, bucket, secrets, service accounts, and Cloud Run service
isolated. Hubs sharing this infra form one trust domain, not a hard
multi-tenancy boundary.

**Best for:** Running more than one hub cost-effectively during development,
or an HA Hub tier where you want the whole stack — infra included —
reproducible and reviewable as code rather than driven by a wizard or a
sequence of manual `gcloud` commands.

**What you get:**
- One `shared-infra` Terraform root, applied once per project
- One `hub` Terraform root, applied once per hub, on top of the shared infra
- Per-hub IAP-protected Cloud Run service, GKE namespace, database, and
  secrets
- A destroy-order guard that prevents tearing down shared infra while any
  hub still exists

**Trade-offs:**
- More moving parts than a single-hub tier (two Terraform roots, a shared
  GKE cluster, per-hub IAM conditions)
- Hubs on shared infra are one trust domain, not isolated tenants — see the
  module README before running this for anything beyond cost-shared
  development
- Requires a Terraform apply workflow rather than a single provisioning
  script
- Hub `min_instances` stays at 1, and multi-instance (`max_instances > 1`)
  operation needs a hub image built from a recent-enough commit (see the
  module README's "Scaling" section); a Cloud SQL failover drill has not
  been run against this module set

See: [Multi-Hub HA: Terraform](terraform-ha.md),
[`deploy/terraform/README.md`](../../deploy/terraform/README.md)

## How to Choose

- **"I want the fastest path to a running Hub"** — **Single-Node VM**. One
  command, no domain setup, no container builds.

- **"I have a container image and prefer managed infrastructure"** — **Cloud Run
  Instance**. No VM to maintain; Cloud Run handles the lifecycle.

- **"I'm developing Scion and need to build from source"** — **Developer Hub**.
  Full source checkout, GCS storage, custom domain, and Secret Manager.

- **"I need high availability, or more than one hub sharing infrastructure"**
  — **Multi-Hub HA (Terraform)**. Declarative, reviewable, and the only tier
  here that supports several hubs sharing one project's Cloud SQL,
  Filestore, and GKE Autopilot cluster.
  - For a **single** HA hub without adopting a Terraform apply workflow,
    see [`scripts/cloudrun/`](../../scripts/cloudrun/) / the docs-site
    [Deploy on GCP](https://scion-ai.dev/scion/hosted/ha/setup-gcp/) guide
    instead.
