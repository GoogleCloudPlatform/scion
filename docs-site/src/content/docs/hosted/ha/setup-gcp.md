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

## 0. Prerequisites & Deployer Identity

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
| `gke-gcloud-auth-plugin` | See below | `gke-gcloud-auth-plugin --version` |
| `psql` | `sudo apt-get install -y postgresql-client` | `psql --version` |
| `scion` CLI | Per Scion repo README | `scion version` |

:::tip[gke-gcloud-auth-plugin in Containers]
In containerized, managed, or non-interactive environments (where standard `gcloud components install` is disabled), you must configure the Google Cloud SDK apt repository and install the plugin natively:
```bash
sudo apt-get install -y google-cloud-cli-gke-gcloud-auth-plugin
```
Otherwise, use the gcloud component manager:
```bash
gcloud components install gke-gcloud-auth-plugin
```
:::

### Authenticate

#### For Human Deployers (Interactive)
```bash
gcloud auth login
gcloud config set project $PROJECT_ID
gcloud auth application-default login
```

#### For Automated Deployers / Service Accounts (Non-Interactive / Cross-Project)
If an automated CI/CD agent or a service account from another project is executing this deployment:
1. Authenticate using the service account's key file:
   ```bash
   gcloud auth activate-service-account --key-file=KEY_FILE_PATH
   gcloud config set project $PROJECT_ID
   ```
2. Note that interactive authentication commands (`gcloud auth login` and `gcloud auth application-default login`) are disabled and should be skipped.

---

### Deployer Permission Requirements

The deployment identity (whether human or service account) must bypass two separate permission walls: **project-level IAM bindings** and **service-account-level IAM bindings**. Ensure the deployer has the following consolidated roles on the target project:

| Role | Scope | Why It's Needed |
|------|-------|-----------------|
| `roles/resourcemanager.projectIamAdmin` | Project-level | Needed to assign project-level IAM policies to service accounts (Section 2) |
| `roles/iam.serviceAccountAdmin` | Service-account-level | Needed to set service-account-level policy bindings (such as Token Creator in 2c, 2d and Workload Identity in 2g) |
| `roles/run.admin` | Service-level | Needed to deploy Cloud Run services and bind service-level permissions (`run.invoker`) |
| `roles/editor` | Project-level | General resource provisioning (Cloud SQL, GCS, Secret Manager, GKE) |

#### Granting Cross-Project Deployer Access
If deploying from a service account in a different "Infrastructure" project (e.g., `scion-deployer@infra-project.iam.gserviceaccount.com`), grant the required roles to that service account on the target project before proceeding:

```bash
DEPLOYER_SA="scion-deployer@infra-project.iam.gserviceaccount.com"

for ROLE in \
  roles/resourcemanager.projectIamAdmin \
  roles/iam.serviceAccountAdmin \
  roles/run.admin \
  roles/editor; do
  gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$DEPLOYER_SA" \
    --role="$ROLE" \
    --quiet
done
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
  --edition=ENTERPRISE \
  --tier=db-f1-micro \
  --region=$REGION \
  --project=$PROJECT_ID \
  --database-flags=max_connections=200
```

:::note[Enterprise Edition Required for Micro Tier]
You must explicitly provide `--edition=ENTERPRISE` in the instance creation command. If omitted, some projects/organizations default to `ENTERPRISE_PLUS`, which does not support the cost-effective `db-f1-micro` tier.
:::

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

:::caution[Shared Directories and Volume Capabilities Failure]
When creating a project through the Hub web UI, Scion includes a default **"scratchpad" shared directory**. Under the hood, this mounts a Kubernetes PersistentVolumeClaim with `ReadWriteMany` (RWX) access mode. 
- **The Catch:** GKE Autopilot's default storage class (`standard-rwo`) **only supports `ReadWriteOnce` (RWO)**. Attempting to dispatch an agent will fail with an opaque scheduling error: `VolumeCapabilities is invalid: specified multi writer with mount access type`, and the Hub will output `pods not found`.
- **The Fix (Option A - Easiest):** If you do not require shared directories, navigate to your project settings in the Hub UI *after* installation and **remove or disable the default shared directory** (e.g., delete the scratchpad entry).
- **The Fix (Option B - Production):** Set up Google Cloud Filestore or a compatible NFS server, configure the Filestore CSI driver, and define the custom storage class in your GKE runtime profile to support native ReadWriteMany volumes.
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
was discovered through real deployment failures.
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
  roles/secretmanager.admin \
  roles/container.developer; do

  gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$SA_HUB" \
    --role="$ROLE" \
    --quiet
