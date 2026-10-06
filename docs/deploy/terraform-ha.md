# Multi-Hub HA: Terraform

A short how-to for an operator applying the Terraform module set under
[`deploy/terraform/`](../../deploy/terraform/README.md).
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
  happens between the two applies, not before either) — see the [agent
  runbook](agent-runbook-terraform-ha.md)'s step 5, "Build and Push Images":
  immutable tags only, `:latest` moves only on explicit ack, and the hub
  image is built for `linux/amd64`.
- Terraform `>= 1.9`.

An IAP OAuth client is **not** a prerequisite — see "Post-apply hub steps"
below.

## Apply: shared infra once, then a hub once per hub

Apply order is: shared-infra once per project, then build and push the hub
image into the AR repo shared-infra just created (out of scope for this
Terraform — see the [agent runbook](agent-runbook-terraform-ha.md)'s step 5,
"Build and Push Images": immutable tags only, `:latest` moves only on
explicit ack, and the hub image is built for `linux/amd64`), then a hub
once per hub. See the README's "Bootstrap sequence" for the exact commands,
backend-config flags, and why `state_prefix` must
be passed and must match `-backend-config prefix` exactly. If one command
sample is useful here, it's the hub pair:

```bash
terraform -chdir=deploy/terraform/configurations/hub init \
  -backend-config="bucket=<project>-<prefix>-tfstate" \
  -backend-config="prefix=<prefix>/hubs/<hub_name>"
terraform -chdir=deploy/terraform/configurations/hub apply \
  -var hub_name=<hub_name> -var state_prefix=<prefix>/hubs/<hub_name> \
  -var-file=<hub_name>.tfvars
```

— see the README for the shared-infra pair and the first-hub-apply
403-on-secrets note (IAM propagation; re-apply rather than widening
anything). The GKE cluster create dominates the shared-infra apply; expect
roughly 10 minutes total.

## Verify

1. **Health endpoints.** See the README's "Health endpoints" section —
   `/readyz` gates traffic and is what "up" means here; `/healthz` is
   aliased as `/health` on Cloud Run (the literal path 404s there) and
   should not be used as a readiness or liveness signal.
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

Before an agent can start on a fresh hub, an admin must set two hub-scope
environment values — a manual, per-hub, post-apply step Terraform cannot do
for you. See the README's "Post-apply step" section for the commands and
why, and its "GCP identity for agents" subsection for the separate step of
setting a new agent's GCP identity to **Passthrough** (it defaults to
**Block**, which leaves it unauthenticated to Vertex even with Workload
Identity fully wired).

## Adding a second hub

Repeat the hub `init`/`apply` pair above with a new `hub_name` and a
matching `-backend-config prefix`/`state_prefix`. The shared layer is
untouched; a fresh hub coexists with existing ones and destroying one hub
never touches the others.

## Teardown order (destroy guard)

**Every hub root first, then shared — never the reverse.** Read the
README's "Destroy runbook" section in full before tearing anything down —
it is the only source for the compliant step-by-step sequence, the
prohibited commands, and why the `destroy_guard` precondition alone isn't
sufficient. Do not improvise; follow that section exactly.
