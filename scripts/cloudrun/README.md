# Scion Hub Cloud Run HA Deployment

This directory deploys the production Cloud Run shape described in
`.design/hub-cloudrun-deployment.md`: a horizontally scalable Hub/Web service
with a co-located stateless Runtime Broker, protected by Cloud Run native IAP.

The production path uses Cloud SQL Postgres and GCS from the start. SQLite,
local filesystem storage, and GKE broker targeting are demo or alternate-runtime
material and are not configured by `deploy.sh`.

## Architecture

```text
User / agent / CLI
  -> Cloud Run native IAP
  -> scion server --enable-hub --enable-web --enable-runtime-broker
  -> Cloud SQL Postgres, GCS, Cloud Run Instances, Filestore
```

Key properties:

- Cloud Run native IAP is enabled directly on the service.
- The IAP audience is `/projects/<PROJECT_NUMBER>/locations/<REGION>/services/<SERVICE_NAME>`.
- Hub state and realtime events use Postgres, not SQLite.
- Template/workspace artifacts use GCS, not instance-local storage.
- One logical broker identity (`server.broker.broker_id`) is shared by all Cloud Run replicas.
- The default runtime is Cloud Run Instances (`runtimes.cloudrun.type: cloudrun`).

## Prerequisites

- `gcloud`, `docker`, `python3`, and `openssl`.
- Enabled APIs: Cloud Run, IAP, Artifact Registry, Secret Manager, IAM Credentials,
  Cloud SQL Admin, Cloud Storage, and Cloud Logging.
- A Cloud SQL Postgres instance in the target region.
- A GCS bucket for Hub artifacts.
- Filestore/NFS details for Cloud Run Instances workspaces.
- A VPC network/subnetwork usable by Cloud Run Instances.

## Required Configuration

Set these environment variables before running `deploy.sh`:

| Variable | Description |
| --- | --- |
| `SCION_PROJECT` | GCP project ID. Defaults to `deploy-demo-test`. |
| `SCION_REGION` | GCP region. Defaults to `us-central1`. |
| `SCION_SERVICE` | Cloud Run service name. Defaults to `scion-hub`. |
| `SCION_CLOUDSQL_INSTANCE` | Cloud SQL instance name, not the full connection name. |
| `SCION_DATABASE_NAME` | Postgres database name. Defaults to `scionhub`. |
| `SCION_DATABASE_USER` | Postgres user. Defaults to `scion`. |
| `SCION_DATABASE_PASSWORD` | Postgres password, used to render the DSN. |
| `SCION_DATABASE_PASSWORD_SECRET` | Alternative to `SCION_DATABASE_PASSWORD`; reads the latest Secret Manager version. |
| `SCION_DATABASE_URL` | Alternative full Postgres DSN. Use this to bypass DSN assembly. |
| `SCION_GCS_BUCKET` | GCS bucket for Hub storage. |
| `SCION_RUNTIME_NETWORK` | VPC network for Cloud Run Instances. |
| `SCION_RUNTIME_SUBNETWORK` | VPC subnetwork for Cloud Run Instances. |
| `SCION_FILESTORE_IP` | Filestore/NFS server IP. |
| `SCION_FILESTORE_EXPORT` | Filestore/NFS export path, for example `/scion-workspaces`. |

Useful optional variables:

| Variable | Default | Description |
| --- | --- | --- |
| `SCION_MIN_INSTANCES` | `2` | Production HA minimum Cloud Run instances. |
| `SCION_MAX_INSTANCES` | `10` | Cloud Run max instances; size this against the DB connection budget. |
| `SCION_DB_MAX_OPEN_CONNS` | `10` | Per-replica Postgres max open connections. |
| `SCION_DB_MAX_IDLE_CONNS` | `5` | Per-replica Postgres idle connections. |
| `SCION_PUBLIC_URL` | discovered after first deploy | Public Hub URL injected into settings. |
| `SCION_BROKER_ID` | `cloudrun-instances` | Stable logical broker ID shared by all replicas. |
| `SCION_BROKER_NAME` | `Cloud Run Instances` | Display name for the logical broker. |
| `SCION_SESSION_SECRET` | generated | Shared cookie/JWT signing secret stored in Secret Manager. |
| `SCION_IAP_CLIENT_ID` / `SCION_IAP_CLIENT_SECRET` | unset | Optional custom OAuth client for IAP. |

## Deploy

```bash
export SCION_PROJECT=deploy-demo-test
export SCION_REGION=us-central1
export SCION_CLOUDSQL_INSTANCE=scion-hub-postgres
export SCION_DATABASE_NAME=scionhub
export SCION_DATABASE_USER=scion
export SCION_DATABASE_PASSWORD_SECRET=scion-hub-db-password
export SCION_GCS_BUCKET=scion-hub-artifacts
export SCION_RUNTIME_NETWORK=projects/deploy-demo-test/global/networks/scion
export SCION_RUNTIME_SUBNETWORK=projects/deploy-demo-test/regions/us-central1/subnetworks/scion
export SCION_FILESTORE_IP=10.0.0.2
export SCION_FILESTORE_EXPORT=/scion-workspaces

./scripts/cloudrun/deploy.sh
```

Redeploy an already-pushed image:

```bash
./scripts/cloudrun/deploy.sh --skip-build
```

On a first deployment, the script may deploy twice. The first pass lets Cloud
Run allocate the service URL; the second pass updates the settings secret with
that URL unless `SCION_PUBLIC_URL` was provided.

## What the Script Does

1. Creates or reuses the Hub, transport-auth, and agent runtime service accounts.
2. Grants the Hub service account Cloud SQL, Secret Manager, GCS, Cloud Run
   Instances, logging, IAP tunnel, and service-account attachment permissions.
3. Grants the Hub service account token-creator access on the transport SA.
4. Builds and pushes the Hub container image to Artifact Registry.
5. Renders `hub-settings-template.yaml` with Postgres, GCS, IAP transport, and
   Cloud Run runtime configuration.
6. Stores settings and the shared session secret in Secret Manager.
7. Deploys Cloud Run with `--iap`, `--no-allow-unauthenticated`,
   `--no-cpu-throttling`, min instances `>= 2`, and Cloud SQL attachment.
8. Grants the IAP service agent `roles/run.invoker`.
9. Grants the transport SA `roles/iap.httpsResourceAccessor`.

## Verification

```bash
URL=$(gcloud run services describe scion-hub \
  --region us-central1 --project deploy-demo-test \
  --format="value(status.url)")

gcloud run services describe scion-hub \
  --region us-central1 --project deploy-demo-test \
  --format="value(metadata.annotations.run.googleapis.com/iap-enabled)"

curl -I "$URL"
```

The unauthenticated request should be blocked or redirected by IAP. Grant human
access with:

```bash
gcloud iap web add-iam-policy-binding \
  --project=deploy-demo-test \
  --region=us-central1 \
  --resource-type=cloud-run \
  --service=scion-hub \
  --member=user:EMAIL \
  --role=roles/iap.httpsResourceAccessor
```

## Files

| File | Purpose |
| --- | --- |
| `Dockerfile` | Multi-stage web plus Go build for Cloud Run. |
| `entrypoint.sh` | Starts Hub, Web, and Runtime Broker in production mode. |
| `deploy.sh` | End-to-end production Cloud Run deploy script. |
| `hub-settings-template.yaml` | Versioned production settings template. |
