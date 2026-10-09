---
title: Multi-Hub HA with Terraform
description: Run several namespaced hubs in one GCP project on shared Cloud SQL, Filestore, and GKE Autopilot, provisioned declaratively with Terraform.
---

A declarative alternative to the manual `gcloud`/`kubectl` steps in
[Deploy on GCP (Cloud Run + GKE)](/scion/hosted/ha/setup-gcp/): a Terraform
module set that provisions several namespaced **hubs** sharing one GCP
project's infrastructure.

## Overview

Each hub is its own IAP-protected Cloud Run service with its own database,
bucket, secrets, service accounts, and GKE namespace. Several hubs share the
expensive, fixed-cost pieces of the stack:

- One Cloud SQL Postgres instance (each hub gets its own database and user)
- One Filestore share (each hub gets its own subdirectory)
- One GKE Autopilot cluster (each hub gets its own namespace, RBAC, and
  Workload Identity binding)
- One Artifact Registry repository

The module set is split into two Terraform roots: `shared-infra`, applied
once per project, and `hub`, applied once per hub on top of it. Hubs sharing
this infra form **one trust domain, not a hard multi-tenancy boundary** — see
the module README for exactly what is and isn't isolated between them.

The modules are safe to apply in a project that already runs other Scion
infrastructure. Every resource name derives from a prefix (`name_prefix` for
the shared layer, `hub_name` for a hub), and nothing existing is imported or
adopted: a name collision fails the apply. All IAM grants are additive
(`google_*_iam_member` only), so applying never replaces a project's existing
IAM bindings.

### Running a hub on GKE instead of Cloud Run

A third root, `hub-gke`, is a sibling of `hub` that runs one hub inside the
shared GKE Autopilot cluster instead of on Cloud Run. It installs the in-repo
[`scion-hub` Helm chart](/scion/hosted/ha/helm/) with a digest-pinned image
built from the `hub-gke` target, and puts a Terraform-owned global external
HTTPS load balancer with IAP in front of it (managed certificate, a 24-hour
backend timeout for WebSockets, and zonal standalone NEGs). DNS stays
external. It needs GKE 1.36.2-gke.3104000 or later on the shared cluster.
Cloud Run and GKE hubs share the same shared infra, naming rules, and state
layout; see
[`deploy/terraform/configurations/hub-gke/README.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/deploy/terraform/configurations/hub-gke/README.md)
for the apply sequence.

## When to choose it

Choose this over the manual GCP setup guide when you want:

- **More than one hub in a project**, sharing infrastructure cost-effectively
  during development, rather than provisioning a full stack per hub.
- **The whole stack as reviewable code** — a Terraform plan you and your team
  review before it touches anything, instead of a sequence of manual
  `gcloud` commands or a one-shot wizard script.
- **A repeatable teardown** with a guard that refuses to destroy shared
  infrastructure while any hub still depends on it.

Stay with the manual guide, or a single-hub tier, if you only need one hub
and prefer not to introduce a Terraform apply workflow. See
[Choosing a Mode](/scion/choosing-a-mode/) for where HA hosted sits among
Scion's run modes; for a full tier comparison including this one, see
[`docs/deploy/choosing-a-mode.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/docs/deploy/choosing-a-mode.md)
in the repository.

## Getting started

The full procedure — prerequisites, the shared-infra and per-hub applies,
verification, the post-apply hub environment step, image rolls, and
teardown — lives in the repository, not duplicated here:

- [`deploy/terraform/README.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/deploy/terraform/README.md) —
  the detailed module reference: every variable, module interface, and
  operational edge case.
- [`docs/deploy/terraform-ha.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/docs/deploy/terraform-ha.md) —
  a short operator how-to that points into the README for detail.
- [`docs/deploy/agent-runbook-terraform-ha.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/docs/deploy/agent-runbook-terraform-ha.md) —
  a step-by-step runbook for an AI agent to run the deployment end to end,
  including the plan-review gates required before every apply or destroy.

:::caution[Upgrading an existing hub]
Current `terraform-ha` modules no longer create the legacy hub-scope Secret
Manager grant or pre-create the hub's OIDC signing key: a fresh hub generates
its own key on first boot, and an existing hub keeps its migrated key. Before
applying this module version to an existing hub, finish the secret-name
migration and check the expected plan delta in
[`docs/deploy/migrate-names-cloudrun.md` §7](https://github.com/GoogleCloudPlatform/scion/blob/main/docs/deploy/migrate-names-cloudrun.md#7-for-terraform-managed-hubs-what-can-be-removed-afterward).
Applying it to a hub whose secrets have not been migrated deletes the only
copy of its OIDC signing key.
:::

After a hub is up, the [hosted user guide](/scion/hosted/user/hosted-user/)
covers connecting to it, and the rest of this Admin Guide
([Runtime Brokers & Profiles](/scion/hosted/ha/runtime-broker/),
[Kubernetes Runtime](/scion/hosted/ha/kubernetes/),
[Identity & Access (RBAC)](/scion/hosted/ha/permissions/)) covers operating
it day to day.
