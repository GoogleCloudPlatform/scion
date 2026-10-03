# Scion Documentation

Documentation for the Scion project lives in three places:

- **Design docs** — [`.design/`](../.design/) in this repo
- **User-facing docs** — [`docs-site/`](../docs-site/) (rendered documentation)
- **Repository & contributor docs** — [`docs-repo`](../docs-repo)

## Reference Documentation

- [Messaging Authorization](messaging-authorization.md) — Message mode system, decision table, piercing rules, and the `set_message_mode` API

## Deployment Guides

- [Choosing a Deployment Mode](deploy/choosing-a-mode.md) — Compare Single-Node VM, Cloud Run Instance, Developer Hub, and Multi-Hub HA (Terraform) tiers
- [Single-Node VM Deployment](deploy/single-node-vm.md) — Deploy a Hub on a GCE VM with IAP auth using released binaries
- [Cloud Run IAM Grants](deploy/cloudrun-iam-grants.md) — Required IAM grants for Cloud Run deployments
- [Multi-Hub HA: Terraform](deploy/terraform-ha.md) — Operator how-to for the multi-hub HA Terraform module set (shared Cloud SQL, Filestore, and GKE Autopilot behind per-hub Cloud Run + IAP)
- [Agent Runbook: Multi-Hub HA Deployment (Terraform)](deploy/agent-runbook-terraform-ha.md) — Step-by-step procedure for an AI agent to run the Terraform deployment end to end
- [Running migrate-names on Cloud Run hubs deployed with the terraform-ha modules](deploy/migrate-names-cloudrun.md) — Run `scion hub secret migrate-names` against a Cloud Run hub deployed with the hub-cloudrun Terraform module via a one-off Cloud Run job, never from a workstation. Hubs from the manual setup-gcp guide are not covered ([ptone/scion#2395](https://github.com/ptone/scion/issues/2395))