done
```

| Role | Purpose |
|------|---------|
| `roles/cloudsql.client` | Connect to Cloud SQL via Unix socket |
| `roles/storage.objectAdmin` | Read/write GCS bucket (templates, artifacts) |
| `roles/secretmanager.admin` | **Required.** Read, write, and create Secret Manager secrets (e.g. API keys saved by users via the Hub web UI). Traditional viewer/accessor roles are too restrictive and cause "Internal Error" failures when writing secrets. |
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

### 2d. Hub Runner SA — Token Creator on Itself

The Hub generates signed GCS URLs for template files when dispatching agents. This operation requires the `iam.serviceAccounts.signBlob` permission, which requires the Hub Runner SA to have the Token Creator role on **itself**:

```bash
gcloud iam service-accounts add-iam-policy-binding \
  $SA_HUB \
  --member="serviceAccount:$SA_HUB" \
  --role="roles/iam.serviceAccountTokenCreator" \
  --project=$PROJECT_ID
```

Without this binding, template generation will fail with: `Permission 'iam.serviceAccounts.signBlob' denied on resource`.

### 2e. Transport SA — IAP Access

The transport SA's identity is used in the IAP token. IAP must allow this identity to access
the Hub:

```bash
# Grant IAP access at project level
gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:$SA_TRANSPORT" \
  --role="roles/iap.httpsResourceAccessor" \
  --quiet
```

:::note[Why `roles/iap.httpsResourceAccessor` and not just `roles/run.invoker`?]
When Cloud Run has native IAP enabled, `roles/run.invoker` alone is NOT sufficient. The request passes through the IAP layer first, which checks `roles/iap.httpsResourceAccessor`. Without it, GKE agents get `403: Access denied` even with a valid token.
:::

Also grant Cloud Run invoker (needed for the underlying service):

```bash
# Run this command AFTER the scion-hub service is created in Section 3
gcloud run services add-iam-policy-binding scion-hub \
  --member="serviceAccount:$SA_TRANSPORT" \
  --role="roles/run.invoker" \
  --project=$PROJECT_ID \
  --region=$REGION
```

:::tip
Make sure your deployer identity has `roles/run.admin` before running this, as it modifies service-level IAM.
:::

### 2f. Discord Runner SA — IAP Access

The Discord service calls Hub APIs through IAP (for registration, message routing, etc.):

```bash
SA_DISCORD="scion-discord-runner@$PROJECT_ID.iam.gserviceaccount.com"

# Cloud SQL access (for Discord's database interactions)
gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:$SA_DISCORD" \
  --role="roles/cloudsql.client" \
  --quiet

# IAP access to call Hub APIs
gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:$SA_DISCORD" \
  --role="roles/iap.httpsResourceAccessor" \
  --quiet
```

### 2g. GKE Workload Identity — Agent Pod Secret Access

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
  --quiet
```

