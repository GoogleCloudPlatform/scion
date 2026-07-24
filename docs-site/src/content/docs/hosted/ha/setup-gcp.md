---
title: Deploy on GCP (Cloud Run + GKE)
description: End-to-end guide for deploying a Scion HA Hub on Cloud Run with GKE Autopilot for agent dispatch, IAP authentication, and Cloud SQL.
---

This guide walks through deploying a production-ready Scion HA Hub on Google Cloud Platform.
The architecture uses **Cloud Run** for the Hub (with IAP authentication), **GKE Autopilot** for
agent dispatch, and **Cloud SQL** for durable state.

**What you will set up:**

| Component | GCP Service | Purpose |
|-----------|-------------|---------|
| Hub | Cloud Run (HA, multi-instance) | Control plane, API, web UI |
| Database | Cloud SQL PostgreSQL | Durable state (Ent AutoMigrate) |
| Agent Runtime | GKE Autopilot | Isolated agent pods |
| Auth | Identity-Aware Proxy (IAP) | Zero-trust access control |
| Secrets | Secret Manager + CSI Driver | Secure secret distribution |
| Storage | GCS | Templates, artifacts, hub data |
| Images | Artifact Registry | Container image repository |

---

## 0. Prerequisites

### GCP Project

You need an active GCP project with billing enabled. Throughout this guide, replace
`$PROJECT_ID` with your project ID and `$PROJECT_NUMBER` with your project number.

```bash
export PROJECT_ID="your-project-id"
export PROJECT_NUMBER=$(gcloud projects describe $PROJECT_ID --format="value(projectNumber)")
export REGION="us-central1"          # Hub + Cloud SQL region
export GKE_REGION="us-west2"         # GKE cluster region (can differ from Hub)
```

### Enable APIs

```bash
gcloud services enable \
  run.googleapis.com \
  sqladmin.googleapis.com \
  secretmanager.googleapis.com \
  container.googleapis.com \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  iap.googleapis.com \
  iam.googleapis.com \
  --project=$PROJECT_ID
```

### CLI Tools

