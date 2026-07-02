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
| container.developer | scion-hub-runner | ✅ granted |
| secretmanager.secretAccessor | scion-instance-gym | ✅ granted |