:::caution
The WI binding is **namespace-scoped** — it binds `scion-agents/default` specifically.
Pods dispatched to a different namespace (e.g. `default`) will fail with
`PermissionDenied: secretmanager.versions.access denied`.
:::

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
| `scion-hub-runner` | `roles/secretmanager.admin` | Project | Read, write, create secret values |
| `scion-hub-runner` | `roles/container.developer` | Project | GKE agent dispatch |
| `scion-hub-runner` | `roles/iam.serviceAccountTokenCreator` | **On itself** | Generate GCS signed URLs |
| `scion-hub-runner` | `roles/iam.serviceAccountTokenCreator` | **On `scion-transport` SA** | Mint IAP tokens for agents |
| `scion-transport` | `roles/iap.httpsResourceAccessor` | Project | Allow IAP access to Hub |
| `scion-transport` | `roles/run.invoker` | Hub service | Allow Cloud Run invocation |
| `scion-discord-runner` | `roles/cloudsql.client` | Project | Cloud SQL connections |
| `scion-discord-runner` | `roles/iap.httpsResourceAccessor` | Project | Call Hub API through IAP |
| GKE pods GSA | `roles/secretmanager.secretAccessor` | Project | CSI secret mounts |
| GKE pods GSA | `roles/iam.workloadIdentityUser` | **On itself, for KSA** | WI auth for pods |

---

## 3. Hub Deployment (Cloud Run)

### 3a. Configure custom OAuth Client ID

For programmatic access to an IAP-protected Cloud Run service (which our GKE agent pods require), you must set up a custom OAuth Client ID:

1. In the GCP Console, go to **APIs & Services > Credentials**.
2. Click **Create Credentials** and select **OAuth client ID**.
3. Set the Application type to **Web application**.
4. Set the Name to `scion-hub-iap`.
5. Under **Authorized redirect URIs**, add the following native IAP redirect path:
   `https://iap.googleapis.com/v1/oauth/clientIds/YOUR_CLIENT_ID:handleRedirect` (Note: replace `YOUR_CLIENT_ID` with the actual client ID string generated after creation).
6. Export the Client ID for subsequent steps:
   ```bash
   export IAP_CLIENT_ID="your-client-id-string.apps.googleusercontent.com"
   ```

---

### 3b. Build Container Images

#### Docker Build Chain Architecture
Scion agent image building follows a strict dependency chain:
$$\text{core-base} \longrightarrow \text{scion-base} \longrightarrow \text{harnesses (gemini-cli, etc.)}$$

1. **`core-base`:** Contains core tools (Go compiler, Git from source, unix packages, GCS FUSE).
2. **`scion-base`:** Copies repository code (`cmd/`, `pkg/`, etc.) and builds the `scion` and `sciontool` binaries on top of `core-base`.
3. **Harnesses:** Pulls `scion-base` and adds target agent packages (like `@google/gemini-cli`).

#### Single-Architecture AMD64 Speed Up
:::tip[Avoid Emulation Build Timeouts]
The official `cloudbuild-scion-base.yaml` compiles multi-platform (`linux/amd64,linux/arm64`). Under Cloud Build, `arm64` compiles run under slow QEMU emulation on standard `amd64` machines, which **routinely times out after 30 minutes**. 
Since GKE Autopilot runs on standard `amd64` nodes by default, you can completely bypass this limit and accelerate your build by building **amd64-only** images. 

Additionally, because `gcloud builds submit --tag` does not support `--build-arg`, we must compile the dependent images using small, custom Cloud Build `--config` YAML files to feed the `BASE_IMAGE` build argument cleanly.
:::

#### 1. Clone the repository
```bash
git clone https://github.com/GoogleCloudPlatform/scion.git
cd scion
```

#### 2. Build the Hub Image
```bash
gcloud builds submit . \
  --tag="$IMAGE_REGISTRY/hub:latest" \
  --project=$PROJECT_ID \
  --ignore-file=.dockerignore \
  --machine-type=e2-highcpu-8 \
  --quiet
```
:::caution[Use `--ignore-file=.dockerignore`]
The default `.gcloudignore` excludes the `web/` frontend directory. Because the Hub's Dockerfile compiles frontend assets and embeds them directly inside the Go binary, forgetting `--ignore-file=.dockerignore` will produce a Hub container missing its entire Web UI.
:::

#### 3. Build the Core Base Image
```bash
gcloud builds submit . \
  --tag="$IMAGE_REGISTRY/core-base:latest" \
  --dockerfile=image-build/core-base/Dockerfile \
  --project=$PROJECT_ID \
  --quiet
```

