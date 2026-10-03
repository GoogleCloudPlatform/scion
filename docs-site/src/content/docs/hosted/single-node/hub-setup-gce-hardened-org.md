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
- **The disabled default Compute Engine service account.** The Cloud Run proxy is deployed with its own dedicated service account (`--service-account`), created with no project IAM roles, instead of falling back to the project's default Compute Engine service account. See "Deployer permissions" below for what deploying with a dedicated service account requires of the deployer.
- **`allUsers` / domain-restricted sharing.** The Cloud Run IAP proxy is deployed in one step with `gcloud beta run deploy ... --no-allow-unauthenticated --iap`, so an `allUsers` IAM binding is never created. `--iap` grants the IAP service agent `roles/run.invoker` on the service itself, but `gcloud` only warns if that grant fails — it never fails the deploy. The script grants it explicitly as a safety net, so a failure there stops the deploy instead of leaving the proxy silently unreachable by IAP.
- **The scoped `tcp:8080` ingress rule.** Instead of depending on (or recreating) the broad `default-allow-internal` rule, the script creates its own rule scoped to `tcp:8080`, sourced from the region's `default` subnet CIDR, targeting only the hub VM's network tag. **You do not need to create any internal-allow firewall rule yourself** — this replaces that need entirely, in every project, hardened or not.
- **An existing Cloud NAT.** Before it creates the service account or any other resources, the script checks every Cloud Router in the region on network `default` for an existing NAT gateway. If one already covers the `default` subnet, the script reuses it and logs which router and NAT provide egress. If another NAT exists but does not cover `default`, the script creates its own NAT scoped to just that subnet (`--nat-custom-subnet-ip-ranges=default`) so the two gateways can coexist. The check requires `jq` and fails closed if the NAT configuration cannot be read.
- **A missing `default` network.** The script checks for the `default` VPC network before creating anything else, and fails fast with a pointer to this page if it is missing. It does **not** create a VPC network on your behalf — see the one manual step below.

## Deployer permissions

Some of the automation above needs the deployer's own account to hold specific IAM permissions that the script itself cannot grant:

- **`iam.serviceAccounts.actAs`** on the Cloud Run proxy's dedicated service account. Deploying the proxy with `--service-account` requires this. The Owner and Editor basic roles both include this permission; a deployer with a more narrowly-scoped role needs `roles/iam.serviceAccountUser` granted on the SA specifically. If it's missing, `deploy.sh` fails at the Cloud Run deploy step with a clear `gcloud` permission error naming the service account — the script is idempotent, so granting the role and re-running is safe.
- **`resourcemanager.projects.setIamPolicy` and `run.services.setIamPolicy`**, for the project-level and Cloud-Run-service-level IAM bindings the script creates. Editor alone has neither; see the runbook's [Permissions row](https://github.com/GoogleCloudPlatform/scion/blob/main/docs/deploy/single-node-vm.md#prerequisites) for the exact role combination that covers both (and, with the optional hybrid tier, two more).

## The one manual prerequisite: the default network

`deploy.sh` needs a `default` VPC network with a subnet named `default` in the deploy region. It does not create either on your behalf.

If your organization enforces `constraints/compute.skipDefaultNetworkCreation` (or otherwise gives new projects no default network), create one before running `deploy.sh`. Most organizations allow auto-mode networks, which create a `default` subnet in every region automatically:

```bash
gcloud compute networks create default \
  --project=PROJECT_ID \
  --subnet-mode=auto
```

Some organizations instead enforce a custom constraint that blocks auto-mode networks entirely (there is no predefined `constraints/compute.*` constraint for this; it's typically a [custom constraint](https://docs.cloud.google.com/vpc/docs/custom-constraints) on `compute.googleapis.com/Network` checking `resource.autoCreateSubnetworks == false`). In that case, create a custom-mode network with an explicitly-created `default` subnet in the deploy region instead:

```bash
gcloud compute networks create default \
  --project=PROJECT_ID \
  --subnet-mode=custom

gcloud compute networks subnets create default \
  --project=PROJECT_ID \
  --network=default \
  --region=REGION \
  --range=10.128.0.0/20   # any free RFC1918 range
```

Either shape works — `deploy.sh` only needs a subnet named `default` to exist in `REGION`; it does not care whether the network itself is auto-mode or custom-mode.

That's it. Do **not** also create a `default-allow-internal` firewall rule — `deploy.sh` creates its own narrowly-scoped `tcp:8080` rule instead (see above), and a broad internal-allow rule is not needed for anything else this script sets up.

## Verifying the prerequisite

```bash
gcloud compute networks describe default --project=PROJECT_ID
```

If this reports the network was not found, run the `gcloud compute networks create` command above before re-running `deploy.sh`.