| Tool | Install | Verify |
|------|---------|--------|
| `gcloud` | [cloud.google.com/sdk/install](https://cloud.google.com/sdk/docs/install) | `gcloud version` |
| `kubectl` | `gcloud components install kubectl` | `kubectl version --client` |
| `gke-gcloud-auth-plugin` | `gcloud components install gke-gcloud-auth-plugin` | `gke-gcloud-auth-plugin --version` |
| `psql` | `sudo apt-get install -y postgresql-client` | `psql --version` |
| `scion` CLI | Per Scion repo README | `scion version` |

### Authenticate

```bash
gcloud auth login
gcloud config set project $PROJECT_ID
gcloud auth application-default login
```

---

## 1. Infrastructure Setup

### 1a. Artifact Registry

```bash
gcloud artifacts repositories create scion \
  --repository-format=docker \
  --location=$REGION \
  --project=$PROJECT_ID
```

Set the registry prefix (used throughout):
```bash
export IMAGE_REGISTRY="$REGION-docker.pkg.dev/$PROJECT_ID/scion"
```

### 1b. Cloud SQL — PostgreSQL Database

```bash
gcloud sql instances create scion-hub-db \
  --database-version=POSTGRES_16 \
  --tier=db-f1-micro \
  --region=$REGION \
  --project=$PROJECT_ID \
  --database-flags=max_connections=200
```

:::note[Why max_connections=200?]
Cloud Run can spin up multiple revisions during deploys. With the Hub, Discord, and agent
services all sharing one Cloud SQL instance, 200 connections provides headroom. The default
of 100 is too low — you will hit `SQLSTATE 53300` (too many connections) during redeploys.
:::

Create the database user and database:

```bash
# Set password (choose your own secure password)
export DB_PASSWORD="YourSecurePassword"

gcloud sql users create scion \
  --instance=scion-hub-db \
  --password=$DB_PASSWORD \
  --project=$PROJECT_ID

# The database is created automatically by Ent AutoMigrate on first Hub boot.
# If you prefer to pre-create:
gcloud sql databases create scionhub \
  --instance=scion-hub-db \
  --project=$PROJECT_ID
```

### 1c. GCS Bucket

```bash
export BUCKET_NAME="scion-hub-$PROJECT_ID"

gsutil mb -l $REGION -p $PROJECT_ID gs://$BUCKET_NAME/
```

### 1d. GKE Autopilot Cluster

```bash
gcloud container clusters create-auto scion-agents \
  --region=$GKE_REGION \
  --project=$PROJECT_ID \
  --enable-secret-manager
```

:::tip
The `--enable-secret-manager` flag installs the Secrets Store CSI Driver add-on for GKE,
allowing agent pods to mount secrets directly from Secret Manager as files. If you forget
this flag, you can enable it later with `gcloud container clusters update`.
:::

:::caution[GKE uses different CSI names than upstream]
| Component | Upstream (will NOT work) | GKE Managed (correct) |
|-----------|--------------------------|----------------------|
| CSI Driver | `secrets-store.csi.x-k8s.io` | `secrets-store-gke.csi.k8s.io` |
| Provider | `gcp` | `gke` |
:::

Create the agent namespace:

```bash
# Get credentials for kubectl
gcloud container clusters get-credentials scion-agents \
  --region=$GKE_REGION --project=$PROJECT_ID

kubectl create namespace scion-agents
```

**Verify:**
```bash
kubectl get namespace scion-agents
# Expected: scion-agents   Active

# Verify CSI driver is running
kubectl get pods -n kube-system | grep csi-secrets-store
# Expected: csi-secrets-store-gke-* pods Running
```

### 1e. Secret Manager — Kubeconfig for Hub

The Hub needs a kubeconfig to dispatch agents to GKE. Store it in Secret Manager:

```bash
# Generate a kubeconfig for the scion-agents cluster
KUBECONFIG=/tmp/gke-kubeconfig.yaml gcloud container clusters get-credentials \
  scion-agents --region=$GKE_REGION --project=$PROJECT_ID

# Store in Secret Manager
gcloud secrets create scion-gke-kubeconfig \
  --data-file=/tmp/gke-kubeconfig.yaml \
  --project=$PROJECT_ID

# Clean up local file
rm /tmp/gke-kubeconfig.yaml
```

:::note
The kubeconfig contains the cluster endpoint and CA cert but **not** credentials. The
Kubernetes client on Cloud Run falls back to GCE metadata auth using the Cloud Run service
account's ambient credentials. This works because the Hub SA has `roles/container.developer`
(granted in the IAM section below).
:::

### 1f. Generate Session Secret

The Hub uses a 32-byte hex session signing key:

```bash
export SESSION_SECRET=$(openssl rand -hex 32)
echo "Session secret: $SESSION_SECRET"
# Save this — you'll use it in the deploy command.
```

### Infrastructure Verification Checklist

```bash
echo "=== Infrastructure Verification ==="
echo "Artifact Registry:"
gcloud artifacts repositories list --location=$REGION --project=$PROJECT_ID \
  --format="value(name)" | grep scion && echo "  OK" || echo "  MISSING"

echo "Cloud SQL:"
gcloud sql instances describe scion-hub-db --project=$PROJECT_ID \
  --format="value(state)" 2>/dev/null && echo "  OK" || echo "  MISSING"

echo "GCS Bucket:"
gsutil ls -b gs://$BUCKET_NAME/ >/dev/null 2>&1 && echo "  OK" || echo "  MISSING"

echo "GKE Cluster:"
gcloud container clusters describe scion-agents --region=$GKE_REGION \
  --project=$PROJECT_ID --format="value(status)" 2>/dev/null && echo "  OK" || echo "  MISSING"

echo "Kubeconfig Secret:"
gcloud secrets describe scion-gke-kubeconfig --project=$PROJECT_ID \
  --format="value(name)" 2>/dev/null && echo "  OK" || echo "  MISSING"
```

---

## 2. IAM Setup

:::danger[Critical Section]
The majority of deployment failures trace back to missing IAM bindings. Every binding below
was discovered through real deployment failures — several of them fail silently (especially
the transport token minting binding in Section 2c).
:::

### 2a. Create Service Accounts

```bash
# Hub runtime SA — runs the Cloud Run Hub service
gcloud iam service-accounts create scion-hub-runner \
  --display-name="Scion Hub Runner" \
  --project=$PROJECT_ID

# Transport SA — identity used to mint IAP tokens for GKE agents
gcloud iam service-accounts create scion-transport \
  --display-name="Scion Transport Token Minter" \
  --project=$PROJECT_ID

# Discord runtime SA — runs the Discord Cloud Run service
gcloud iam service-accounts create scion-discord-runner \
  --display-name="Scion Discord Runner" \
  --project=$PROJECT_ID
```

### 2b. Hub Runner SA — Project-Level Roles

```bash
SA_HUB="scion-hub-runner@$PROJECT_ID.iam.gserviceaccount.com"

for ROLE in \
  roles/cloudsql.client \
  roles/storage.objectAdmin \
  roles/secretmanager.secretAccessor \
  roles/secretmanager.viewer \
  roles/container.developer; do

  gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$SA_HUB" \
    --role="$ROLE" \
    --condition=None \
    --quiet
done
```

| Role | Purpose |
|------|---------|
| `roles/cloudsql.client` | Connect to Cloud SQL via Unix socket |
| `roles/storage.objectAdmin` | Read/write GCS bucket (templates, artifacts) |
| `roles/secretmanager.secretAccessor` | Read secret values |
| `roles/secretmanager.viewer` | List and describe secrets |
| `roles/container.developer` | Dispatch agent pods to GKE, create K8s resources |

### 2c. Hub Runner SA — Token Creator on Transport SA

:::danger[Most commonly missed binding]
Without this binding, the Hub cannot mint IAP identity tokens for GKE agents. The failure is
**silent** — no error in Hub logs, but `SCION_TRANSPORT_TOKEN` is simply not injected into
GKE pods. The agent then fails with `"Invalid IAP credentials: empty token"`.

This binding is **on the transport SA**, not at the project level.
:::

```bash
SA_TRANSPORT="scion-transport@$PROJECT_ID.iam.gserviceaccount.com"

gcloud iam service-accounts add-iam-policy-binding \
  $SA_TRANSPORT \
  --member="serviceAccount:$SA_HUB" \
  --role="roles/iam.serviceAccountTokenCreator" \
  --project=$PROJECT_ID
```

**Why this exists:** When dispatching a GKE agent, the Hub mints an OIDC identity token on
behalf of the transport SA (via `gcpTransportMinter.MintIDToken`). This token is injected as
`SCION_TRANSPORT_TOKEN` into the agent pod. The agent uses this token as
`Authorization: Bearer` for all Hub API calls through IAP. The `serviceAccountTokenCreator`
role authorizes the Hub SA to mint tokens as the transport SA.

### 2d. Transport SA — IAP Access

The transport SA's identity is used in the IAP token. IAP must allow this identity to access
the Hub:

```bash
# Grant IAP access at project level
gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:$SA_TRANSPORT" \
  --role="roles/iap.httpsResourceAccessor" \
  --condition=None \
  --quiet
```

:::note[Why `roles/iap.httpsResourceAccessor` and not just `roles/run.invoker`?]
When Cloud Run has native IAP enabled (`run.googleapis.com/iap-enabled: 'true'`),
`roles/run.invoker` alone is NOT sufficient. The request passes through the IAP layer first,
which checks `roles/iap.httpsResourceAccessor`. Without it, GKE agents get
`403: Access denied` even with a valid token.
:::

Also grant Cloud Run invoker (needed for the underlying service):

```bash
gcloud run services add-iam-policy-binding scion-hub \
  --member="serviceAccount:$SA_TRANSPORT" \
  --role="roles/run.invoker" \
  --project=$PROJECT_ID \
  --region=$REGION
```

:::tip
Run this command **after** the Hub service is created in Section 3. Come back to this step
after the initial deploy.
:::

### 2e. Discord Runner SA — IAP Access

The Discord service calls Hub APIs through IAP (for registration, message routing, etc.):

```bash
SA_DISCORD="scion-discord-runner@$PROJECT_ID.iam.gserviceaccount.com"

# Cloud SQL access (for Discord's Cloud SQL proxy sidecar)
gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:$SA_DISCORD" \
  --role="roles/cloudsql.client" \
  --condition=None \
  --quiet

# IAP access to call Hub APIs
gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:$SA_DISCORD" \
  --role="roles/iap.httpsResourceAccessor" \
  --condition=None \
  --quiet
```

### 2f. GKE Workload Identity — Agent Pod Secret Access

Agent pods need Workload Identity (WI) bindings to access Secret Manager for CSI-mounted
secrets:

```bash
# Use the default compute SA or create a dedicated one
export GKE_GSA="${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"

# Annotate the KSA in scion-agents namespace
kubectl annotate serviceaccount default \
  --namespace=scion-agents \
  iam.gke.io/gcp-service-account=$GKE_GSA \
  --overwrite

# Grant the WI binding (KSA → GSA)
gcloud iam service-accounts add-iam-policy-binding \
  $GKE_GSA \
  --role=roles/iam.workloadIdentityUser \
  --member="serviceAccount:$PROJECT_ID.svc.id.goog[scion-agents/default]" \
  --project=$PROJECT_ID

# Ensure the GSA can access secrets
gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:$GKE_GSA" \
  --role="roles/secretmanager.secretAccessor" \
  --condition=None \
  --quiet
```

:::caution
The WI binding is **namespace-scoped** — it binds `scion-agents/default` specifically.
Pods dispatched to a different namespace (e.g. `default`) will fail with
`PermissionDenied: secretmanager.versions.access denied`.
:::

### 2g. Deployer Roles (for the human running deploys)

```bash
# Replace with your own identity
DEPLOYER="user:you@example.com"

for ROLE in \
  roles/run.admin \
  roles/iam.serviceAccountUser \
  roles/logging.viewer \
  roles/iap.admin; do

  gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="$DEPLOYER" \
    --role="$ROLE" \
    --condition=None \
    --quiet
done
```

### IAM Verification

```bash
echo "=== Hub Runner SA Roles ==="
gcloud projects get-iam-policy $PROJECT_ID \
  --flatten="bindings[].members" \
  --filter="bindings.members:$SA_HUB" \
  --format="table(bindings.role)"

echo ""
echo "=== Token Creator Binding on Transport SA ==="
gcloud iam service-accounts get-iam-policy $SA_TRANSPORT \
  --project=$PROJECT_ID \
  --format="yaml(bindings)"

echo ""
echo "=== Transport SA IAP Access ==="
gcloud projects get-iam-policy $PROJECT_ID \
  --flatten="bindings[].members" \
  --filter="bindings.members:$SA_TRANSPORT" \
  --format="table(bindings.role)"
```

### Complete IAM Reference

| Service Account | Role | Scope | Purpose |
|----------------|------|-------|---------|
| `scion-hub-runner` | `roles/cloudsql.client` | Project | Cloud SQL connections |
| `scion-hub-runner` | `roles/storage.objectAdmin` | Project | GCS bucket read/write |
| `scion-hub-runner` | `roles/secretmanager.secretAccessor` | Project | Read secret values |
| `scion-hub-runner` | `roles/secretmanager.viewer` | Project | List/describe secrets |
| `scion-hub-runner` | `roles/container.developer` | Project | GKE agent dispatch |
| `scion-hub-runner` | `roles/iam.serviceAccountTokenCreator` | **On `scion-transport` SA** | Mint IAP tokens for agents |
| `scion-transport` | `roles/iap.httpsResourceAccessor` | Project | Allow IAP access to Hub |
| `scion-transport` | `roles/run.invoker` | Hub service | Allow Cloud Run invocation |
| `scion-discord-runner` | `roles/cloudsql.client` | Project | Discord Cloud SQL proxy |
| `scion-discord-runner` | `roles/iap.httpsResourceAccessor` | Project | Call Hub API through IAP |
| GKE pods GSA | `roles/secretmanager.secretAccessor` | Project | CSI secret mounts |
| GKE pods GSA | `roles/iam.workloadIdentityUser` | **On itself, for KSA** | WI auth for pods |
| Deployer (human) | `roles/run.admin` | Project | Deploy Cloud Run services |
| Deployer (human) | `roles/iam.serviceAccountUser` | Project | Attach SAs to services |
| Deployer (human) | `roles/logging.viewer` | Project | Read deployment logs |
| Deployer (human) | `roles/iap.admin` | Project | Configure IAP |

---

## 3. Hub Deployment (Cloud Run)

### 3a. Configure IAP

Before deploying, enable IAP on the Cloud Run service. IAP is configured through the
Google Cloud Console:

1. Go to **Security > Identity-Aware Proxy** in the Cloud Console
2. After the Hub service is deployed (step 3c), find `scion-hub` in the list
3. Toggle IAP ON for the service
4. Note the **OAuth Client ID** from the IAP configuration page — you need it for transport auth

```bash
# After IAP is configured, export the OAuth client ID
export IAP_CLIENT_ID="YOUR_OAUTH_CLIENT_ID.apps.googleusercontent.com"
```

:::caution[IAP audience vs OAuth client ID]
These are different values with different purposes:

- **IAP audience** (native path): `/projects/$PROJECT_NUMBER/locations/$REGION/services/scion-hub` — used in `proxy.iap.audience` for proxy auth
- **OAuth client ID**: `$PROJECT_NUMBER-xxxx.apps.googleusercontent.com` — used in `transport.oidc_audience` for minting transport tokens

The proxy audience is the Cloud Run native resource path. The transport audience **must** be
the OAuth client ID because that is what IAP validates in the Bearer token.
:::

### 3b. Build the Hub Image

```bash
# Clone and enter the repo
git clone https://github.com/GoogleCloudPlatform/scion.git
cd scion

# Build the Hub image using Cloud Build
gcloud builds submit . \
  --tag="$IMAGE_REGISTRY/hub:latest" \
  --project=$PROJECT_ID \
  --ignore-file=.dockerignore \
  --machine-type=e2-highcpu-8 \
  --quiet
```

:::caution[Use `--ignore-file=.dockerignore`]
The default `.gcloudignore` excludes `web/` frontend files. But the Hub Dockerfile is
multi-stage — it builds the frontend and embeds it in the Go binary. Without
`--ignore-file=.dockerignore`, the build succeeds but produces a Hub with missing web assets.
:::

### 3c. Build Agent Images

Agent dispatch requires base and harness images in your registry:

```bash
# Build scion-base image
gcloud builds submit image-build/scion-base/ \
  --tag="$IMAGE_REGISTRY/scion-base:latest" \
  --project=$PROJECT_ID \
  --quiet

# Build scion-gemini-cli harness image
gcloud builds submit harnesses/gemini-cli/ \
  --tag="$IMAGE_REGISTRY/scion-gemini-cli:latest" \
  --build-arg="BASE_IMAGE=$IMAGE_REGISTRY/scion-base:latest" \
  --project=$PROJECT_ID \
  --quiet
```

:::note
The `image_registry` value in settings.yaml must match the registry where you pushed agent
images. If they don't match, agent dispatch fails with `docker.io/library/...` not found
errors.
:::

### 3d. Deploy the Hub to Cloud Run

The Hub startup requires a `settings.yaml` file because several config fields use camelCase
koanf tags that cannot be set via `SCION_SERVER_*` environment variables. Fields affected
include `oidcAudience`, `platformAuthSA`, `gcpProjectId`, `hubName`, and `adminEmails`.

```bash
HUB_NAME="hub-$(date +%Y%m%d)-initial"

gcloud run deploy scion-hub \
  --image="$IMAGE_REGISTRY/hub:latest" \
  --region=$REGION \
  --project=$PROJECT_ID \
  --service-account=scion-hub-runner@$PROJECT_ID.iam.gserviceaccount.com \
  --add-cloudsql-instances=$PROJECT_ID:$REGION:scion-hub-db \
  --set-env-vars="SCION_DEPLOY=$(date +%s),KUBECONFIG=/etc/scion/kubeconfig.yaml,SCION_K8S_NAMESPACE=scion-agents" \
  --set-secrets="/etc/scion/kubeconfig.yaml=scion-gke-kubeconfig:latest" \
  --min-instances=1 \
  --max-instances=3 \
  --cpu=1 \
  --memory=512Mi \
  --port=8080 \
  --command="bash" \
  --args="-c,mkdir -p \$HOME/.scion && printf 'schema_version: \"1\"\nimage_registry: $IMAGE_REGISTRY\nactive_profile: remote\nruntimes:\n  remote:\n    type: kubernetes\n    gke: true\n  cr:\n    type: cloudrun\n    cloudrun:\n      project: $PROJECT_ID\n      region: $REGION\nprofiles:\n  remote:\n    runtime: remote\n  cr:\n    runtime: cr\nserver:\n  database:\n    driver: postgres\n    url: postgres://scion:${DB_PASSWORD}@/scionhub?host=/cloudsql/$PROJECT_ID:$REGION:scion-hub-db\n  auth:\n    mode: proxy\n    proxy:\n      provider: iap\n      iap:\n        audience: /projects/$PROJECT_NUMBER/locations/$REGION/services/scion-hub\n    transport:\n      mode: iap\n      oidc_audience: $IAP_CLIENT_ID\n      platform_auth_sa: scion-transport@$PROJECT_ID.iam.gserviceaccount.com\n  secrets:\n    backend: gcpsm\n    gcp_project_id: $PROJECT_ID\n  hub:\n    admin_emails:\n      - your-admin@example.com\n    hub_name: $HUB_NAME\n  storage:\n    provider: gcs\n    bucket: $BUCKET_NAME\n  broker:\n    enabled: true\n    host: 127.0.0.1\n' > \$HOME/.scion/settings.yaml && exec /usr/local/bin/scion server start --hosted --enable-hub --enable-runtime-broker --enable-web --foreground --web-port 8080 --session-secret $SESSION_SECRET" \
  --no-allow-unauthenticated \
  --quiet
```

:::danger[Critical settings.yaml requirements]
1. **`schema_version: "1"` must be the first key.** Without it, the legacy loader silently drops `type: cloudrun` and the `cloudrun:` sub-block.
2. **`image_registry` is root-level** (not under `server:`). If missing, agent dispatch returns 500.
3. **`active_profile: remote`** (not `cr`). The `cr` profile dispatches agents to Cloud Run, not GKE.
4. **Runtime key must not be named `cloudrun`** — koanf cannot parse `runtimes.cloudrun.cloudrun` (same key at two levels = silent nil sub-config). Use a short name like `cr`.
5. **`gke: true`** enables Secrets Store CSI Driver path. Requires the CSI add-on on the cluster.
6. **`mkdir -p $HOME/.scion`** must run before the server starts.
:::

**Environment variables explained:**

| Env Var | Purpose |
|---------|---------|
| `SCION_DEPLOY=$(date +%s)` | Forces a new revision on redeploy (timestamp changes) |
| `KUBECONFIG=/etc/scion/kubeconfig.yaml` | Points to the Secret Manager-mounted kubeconfig |
| `SCION_K8S_NAMESPACE=scion-agents` | Tells the broker to dispatch pods to `scion-agents` namespace. Without this, pods go to `default` where the WI binding does not exist. |

### 3e. Enable IAP (if not already done)

After the service is created, enable IAP:

1. Go to **Cloud Console > Security > Identity-Aware Proxy**
2. Find `scion-hub` and toggle IAP ON
3. Add authorized users/groups as **IAP-Secured Web App User** (`roles/iap.httpsResourceAccessor`)

Now go back and run the deferred IAM binding from Section 2d:

```bash
gcloud run services add-iam-policy-binding scion-hub \
  --member="serviceAccount:scion-transport@$PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/run.invoker" \
  --project=$PROJECT_ID \
  --region=$REGION
```

### 3f. Verify Hub Health

```bash
# Check revision is serving
gcloud run services describe scion-hub \
  --region=$REGION --project=$PROJECT_ID \
  --format="yaml(status.conditions)"
# Expected: All conditions status: 'True' (Ready, ConfigurationsReady, RoutesReady)

# Get the service URL
HUB_URL=$(gcloud run services describe scion-hub \
  --region=$REGION --project=$PROJECT_ID \
  --format="value(status.url)")
echo "Hub URL: $HUB_URL"

# Test IAP is working (unauthenticated request should be blocked)
curl -s -o /dev/null -w "%{http_code}" "$HUB_URL/"
# Expected: 302 (redirect to Google sign-in) or 403 (IAP block)
```

**Check startup logs for errors:**
```bash
gcloud logging read \
  'resource.type="cloud_run_revision" AND resource.labels.service_name="scion-hub" AND severity>=ERROR' \
  --project=$PROJECT_ID --limit=10 --freshness=5m \
  --format="table(timestamp, textPayload)"
```

**Verify broker started correctly:**
```bash
gcloud logging read \
  'resource.type="cloud_run_revision" AND resource.labels.service_name="scion-hub"' \
  --project=$PROJECT_ID --limit=50 --freshness=5m \
  --format="value(jsonPayload.message, textPayload)" \
  | grep -i -E "broker|runtime|transport"
```

**Expected log lines (confirm all present):**
```
Runtime broker using runtime: kubernetes
Starting Runtime Broker API server on 127.0.0.1:9800
Runtime Broker API server starting
Message broker started: fan-out with N spoke(s)
```

**Authenticated health check:**
```bash
TOKEN=$(gcloud auth print-identity-token --audiences="$IAP_CLIENT_ID")

curl -s "$HUB_URL/healthz" \
  -H "Authorization: Bearer $TOKEN"
# Expected: {"status":"healthy",...}
```

### Common Startup Failures

| Error | Cause | Fix |
|-------|-------|-----|
| `no supported container runtime found` | `~/.scion/` dir not created | Ensure startup command has `mkdir -p $HOME/.scion` |
| `hosted HA deployment requires server.auth.transport.oidc_audience` | camelCase field set via env var | Move to settings.yaml (not env vars) |
| `gcpsm backend requires a GCP project ID` | Same camelCase issue for `gcp_project_id` | Must be in settings.yaml |
| `SQLSTATE 53300` (too many connections) | Other services holding DB connections | Scale Discord to min-instances=0 first |
| `dial tcp: lookup scion-hub-db: no such host` | Missing `--add-cloudsql-instances` | Add the Cloud SQL instance flag |

---

## 4. Discord Service Deployment (Cloud Run)

### 4a. Build Discord Image

```bash
gcloud builds submit extras/scion-discord/ \
  --tag="$IMAGE_REGISTRY/discord:latest" \
  --project=$PROJECT_ID \
  --quiet
```

### 4b. Deploy Discord Service

```bash
gcloud run deploy scion-discord \
  --image="$IMAGE_REGISTRY/discord:latest" \
  --region=$REGION \
  --project=$PROJECT_ID \
  --service-account=scion-discord-runner@$PROJECT_ID.iam.gserviceaccount.com \
  --add-cloudsql-instances=$PROJECT_ID:$REGION:scion-hub-db \
  --min-instances=1 \
  --max-instances=1 \
  --cpu=1 \
  --memory=512Mi \
  --port=8080 \
  --no-allow-unauthenticated \
  --quiet
```

:::note
Keep `min-instances=1` for Discord — Discord bots need persistent connections. Cold-start
timeouts (15-30s) can cause setup commands to time out.
:::

### 4c. Verify Discord

```bash
gcloud run services describe scion-discord \
  --region=$REGION --project=$PROJECT_ID \
  --format="yaml(status.conditions)"
# Expected: All conditions status: 'True'

# Check for errors
gcloud logging read \
  'resource.type="cloud_run_revision" AND resource.labels.service_name="scion-discord" AND severity>=ERROR' \
  --project=$PROJECT_ID --limit=10 --freshness=5m \
  --format="table(timestamp, textPayload)"
```

---

## 5. GKE Agent Dispatch Verification

By this point you should have:
- GKE Autopilot cluster with Secret Manager CSI add-on (Section 1d)
- `scion-agents` namespace created (Section 1d)
- Kubeconfig in Secret Manager (Section 1e)
- Workload Identity binding (Section 2f)
- Hub deployed with `gke: true` and `SCION_K8S_NAMESPACE=scion-agents` (Section 3d)

### 5a. Verify GKE Configuration

```bash
# Verify CSI driver is running
kubectl get csidrivers | grep secrets-store
# Expected: secrets-store-gke.csi.k8s.io

# Verify WI annotation on default KSA in scion-agents namespace
kubectl get serviceaccount default -n scion-agents -o yaml | grep gcp-service-account
# Expected: iam.gke.io/gcp-service-account: $PROJECT_NUMBER-compute@developer.gserviceaccount.com

# Verify the WI IAM binding
gcloud iam service-accounts get-iam-policy $GKE_GSA --project=$PROJECT_ID
# Expected: binding for serviceAccount:$PROJECT_ID.svc.id.goog[scion-agents/default]
```

### 5b. Settings.yaml Reference for GKE Dispatch

The key settings.yaml sections for GKE:

```yaml
schema_version: "1"
image_registry: us-central1-docker.pkg.dev/your-project/scion  # WHERE agent images live
active_profile: remote                                           # DEFAULT dispatch target

runtimes:
  remote:
    type: kubernetes
    gke: true          # true = CSI Secret Manager mounts
                       # false = K8s native Secrets (no CSI needed)
  cr:
    type: cloudrun
    cloudrun:
      project: your-project
      region: us-central1

profiles:
  remote:
    runtime: remote    # Maps to runtimes.remote (kubernetes)
  cr:
    runtime: cr        # Maps to runtimes.cr (cloudrun)
```

| `gke` Setting | Secret Storage | Requires CSI Add-on | Secret Values in Pod Spec |
|---------------|---------------|---------------------|--------------------------|
| `false` | K8s native Secrets | No | Yes (in K8s Secret object) |
| `true` | CSI → Secret Manager direct | Yes | No (fetched at mount time) |

Use `gke: true` for production (better security posture — secret values never appear in
Kubernetes objects). Use `gke: false` if the CSI add-on is not installed or for quick testing.

### Agent IAP Auth Flow

Understanding this flow is essential for debugging:

```
1. Hub dispatches agent pod to GKE
2. Hub mints IAP token via scion-transport SA
   └─ Requires: roles/iam.serviceAccountTokenCreator ON scion-transport SA
3. Token injected as SCION_TRANSPORT_TOKEN env var in pod
4. sciontool in the pod reads SCION_TRANSPORT_TOKEN → "injected" mode
5. sciontool uses token as Authorization: Bearer for Hub API calls
6. IAP validates the token against the OAuth client ID
   └─ Requires: scion-transport has roles/iap.httpsResourceAccessor
7. Hub processes the request
```

**If step 2 fails silently** (missing IAM binding): `SCION_TRANSPORT_TOKEN` is empty →
sciontool skips OIDC → IAP rejects with `"Invalid IAP credentials: empty token"`.

**Critical env vars injected into GKE agent pods:**

| Var | Expected Value | If Wrong |
|-----|---------------|----------|
| `SCION_HUB_ENDPOINT` | Hub Cloud Run URL | Agent cannot reach Hub |
| `SCION_TRANSPORT_MODE` | `iap` | Will not use IAP auth |
| `SCION_TRANSPORT_TOKEN` | Non-empty JWT | Hub did not mint token — check IAM |
| `SCION_TRANSPORT_AUDIENCE` | OAuth client ID | Token minted with wrong audience |
| `SCION_METADATA_MODE` | `block` | sciontool may try GCE metadata fallback |

---

## 6. First Agent Test (E2E Verification)

### 6a. Dispatch a Test Agent

```bash
# Get IAP token
TOKEN=$(gcloud auth print-identity-token --audiences="$IAP_CLIENT_ID")

# Check Hub is reachable
curl -s "$HUB_URL/healthz" -H "Authorization: Bearer $TOKEN"
# Expected: {"status":"healthy",...}

# Dispatch a test agent
curl -s -X POST "$HUB_URL/api/v1/agents" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "e2e-test-1",
    "template": "gemini-cli",
    "task": "echo Hello from GKE"
  }'
# Expected: HTTP 201 with agent details
```

### 6b. Verify the Agent Pod

```bash
# Wait 30-60 seconds for pod to schedule on GKE Autopilot
# (Autopilot may need to provision a new node — can take 1-2 minutes)

kubectl get pods -n scion-agents
# Expected: spawn-test--e2e-test-1-xxxx in Running or ContainerCreating state
```

### 6c. Check Agent Logs

```bash
# Get the pod name
POD=$(kubectl get pods -n scion-agents -o name | grep e2e-test-1 | head -1)

# Stream logs
kubectl logs -f -n scion-agents $POD

# Look for key indicators
kubectl logs -n scion-agents $POD --tail=100 \
  | grep -i "transport\|IAP\|heartbeat\|error\|401\|403"
```

**Success indicators:**
```
Heartbeat succeeded
Transport token refresh scheduled
```

**Failure indicators:**

| Log Line | Root Cause | Fix |
|----------|-----------|-----|
| `Heartbeat failed: ... empty token` | `SCION_TRANSPORT_TOKEN` not set | Check `serviceAccountTokenCreator` binding (Section 2c) |
| `Heartbeat failed: ... 403: Access denied` | Transport SA lacks IAP access | Grant `iap.httpsResourceAccessor` (Section 2d) |
| `Heartbeat failed: dial tcp 127.0.0.1:8080: connection refused` | Hub endpoint is localhost | Update Scion to latest |
| `FailedMount: driver name secrets-store.csi.x-k8s.io not found` | Wrong CSI driver name | Update Scion to latest |
| `PermissionDenied: secretmanager.versions.access denied` | WI binding missing or wrong namespace | Check WI binding and `SCION_K8S_NAMESPACE` (Section 2f) |

### 6d. E2E Success Criteria

All of these must be true for a successful deployment:

- [ ] Hub healthz returns 200 with IAP token
- [ ] Agent pod created in `scion-agents` namespace
- [ ] Pod reaches `Running` state
- [ ] `SCION_TRANSPORT_TOKEN` is non-empty in pod env
- [ ] Heartbeat succeeds (check pod logs)
- [ ] Agent appears in Hub agent list (`GET /api/v1/agents`)

---

## 7. Maintenance

### 7a. Redeploy Checklist

When redeploying the Hub with a new image:

1. **Scale Discord to min-instances=0** (prevents connection exhaustion):
   ```bash
   gcloud run services update scion-discord \
     --min-instances=0 \
     --region=$REGION --project=$PROJECT_ID --quiet
   ```

2. **Sync workspace to latest** before building:
   ```bash
   git fetch origin main && git rebase origin/main
   ```

3. **Build with `--ignore-file=.dockerignore`** (always):
   ```bash
   gcloud builds submit . \
     --tag="$IMAGE_REGISTRY/hub:latest" \
     --ignore-file=.dockerignore \
     --project=$PROJECT_ID --quiet
   ```

4. **Deploy with `--update-env-vars`** (not `--set-env-vars`):
   ```bash
   # CORRECT — preserves all other env vars:
   gcloud run services update scion-hub \
     --update-env-vars="SCION_DEPLOY=$(date +%s)" ...

   # WRONG — wipes all other env vars:
   # gcloud run services update scion-hub \
   #   --set-env-vars="SCION_DEPLOY=$(date +%s)" ...
   ```

5. **Restore Discord to min-instances=1**:
   ```bash
   gcloud run services update scion-discord \
     --min-instances=1 --max-instances=1 \
     --region=$REGION --project=$PROJECT_ID --quiet
   ```

### 7b. Database Reset (Clean Deploy)

For a fresh schema, drop and recreate the database. Ent AutoMigrate creates all tables on
first boot.

```bash
# Start proxy if not running
cloud-sql-proxy $PROJECT_ID:$REGION:scion-hub-db --port 5433 &

# Drop and recreate
PGPASSWORD=$DB_PASSWORD psql -h 127.0.0.1 -p 5433 -U scion -d postgres \
  -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='scionhub' AND pid <> pg_backend_pid();" \
  -c "DROP DATABASE IF EXISTS scionhub;" \
  -c "CREATE DATABASE scionhub OWNER scion;"
```

### 7c. GKE Agent Pod Cleanup

Stale agent pods may accumulate. The Hub's stale sweep marks agents as offline (typically
within 2-3 minutes of heartbeat failure), but pods may linger:

```bash
# Delete completed/failed pods
kubectl delete pods -n scion-agents --field-selector=status.phase=Succeeded
kubectl delete pods -n scion-agents --field-selector=status.phase=Failed
```

### 7d. Updating the Kubeconfig

If the GKE cluster is recreated or credentials rotate:

```bash
KUBECONFIG=/tmp/gke-kubeconfig.yaml gcloud container clusters get-credentials \
  scion-agents --region=$GKE_REGION --project=$PROJECT_ID

# Update the secret (creates a new version)
gcloud secrets versions add scion-gke-kubeconfig \
  --data-file=/tmp/gke-kubeconfig.yaml \
  --project=$PROJECT_ID

# Force Hub to pick up new secret version
gcloud run services update scion-hub \
  --update-env-vars="SCION_DEPLOY=$(date +%s)" \
  --region=$REGION --project=$PROJECT_ID --quiet

rm /tmp/gke-kubeconfig.yaml
```

---

## Appendix A: Complete settings.yaml Reference

```yaml
# === ROOT-LEVEL KEYS (not under server:) ===
schema_version: "1"                    # REQUIRED — must be first key
image_registry: us-central1-docker.pkg.dev/your-project/scion
active_profile: remote                 # Default dispatch runtime

# === RUNTIME DEFINITIONS ===
runtimes:
  remote:
    type: kubernetes
    gke: true                          # CSI secret mounts (true) or K8s Secrets (false)
    # context: my-context              # Optional: kubeconfig context name (for multi-cluster)
  cr:
    type: cloudrun                     # DO NOT name this key "cloudrun"
    cloudrun:
      project: your-project
      region: us-central1

# === PROFILE MAPPINGS ===
profiles:
  remote:
    runtime: remote
  cr:
    runtime: cr

# === SERVER CONFIGURATION ===
server:
  database:
    driver: postgres
    url: postgres://scion:PASSWORD@/scionhub?host=/cloudsql/PROJECT:REGION:INSTANCE

  auth:
    mode: proxy
    proxy:
      provider: iap
      iap:
        audience: /projects/PROJECT_NUMBER/locations/REGION/services/scion-hub
    transport:
      mode: iap
      oidc_audience: PROJECT_NUMBER-xxxx.apps.googleusercontent.com  # OAuth client ID
      platform_auth_sa: scion-transport@PROJECT_ID.iam.gserviceaccount.com

  secrets:
    backend: gcpsm
    gcp_project_id: your-project

  hub:
    admin_emails:
      - admin@example.com
    hub_name: hub-20260723-initial

  storage:
    provider: gcs
    bucket: scion-hub-your-project

  broker:
    enabled: true
    host: 127.0.0.1
```