#### 4. Build the Scion Base Image
Create a single-arch temporary build file to inject the custom `BASE_IMAGE` cleanly:
```bash
cat <<EOF > /tmp/scion-base-build.yaml
steps:
- name: 'gcr.io/cloud-builders/docker'
  args: [
    'build',
    '--build-arg', 'BASE_IMAGE=$IMAGE_REGISTRY/core-base:latest',
    '-t', '$IMAGE_REGISTRY/scion-base:latest',
    '-f', 'image-build/scion-base/Dockerfile',
    '.'
  ]
images:
- '$IMAGE_REGISTRY/scion-base:latest'
EOF

gcloud builds submit . --config /tmp/scion-base-build.yaml --project=$PROJECT_ID --quiet
rm /tmp/scion-base-build.yaml
```

#### 5. Build the Gemini-CLI Harness Image
Create a single-arch temporary build file:
```bash
cat <<EOF > /tmp/gemini-cli-build.yaml
steps:
- name: 'gcr.io/cloud-builders/docker'
  args: [
    'build',
    '--build-arg', 'BASE_IMAGE=$IMAGE_REGISTRY/scion-base:latest',
    '-t', '$IMAGE_REGISTRY/scion-gemini-cli:latest',
    '-f', 'harnesses/gemini-cli/Dockerfile',
    '.'
  ]
images:
- '$IMAGE_REGISTRY/scion-gemini-cli:latest'
EOF

gcloud builds submit . --config /tmp/gemini-cli-build.yaml --project=$PROJECT_ID --quiet
rm /tmp/gemini-cli-build.yaml
```

---

### 3c. Configure and Store `settings.yaml`

To eliminate extremely fragile shell escaping and prevent nested quotation failures, **do not embed settings.yaml within startup commands**. Instead, write a clean local `settings.yaml`, save it as a Secret Manager secret, and mount it natively as a file on Cloud Run.

:::caution[No Cloud Run Profile Allowed]
If the Hub is deployed on Cloud Run with GKE as the agent runtime, the `settings.yaml` **must NOT** include a Cloud Run (`cr`) runtime/profile block. Having both runtimes in the file causes transient routing issues where the broker incorrectly attempts to spin up agents via Cloud Run, outputting the error: `cloudrun: PullImage not yet implemented`. Keep the profile focused exclusively on Kubernetes.
:::

#### 1. Write the Configuration File
Create a local `settings.yaml` file:

```yaml
schema_version: "1"                    # REQUIRED — must be first key
image_registry: REGISTRY_PLACEHOLDER   # Overwritten with $IMAGE_REGISTRY
active_profile: remote                 # Default dispatch runtime is GKE

# === RUNTIME DEFINITIONS ===
runtimes:
  remote:
    type: kubernetes
    gke: true                          # Enables CSI Secret Manager mounts

# === PROFILE MAPPINGS ===
profiles:
  remote:
    runtime: remote

# === SERVER CONFIGURATION ===
server:
  database:
    driver: postgres
    url: postgres://scion:DB_PASSWORD_PLACEHOLDER@/scionhub?host=/cloudsql/PROJECT_ID:REGION:scion-hub-db

  auth:
    mode: proxy
    proxy:
      provider: iap
      iap:
        audience: /projects/PROJECT_NUMBER/locations/REGION/services/scion-hub
    transport:
      mode: iap
      oidc_audience: IAP_CLIENT_ID_PLACEHOLDER
      platform_auth_sa: scion-transport@PROJECT_ID.iam.gserviceaccount.com

  secrets:
    backend: gcpsm
    gcp_project_id: PROJECT_ID

  hub:
    admin_emails:
      - your-admin@example.com          # Admin email addresses
    hub_name: scion-hub-ha

  storage:
    provider: gcs
    bucket: BUCKET_NAME_PLACEHOLDER

  broker:
    enabled: true
    host: 127.0.0.1
```

