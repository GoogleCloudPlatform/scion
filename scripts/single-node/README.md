# Single-Node Cloud Run Instance Scripts

Helper scripts for deploying and tearing down a Scion Hub on a single
**Cloud Run Instance** with sandbox-based agent execution and IAP protection.

> **Looking for the HA Cloud Run deployment?** See [`scripts/cloudrun/`](../cloudrun/)
> — that deploys a horizontally scaled Cloud Run *service* backed by Cloud SQL,
> GCS, and Filestore.

## Scripts

| Script | Purpose |
|--------|---------|
| `deploy.sh` | Deploy a Cloud Run Instance with IAP. Wraps `scion deploy-instance`. |
| `teardown.sh` | Delete the Instance and remove the IAP policy binding. |

## Quick Start

```bash
# Deploy
./scripts/single-node/deploy.sh \
  --name my-instance \
  --project my-gcp-project \
  --image us-docker.pkg.dev/ptone-misc/scion-alt/scion-omni:f99a818

# Tear down
./scripts/single-node/teardown.sh \
  --name my-instance \
  --project my-gcp-project
```

See the full tutorial: [Deploy on Cloud Run (Single-Node)](../../docs-site/src/content/docs/hosted/single-node/cloud-run.md).
