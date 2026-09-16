# Scion Documentation

Documentation for the Scion project lives in three places:

- **Design docs** — [`.design/`](../.design/) in this repo
- **User-facing docs** — [`docs-site/`](../docs-site/) (rendered documentation)
- **Repository & contributor docs** — [`docs-repo`](../docs-repo)

## Reference Documentation

- [Messaging Authorization](messaging-authorization.md) — Message mode system, decision table, piercing rules, and the `set_message_mode` API

## Deployment Guides

- [Choosing a Deployment Mode](deploy/choosing-a-mode.md) — Compare Single-Node VM, Cloud Run Instance, and Developer Hub tiers
- [Single-Node VM Deployment](deploy/single-node-vm.md) — Deploy a Hub on a GCE VM with IAP auth using released binaries
- [Cloud Run IAM Grants](deploy/cloudrun-iam-grants.md) — Required IAM grants for Cloud Run deployments