Replace placeholders with your live values:
```bash
sed -e "s|REGISTRY_PLACEHOLDER|$IMAGE_REGISTRY|" \
    -e "s|DB_PASSWORD_PLACEHOLDER|$DB_PASSWORD|" \
    -e "s|PROJECT_ID|$PROJECT_ID|g" \
    -e "s|PROJECT_NUMBER|$PROJECT_NUMBER|g" \
    -e "s|REGION|$REGION|g" \
    -e "s|IAP_CLIENT_ID_PLACEHOLDER|$IAP_CLIENT_ID|" \
    -e "s|BUCKET_NAME_PLACEHOLDER|$BUCKET_NAME|" \
    settings.yaml > /tmp/settings-final.yaml
```

:::note[Understanding admin_emails Settings Precedence]
- **What it is:** `admin_emails` defines the list of Google/IAP accounts granted the Administrator role in the Hub Web UI, enabling them to configure projects and dispatches.
- **The Precedence Trap:** Scion's settings follow a strict precedence order: **Embedded Defaults → YAML Config → PostgreSQL Operational Settings → Environment Variables**.
- **The Friction:** When the Hub starts for the first time, YAML values are persisted directly to the Database. Any subsequent updates to `admin_emails` inside `settings.yaml` **will be silently ignored** because database-stored operational settings override them. 
- **The Workaround:** Once the Hub database is initialized, you must add administrators through the Hub Admin Web UI/API, or execute a database update manually.
:::

#### 2. Create the Secret in Secret Manager
```bash
gcloud secrets create scion-hub-settings \
  --data-file=/tmp/settings-final.yaml \
  --project=$PROJECT_ID

rm /tmp/settings-final.yaml
```

---

### 3d. Deploy the Hub to Cloud Run

We can now deploy the Hub to Cloud Run cleanly. Notice that we mount both the kubeconfig and the `settings.yaml` files as secrets directly under `/etc/scion/` and `/home/scion/.scion/`, keeping the start command extremely simple and robust.

```bash
gcloud run deploy scion-hub \
  --image="$IMAGE_REGISTRY/hub:latest" \
  --region=$REGION \
  --project=$PROJECT_ID \
  --service-account=scion-hub-runner@$PROJECT_ID.iam.gserviceaccount.com \
  --add-cloudsql-instances=$PROJECT_ID:$REGION:scion-hub-db \
  --set-env-vars="SCION_DEPLOY=$(date +%s),KUBECONFIG=/etc/scion/kubeconfig.yaml,SCION_K8S_NAMESPACE=scion-agents" \
  --set-secrets="/etc/scion/kubeconfig.yaml=scion-gke-kubeconfig:latest,/home/scion/.scion/settings.yaml=scion-hub-settings:latest" \
  --min-instances=1 \
  --max-instances=3 \
  --cpu=1 \
  --memory=512Mi \
  --port=8080 \
  --timeout=900 \
  --command="/usr/local/bin/scion" \
  --args="server,start,--hosted,--enable-hub,--enable-runtime-broker,--enable-web,--foreground,--web-port,8080,--session-secret,$SESSION_SECRET" \
  --no-allow-unauthenticated \
  --quiet
```

:::caution[Cloud Run Timeout Warning]
We explicitly set `--timeout=900` (15 minutes). When dispatching the very first agent, GKE Autopilot triggers node provisioning to scale up from 0 nodes, which routinely takes 5-10 minutes. The default Cloud Run timeout (300 seconds) will prematurely kill the request, return a `503 Service Unavailable`, and tear down the initiating container. Set the timeout to at least 900 seconds to prevent this.
:::

---

### 3e. Enable Cloud Run Native IAP

Once the Hub service is deployed, activate Native IAP via the CLI:

```bash
gcloud run services update scion-hub \
  --iap=enabled \
  --region=$REGION \
  --project=$PROJECT_ID
```

#### Finalize Invoker Permissions
Grant invoker permissions to both the `scion-transport` service account and the native IAP service agent so the authentication pipeline completes smoothly:

