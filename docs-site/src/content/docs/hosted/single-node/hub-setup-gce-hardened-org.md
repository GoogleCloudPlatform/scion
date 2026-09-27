---
title: Deploy on a VM (Hardened Org)
description: Addendum to the GCE Hub Setup guide for GCP organizations that enforce common security/hardening org policies.
---

## Overview

Some GCP organizations enforce org policy constraints that a default (unrestricted) project does not have. `scripts/single-node-vm/deploy.sh` (see [Deploy on a VM (GCE)](/scion/hosted/single-node/hub-setup-gce/#binary-release-deployment-single-node-vm)) handles most of these automatically. This page covers which constraints are common, what the script does about each one, and the one manual step it cannot do for you.

## Common constraints

An organization with a hardening baseline commonly enforces some combination of:

- **`constraints/compute.skipDefaultNetworkCreation`** — new projects do not get an auto-mode `default` VPC network (or its `default-allow-internal` firewall rule).
- **`constraints/compute.requireShieldedVm`** — GCE VMs must have Shielded VM features (Secure Boot, vTPM, integrity monitoring) enabled.
- **A disabled default Compute Engine service account** — the `PROJECT_NUMBER-compute@developer.gserviceaccount.com` account that GCE resources fall back to when no `--service-account` is specified.
- **`constraints/iam.allowedPolicyMemberDomains`** — rejects IAM bindings for principals outside an allow-listed set of domains, including the `allUsers` principal.

## What deploy.sh handles automatically

- **Shielded VM.** The hub VM is created with `--shielded-secure-boot` (vTPM and integrity monitoring are already on by default for the `ubuntu-2204-lts` image family this script uses).
- **The disabled default Compute Engine service account.** The Cloud Run proxy is deployed with its own dedicated service account (`--service-account`), created with no project IAM roles, instead of falling back to the project's default Compute Engine service account.
- **`allUsers` / domain-restricted sharing.** The Cloud Run IAP proxy is deployed in one step with `gcloud beta run deploy ... --no-allow-unauthenticated --iap`, so an `allUsers` IAM binding is never created. `--iap` grants the IAP service agent `roles/run.invoker` on the service itself, but `gcloud` only warns if that grant fails — it never fails the deploy. The script grants it explicitly as a safety net, so a failure there stops the deploy instead of leaving the proxy silently unreachable by IAP.
- **A dedicated proxy service account.** Deploying the Cloud Run proxy with `--service-account` requires the deployer to hold `iam.serviceAccounts.actAs` on that service account. The Owner and Editor basic roles both include this permission; a deployer with a more narrowly-scoped role needs `roles/iam.serviceAccountUser` granted on the SA specifically.
- **The scoped `tcp:8080` ingress rule.** Instead of depending on (or recreating) the broad `default-allow-internal` rule, the script creates its own rule scoped to `tcp:8080`, sourced from the region's `default` subnet CIDR, targeting only the hub VM's network tag. **You do not need to create any internal-allow firewall rule yourself** — this replaces that need entirely, in every project, hardened or not.
- **A missing `default` network.** The script checks for the `default` VPC network before creating anything else, and fails fast with a pointer to this page if it is missing. It does **not** create a VPC network on your behalf — see the one manual step below.

## The one manual prerequisite: the default network

If your organization enforces `constraints/compute.skipDefaultNetworkCreation`, create the auto-mode `default` VPC network once, before running `deploy.sh`:

```bash
gcloud compute networks create default \
  --project=PROJECT_ID \
  --subnet-mode=auto
```

That's it. Do **not** also create a `default-allow-internal` firewall rule — `deploy.sh` creates its own narrowly-scoped `tcp:8080` rule instead (see above), and a broad internal-allow rule is not needed for anything else this script sets up.

## Verifying the prerequisite

```bash
gcloud compute networks describe default --project=PROJECT_ID
```

If this reports the network was not found, run the `gcloud compute networks create` command above before re-running `deploy.sh`.