## Appendix B: Troubleshooting Quick Reference

### IAP Token for Manual API Calls

```bash
IAP_CLIENT_ID="your-oauth-client-id.apps.googleusercontent.com"
TOKEN=$(gcloud auth print-identity-token --audiences="$IAP_CLIENT_ID")
```

### JWT Token Debugging

```bash
# Decode a JWT to inspect claims
echo "$TOKEN" | cut -d. -f2 | base64 -d 2>/dev/null | python3 -m json.tool
# Check: aud, iss, email, exp
```

### Hub API Endpoints

```bash
HUB="https://scion-hub-PROJECT_NUMBER.REGION.run.app"

# Health check
curl -s "$HUB/healthz" -H "Authorization: Bearer $TOKEN"

# List agents
curl -s "$HUB/api/v1/agents" -H "Authorization: Bearer $TOKEN" | python3 -m json.tool

# Dispatch agent
curl -s -X POST "$HUB/api/v1/agents" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"test-1","template":"gemini-cli","task":"echo hello"}'
```

### Common Log Patterns

| Log Line | Meaning | Action |
|----------|---------|--------|
| `Heartbeat failed: ... empty token` | Transport token not injected | Check tokenCreator IAM (Section 2c) |
| `Heartbeat failed: ... 403: Access denied` | Transport SA lacks IAP role | Grant iap.httpsResourceAccessor (Section 2d) |
| `dial tcp 127.0.0.1:8080: connection refused` | Hub endpoint is localhost | Update to latest Scion |
| `driver name secrets-store.csi.x-k8s.io not found` | Wrong CSI driver name | Update to latest Scion |
| `PermissionDenied: secretmanager.versions.access` | WI binding missing | Check Section 2f |
| `Runtime broker using runtime: kubernetes` | Hub started correctly with GKE | All good |
| `Heartbeat succeeded` | Agent auth working | All good |
| `SQLSTATE 53300` | Too many DB connections | Scale dependent services first |
