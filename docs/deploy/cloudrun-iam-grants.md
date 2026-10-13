# Scion Hub Cloud Run — Required IAM Grants

**Project:** ptone-experiments  
**Hub SA:** scion-hub-runner@ptone-experiments.iam.gserviceaccount.com

> ⚠️ Important: Always pass --service-account=scion-hub-runner@ptone-experiments.iam.gserviceaccount.com when deploying. If omitted, Cloud Run defaults to the Compute SA and all custom IAM grants fail.  
**Agent SA (sandbox):** scion-instance-gym@serverless-team-scion.iam.gserviceaccount.com

---

## Hub Service Account (scion-hub-runner)

These must be granted for the Hub to function correctly in Cloud Run:

### Core Hub Operation
```bash
# CloudSQL access (for Postgres connection)
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/cloudsql.client

# GCS read/write for template/harness file storage and bootstrap seeding
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/storage.objectAdmin

# GCS signed URL generation (self-impersonation for signing)
# REQUIRED for template file downloads — Hub generates signed URLs using its own SA
gcloud iam service-accounts add-iam-policy-binding \
  scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/iam.serviceAccountTokenCreator

# Secret Manager — read secrets (signing keys, DB password, etc.)
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/secretmanager.secretAccessor

# Secret Manager — list/describe secrets (needed for signing key lookup)
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/secretmanager.viewer
```

### Cloud Run Instances Runtime (co-located broker)
```bash
# Create/manage Cloud Run Instances as agent runtime
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/run.admin

# Attach service account to Cloud Run Instances
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/iam.serviceAccountUser

# Read Cloud Logging for agent log streaming
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/logging.viewer

# IAP tunnel for exec/attach to agent instances
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/iap.tunnelResourceAccessor
```

### Hub-minted service accounts (granted by default)

`scripts/cloudrun/deploy.sh` grants this role and enables the API by default;
set `SCION_HUB_SA_MINTING=false` to skip the role grant (it does not revoke an
earlier one). The Hub mints service accounts for agents with its own
credentials: it creates each account, sets IAM policy on it, and deletes it if
a follow-up grant fails, so `roles/iam.serviceAccountCreator` alone is not
enough.

```bash
# Create, set IAM policy on, and delete hub-minted service accounts
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/iam.serviceAccountAdmin --condition=None

# Issue tokens for minted service accounts
gcloud services enable iamcredentials.googleapis.com --project=ptone-experiments
```

`roles/iam.serviceAccountAdmin` applies to every service account in the
project, so in a project shared with other workloads or other hubs the Hub SA
can change IAM policy on those accounts too. The default grant assumes one hub
per GCP project. If you run more than one hub in the same project, or the
project holds other privileged service accounts, set
`SCION_HUB_SA_MINTING=false` and grant minting access separately, for example
from a dedicated project.

### GKE Autopilot Runtime (optional — for gke profile)
```bash
# Deploy pods to GKE Autopilot cluster
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/container.developer
```

### IAP (if using External LB + IAP instead of Cloud Run native IAP)
```bash
gcloud iap web add-iam-policy-binding \
  --resource-type=backend-services \
  --service=scion-hub-backend \
  --member=serviceAccount:scion-hub-runner@ptone-experiments.iam.gserviceaccount.com \
  --role=roles/iap.httpsResourceAccessor \
  --project=ptone-experiments
```

---

## Sandbox Agent SA (scion-instance-gym) — for agents running infrastructure tasks

```bash
# Secret Manager read (for accessing deployment secrets)
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-instance-gym@serverless-team-scion.iam.gserviceaccount.com \
  --role=roles/secretmanager.secretAccessor

# Network admin (for VPC/PSA setup — if needed)
gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-instance-gym@serverless-team-scion.iam.gserviceaccount.com \
  --role=roles/servicenetworking.networksAdmin

gcloud projects add-iam-policy-binding ptone-experiments \
  --member=serviceAccount:scion-instance-gym@serverless-team-scion.iam.gserviceaccount.com \
  --role=roles/compute.networkAdmin
```

---

## Status Tracker

| Grant | SA | Status |
|---|---|---|
| cloudsql.client | scion-hub-runner | ✅ granted |
| storage.objectAdmin | scion-hub-runner | ✅ granted |
| iam.serviceAccountTokenCreator (self) | scion-hub-runner | ✅ granted |
| secretmanager.secretAccessor | scion-hub-runner | ✅ granted |
| secretmanager.viewer | scion-hub-runner | ✅ granted |
| run.admin | scion-hub-runner | ✅ granted |
| iam.serviceAccountUser | scion-hub-runner | ✅ granted |
| logging.viewer | scion-hub-runner | ✅ granted |
| iap.tunnelResourceAccessor | scion-hub-runner | ✅ granted |
| iam.serviceAccountAdmin | scion-hub-runner | granted by deploy.sh unless SCION_HUB_SA_MINTING=false |
| container.developer | scion-hub-runner | ✅ granted |
| secretmanager.secretAccessor | scion-instance-gym | ✅ granted |
