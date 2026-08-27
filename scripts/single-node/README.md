# Single-Node Cloud Run Instance Scripts

Helper scripts for deploying and tearing down a Scion Hub on a single
**Cloud Run Instance** with sandbox-based agent execution and IAP protection.

> **Looking for the HA Cloud Run deployment?** See [`scripts/cloudrun/`](../cloudrun/)
> — that deploys a horizontally scaled Cloud Run *service* backed by Cloud SQL,
> GCS, and Filestore.

## Scripts

| Script | Purpose |
|--------|---------|
| `deploy.sh` | Deploy a Cloud Run Instance with IAP. Implements the full deploy flow (identity, project number, gcloud deploy, IAP enable, IAP reconcile gate, policy bind, perimeter assertion). |
| `teardown.sh` | Delete the Instance. Prints the IAP cleanup commands but does **not** run them — see [Teardown does not remove IAP access](#teardown-does-not-remove-iap-access). |

## Quick Start

```bash
# Deploy
./scripts/single-node/deploy.sh \
  --name my-instance \
  --project my-gcp-project \
  --image us-central1-docker.pkg.dev/YOUR_PROJECT/scion/scion-omni:YOUR_TAG

# Tear down
./scripts/single-node/teardown.sh \
  --name my-instance \
  --project my-gcp-project
```

## Teardown does not remove IAP access

`teardown.sh` deletes the Cloud Run Instance and nothing else. **IAP access
bindings survive teardown.** The script prints a reminder and the exact commands,
then deliberately leaves the bindings alone: they are scoped to the *region*, not
to the instance, so removing them automatically could revoke access to other
instances in the same region.

This means that after teardown, every identity you granted IAP access is still
authorized in that region. Do not assume teardown revoked it.

List what is still bound:

```bash
gcloud iap web get-iam-policy \
  --project=my-gcp-project \
  --region=us-east4 \
  --resource-type=cloud-run
```

Remove each member you no longer want to have access:

```bash
gcloud iap web remove-iam-policy-binding \
  --project=my-gcp-project \
  --region=us-east4 \
  --resource-type=cloud-run \
  --member=user:someone@example.com \
  --role=roles/iap.httpsResourceAccessor
```

If the instance you tore down was the last one in the region, there is no reason
to keep any of these bindings.

See the full tutorial: [Deploy on Cloud Run (Sandbox)](../../docs-site/src/content/docs/hosted/single-node/hub-setup-cloudrun.md).