```bash
# Allow IAP service agent to invoke Cloud Run
gcloud run services add-iam-policy-binding scion-hub \
  --region=$REGION \
  --project=$PROJECT_ID \
  --member="serviceAccount:service-$PROJECT_NUMBER@gcp-sa-iap.iam.gserviceaccount.com" \
  --role="roles/run.invoker" \
  --quiet

# Allow scion-transport SA to invoke Cloud Run
gcloud run services add-iam-policy-binding scion-hub \
  --region=$REGION \
  --project=$PROJECT_ID \
  --member="serviceAccount:scion-transport@$PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/run.invoker" \
  --quiet
```

---

### 3f. Verify Hub Health

Retrieve your live Hub URL:
```bash
export HUB_URL=$(gcloud run services describe scion-hub \
  --region=$REGION --project=$PROJECT_ID \
  --format="value(status.url)")
echo "Hub URL: $HUB_URL"
```

Verify that unauthenticated endpoints are successfully blocked by IAP:
```bash
curl -s -o /dev/null -w "%{http_code}" "$HUB_URL/"
# Expected: 302 (redirect to Google sign-in) or 403 (IAP block)
```

#### Verify authenticated `/health` check
:::danger[Avoid /healthz Endpoints]
Do **not** hit `$HUB_URL/healthz`. The HA Hub serves operational diagnostics strictly at `/health` (the legacy `/healthz` path returns a Google IAP 404 page, which can mislead administrators into thinking their entire IAP routing is broken).
:::

```bash
TOKEN=$(gcloud auth print-identity-token --audiences="$IAP_CLIENT_ID")

curl -s "$HUB_URL/health" \
  -H "Authorization: Bearer $TOKEN"
# Expected: {"status":"healthy",...}
```

---

## 4. Discord Service Deployment (Cloud Run)

The Scion Discord service links Discord servers directly to the Hub API, enabling Slack/Discord-styled interactive chat dispatches.

