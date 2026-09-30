# Multi-Hub HA: Terraform

A minimalist how-to for an operator applying the Terraform module set under
[`deploy/terraform/`](https://github.com/GoogleCloudPlatform/scion/blob/main/deploy/terraform/README.md).
For an AI agent running this end to end, see the
[agent runbook](agent-runbook-terraform-ha.md) instead. For full detail on
every module, variable, and edge case, see
[`deploy/terraform/README.md`](../../deploy/terraform/README.md) — this page
only orients you and points into it.

## What the pattern is

Several namespaced **hubs** share one GCP project's infrastructure: one Cloud
SQL Postgres instance, one Filestore share, one GKE Autopilot cluster, and one
Artifact Registry repo. Each hub is its own Cloud Run service behind IAP, with
its own database, bucket, secrets, service accounts, and GKE namespace. Hubs
sharing infra form one trust domain, not a hard multi-tenancy boundary — see
the README's "Shared-infra trust domain and sizing" section before running
this in anything other than a development/cost-sharing setting.

The module set is split into two Terraform roots:

- **`configurations/shared-infra`** — apply once per project.
- **`configurations/hub`** — apply once per hub, on top of the shared infra.

## Prerequisites

- A GCP project with billing enabled, and an operator identity with the role
  set the README's "Prerequisites" section lists (Editor plus several
  specific admin roles). Get all of them granted up front.
- A versioned GCS state bucket: `<project>-<prefix>-tfstate`.
- A hub container image, built from the same commit as the agent images and
  pushed to the Artifact Registry repo `shared-infra` creates (build/push
  happens between the two applies, not before either).
- Terraform `>= 1.9`.

An IAP OAuth client is **not** a prerequisite — see "Post-apply hub steps"
below.

## Apply: shared infra once, then a hub once per hub

```bash
# 1. Shared infra, once per project.
terraform -chdir=deploy/terraform/configurations/shared-infra init \
  -backend-config="bucket=<project>-<prefix>-tfstate" \
  -backend-config="prefix=<prefix>/shared"
terraform -chdir=deploy/terraform/configurations/shared-infra apply \
  -var-file=terraform.tfvars

# 2. Build and push the hub image into the AR repo shared-infra just created
#    (out of scope for this Terraform).

# 3. One hub.
terraform -chdir=deploy/terraform/configurations/hub init \
  -backend-config="bucket=<project>-<prefix>-tfstate" \
  -backend-config="prefix=<prefix>/hubs/<hub_name>"
terraform -chdir=deploy/terraform/configurations/hub apply \
  -var hub_name=<hub_name> -var state_prefix=<prefix>/hubs/<hub_name> \
  -var-file=<hub_name>.tfvars
```

`state_prefix` must always be passed and must equal the `-backend-config
prefix` used at `init` — a variable `validation` block enforces this so a
mistake fails the plan loudly instead of silently applying one hub's
variables onto another hub's state.

The shared-infra apply is dominated by the GKE cluster create (several
minutes); expect the whole apply to take roughly 10 minutes. A first hub apply
can hit a 403 on its own secrets shortly after creation — that's IAM
propagation, not a wrong condition; re-apply rather than widening anything.

## Verify

1. **Health endpoints.** The hub exposes `/readyz` (gates traffic; what "up"
   means here) and `/healthz` (always 200, but does unbounded DB/NFS work —
   don't wire it up as a liveness check).
2. **A second `plan` on every root exits 0 ("No changes").** This is the
   acceptance bar. A refresh-only note (bucket lifecycle condition defaults,
   an IAM `etag` moving) is benign; an actual add/change/destroy on a plan you
   expected to be clean is a real finding.
3. **`deploy/terraform/tools/check-broker.sh`** — read-only, checks a Cloud
   Run revision's own log lines to confirm its co-located runtime broker
   actually registered and is heartbeating, rather than inferring it from the
   service being up:
   ```bash
   deploy/terraform/tools/check-broker.sh -p <project> -s <hub_name> <revision-name>
   ```

## Post-apply hub env

Before an agent can start on a fresh hub, set two hub-scope environment
values (a manual, per-hub, post-apply step — Terraform cannot do this, see
the README's "Post-apply step" section for why):

```bash
scion hub env set --scope hub --always GOOGLE_CLOUD_PROJECT=<project>
scion hub env set --scope hub --always GOOGLE_CLOUD_REGION=<region>
```

Also see the README's "GCP identity for agents" section — an agent's GCP
identity defaults to **Block**, which leaves it unauthenticated to Vertex
even with Workload Identity fully wired. Set it to **Passthrough**.

## Adding a second hub

Repeat step 3 above with a new `hub_name` and a matching `-backend-config
prefix`/`state_prefix`. The shared layer is untouched; a fresh hub coexists
with existing ones and destroying one hub never touches the others.

## Teardown order (destroy guard)

**Every hub root first, then shared — never the reverse.** The shared root's
`destroy_guard` precondition fails the apply that turns off deletion
protection while any hub database still exists. Read the README's "Destroy
runbook" section in full before tearing anything down: it also covers why the
guard alone isn't sufficient (`terraform destroy` skips preconditions
entirely) and what a compliant teardown looks like, step by step.

Never run `terraform destroy -var deletion_protection=false` against
`shared-infra` outside that documented sequence, and never combine `-target`
with `deletion_protection` on `shared-infra`.