### 4a. Discord Bot Setup (Developer Portal)
Before deploying, you must register a bot application with Discord:
1. Navigate to the [Discord Developer Portal](https://discord.com/developers/applications).
2. Click **New Application** and choose a name (e.g. `scion-bot`).
3. Under the **Bot** tab:
   - Click **Reset Token** and copy the generated **Bot Token** (save this as `DISCORD_BOT_TOKEN`).
   - Under **Privileged Gateway Intents**, enable **Presence Intent**, **Server Members Intent**, and **Message Content Intent** (required to read client messages).
4. Under the **OAuth2** tab, note the **Client ID** (save this as `DISCORD_APPLICATION_ID`).
5. Open your Discord client, enable developer mode (User Settings > Advanced > Developer Mode), right-click your Discord Server, and copy its ID (save this as `DISCORD_GUILD_IDS`).
6. Generate the Bot Invite Link:
   - Go to OAuth2 > URL Generator.
   - Under Scopes, select `bot`.
   - Under Bot Permissions, select `Send Messages`, `Read Message History`, and `View Channel`.
   - Copy the generated URL at the bottom and load it in your browser to invite the bot to your target server.

---

### 4b. Build Discord Image
Ensure the build context is set to the repo root (`.`) and not `extras/scion-discord/`. The Discord service depends on parent modules within the workspace directory, so building from the subdirectory will throw compilation errors.

```bash
gcloud builds submit . \
  --tag="$IMAGE_REGISTRY/discord:latest" \
  --dockerfile=extras/scion-discord/Dockerfile \
  --project=$PROJECT_ID \
  --quiet
```

---

### 4c. Deploy Discord Service
:::caution[gRPC Port Configuration]
The Discord plugin is an internal gRPC service listening on port `50051` by default. Under Cloud Run, the TCP startup probe routes traffic to the container port (`8080` by default), which causes immediate startup probe crashes and deployment failures. 
To resolve this:
1. You must inject the environment variable `GRPC_PORT=8080` to instruct the service to bind to Cloud Run's port.
2. You must enable HTTP2 support on Cloud Run using the `--use-http2` flag so gRPC traffic flows correctly.
:::

#### Required Environment Variables:
| Variable | Description |
|----------|-------------|
| `DATABASE_URL` | Cloud SQL PostgreSQL connection string |
| `DISCORD_STANDALONE` | Instructs the plugin to run in standalone listener mode (`true`) |
| `DISCORD_BOT_TOKEN` | Your secure Discord bot token |
| `DISCORD_APPLICATION_ID` | Your Discord client/application ID |
| `DISCORD_GUILD_IDS` | Comma-separated target Discord Guild (server) IDs |
| `DISCORD_HUB_URL` | The fully qualified public URL of your Hub (`$HUB_URL`) |
| `DISCORD_TRANSPORT_MODE` | Set to `iap` for Identity-Aware Proxy auth |
| `DISCORD_TRANSPORT_AUDIENCE` | The OAuth client ID of the Hub (`$IAP_CLIENT_ID`) |

#### Deployment Command:
```bash
gcloud run deploy scion-discord \
  --image="$IMAGE_REGISTRY/discord:latest" \
  --region=$REGION \
  --project=$PROJECT_ID \
  --service-account=scion-discord-runner@$PROJECT_ID.iam.gserviceaccount.com \
  --add-cloudsql-instances=$PROJECT_ID:$REGION:scion-hub-db \
  --use-http2 \
  --port=8080 \
  --set-env-vars="GRPC_PORT=8080,DISCORD_STANDALONE=true,DATABASE_URL=postgres://scion:${DB_PASSWORD}@/scionhub?host=/cloudsql/$PROJECT_ID:$REGION:scion-hub-db,DISCORD_BOT_TOKEN=$DISCORD_BOT_TOKEN,DISCORD_APPLICATION_ID=$DISCORD_APPLICATION_ID,DISCORD_GUILD_IDS=$DISCORD_GUILD_IDS,DISCORD_HUB_URL=$HUB_URL,DISCORD_TRANSPORT_MODE=iap,DISCORD_TRANSPORT_AUDIENCE=$IAP_CLIENT_ID" \
  --min-instances=1 \
  --max-instances=1 \
  --cpu=1 \
  --memory=512Mi \
  --no-allow-unauthenticated \
  --quiet
```

---

## 5. GKE Agent Dispatch Verification

By this point you should have:
- GKE Autopilot cluster with Secret Manager CSI add-on (Section 1d)
- `scion-agents` namespace created (Section 1d)
- Kubeconfig in Secret Manager (Section 1e)
- Workload Identity binding (Section 2g)
- Hub deployed with GKE support (Section 3d)

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

Our settings.yaml only exposes GKE (remote) to ensure zero profile/runtime resolution conflicts:

```yaml
schema_version: "1"
image_registry: us-central1-docker.pkg.dev/your-project/scion  # WHERE agent images live
active_profile: remote                                           # DEFAULT dispatch target

runtimes:
  remote:
    type: kubernetes
    gke: true          # true = CSI Secret Manager mounts

profiles:
  remote:
    runtime: remote    # Maps to runtimes.remote (kubernetes)
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

---

## 6. First Agent Test (E2E Verification)

### 6a. Dispatch a Test Agent

```bash
# Get IAP token
TOKEN=$(gcloud auth print-identity-token --audiences="$IAP_CLIENT_ID")

# Check Hub is reachable
curl -s "$HUB_URL/health" -H "Authorization: Bearer $TOKEN"
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

---

## 7. Maintenance & Troubleshooting

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
   ```

5. **Restore Discord to min-instances=1**:
   ```bash
   gcloud run services update scion-discord \
     --min-instances=1 --max-instances=1 \
     --region=$REGION --project=$PROJECT_ID --quiet
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
    gke: true                          # CSI secret mounts

# === PROFILE MAPPINGS ===
profiles:
  remote:
    runtime: remote

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
    hub_name: hub-ha-prod

  storage:
    provider: gcs
    bucket: scion-hub-your-project

  broker:
    enabled: true
    host: 127.0.0.1
```

---

## Appendix B: Troubleshooting Quick Reference

### 1. IAP Token for Manual API Calls
```bash
IAP_CLIENT_ID="your-oauth-client-id.apps.googleusercontent.com"
TOKEN=$(gcloud auth print-identity-token --audiences="$IAP_CLIENT_ID")
```

### 2. JWT Token Debugging
```bash
# Decode a JWT to inspect claims
echo "$TOKEN" | cut -d. -f2 | base64 -d 2>/dev/null | python3 -m json.tool
# Check: aud, iss, email, exp
```

### 3. Hub API Endpoints
```bash
HUB="https://scion-hub-PROJECT_NUMBER.REGION.run.app"

# Health check
curl -s "$HUB/health" -H "Authorization: Bearer $TOKEN"

# List agents
curl -s "$HUB/api/v1/agents" -H "Authorization: Bearer $TOKEN" | python3 -m json.tool

# Dispatch agent
curl -s -X POST "$HUB/api/v1/agents" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"test-1","template":"gemini-cli","task":"echo hello"}'
```

### 4. Diagnostic Log Patterns & Opaque Failures

#### Volume Provisioning PVC Failure (`pods "..." not found`)
- **Symptoms:** Handlers return a generic dispatch timeout: `failed to launch container: pods "test-project--test22" not found`.
- **Cause:** Your Hub project includes a default `scratchpad` shared directory, which mounts a `ReadWriteMany` PVC. If GKE Autopilot only has the default `standard-rwo` storage class (which only supports `ReadWriteOnce`), GKE Autopilot fails to bind the volume and immediately destroys the pod.
- **Diagnosis:** Run `kubectl get pvc -n scion-agents` and `kubectl describe pvc -n scion-agents`. Look for: `VolumeCapabilities is invalid: specified multi writer with mount access type`.
- **Resolution:** Delete/disable the `scratchpad` shared directory from your project settings in the Hub UI, or provision a Google Cloud Filestore backend to support ReadWriteMany PVC mounts.

#### Pull Image Not Yet Implemented (`cloudrun: PullImage not yet implemented`)
- **Symptoms:** The Hub returns a `PullImage not yet implemented` error.
- **Cause:** The runtime broker has incorrectly attempted to route your dispatch request to a Cloud Run (`cr`) backend instead of your GKE (`remote`) backend.
- **Resolution:** Remove any `cr` / `cloudrun` profile or runtime mappings from your `settings.yaml` secret in Secret Manager, leaving GKE (`remote`) as the sole configured profile.

#### Administrative Email Changes Ignored
- **Symptoms:** You updated `admin_emails` in `settings.yaml` and redeployed, but the added user accounts still cannot perform administrative tasks in the Hub UI.
- **Cause:** Once the Hub database is initially booted, all YAML-configured Hub settings are written to the database. These database settings permanently supersede future YAML changes.
- **Resolution:** Explicitly update the settings through the Hub Admin UI, or manually modify the `settings` table inside your Cloud SQL PostgreSQL database.

#### Standard Error Resolution Table
| Log Line | Root Cause | Fix |
|----------|-----------|-----|
| `Heartbeat failed: ... empty token` | Transport token not injected | Check `serviceAccountTokenCreator` IAM (Section 2c) |
| `Heartbeat failed: ... 403: Access denied` | Transport SA lacks IAP role | Grant `iap.httpsResourceAccessor` (Section 2e) |
| `dial tcp 127.0.0.1:8080: connection refused` | Hub endpoint is localhost | Upgrade Scion CLI to the latest version |
| `driver name secrets-store.csi.x-k8s.io not found` | Wrong CSI driver name on GKE | GKE requires `secrets-store-gke.csi.k8s.io`. Ensure `gke: true` is configured in `settings.yaml`. |
| `PermissionDenied: secretmanager.versions.access` | WI binding missing | Check GKE Workload Identity bindings (Section 2g) |
| `SQLSTATE 53300` | Too many DB connections | Scale dependent services (like Discord) to 0 instances before upgrading. |
