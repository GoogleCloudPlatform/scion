# Continuous Delivery for Scion on GKE (`hub-gke`)

This directory provides a customer-ready Google Cloud Build Continuous Delivery (CD) pipeline ([`cloudbuild-gke-cd.yaml`](./cloudbuild-gke-cd.yaml)) for running a version-pinned Scion Hub and agent runtime on GKE Autopilot using [`deploy/terraform/configurations/hub-gke`](../terraform/configurations/hub-gke/README.md).

## Architecture Overview

```
Cloud Scheduler (daily 05:00 UTC) or manual trigger
  │
  ▼
Cloud Build (deploy/cd/cloudbuild-gke-cd.yaml, dedicated CD SA)
  ├── 1. resolve-and-check-noop  (~5–10s)
  │        Reads _VERSION_TAG or LATEST.json (.channels[$_CHANNEL].version).
  │        Checks gs://<state_bucket>/<prefix>/hubs/<hub>/deployed.json + live
  │        hub.tfvars (tfvars_sha256) + default.tfstate outputs (hub_installed == true
  │        and matching @sha256 digest). Exits cleanly via /workspace/SKIP_DEPLOY
  │        when unchanged; blocks downgrades and schema DowngradeFloor violations.
  │
  ├── 2. checkout-release-tag
  │        Shallow-clones _REPO_URL at TARGET_TAG into /workspace/release-src,
  │        verifies `git describe --tags --exact-match`, and records COMMIT_SHA,
  │        SHORT_SHA, and deterministic commit-epoch BUILD_TIME.
  │
  ├── 3. prepare-build-images + build-images
  │        Installs GCE metadata Docker credential helper (no 60m OAuth expiry),
  │        skips already-pushed images on retry (`gcloud artifacts docker tags list`),
  │        caches `core-base` by content+platform hash (`image-build/core-base` + `lib`),
  │        builds `scion-hub-gke` + `core-base` + `scion-base` + all catalog harnesses
  │        from /workspace/release-src, and captures `scion-hub-gke`'s `sha256` digest.
  │
  └── 4. fetch-hub-tfvars + terraform-apply-hub-gke + record-deployed
           Runs `hashicorp/terraform:1.9.8` (acquiring a fresh 60m OAuth token for the
           `kubernetes` and `helm` providers after long image builds):
           `init -lockfile=readonly` -> `plan -out` -> `apply` -> post-apply
           `plan -detailed-exitcode` convergence gate -> writes `deployed.json` to GCS.
```

### Key Properties

- **Isolated `shared-infra` foundation:** Automated daily CD applies **only** `configurations/hub-gke`. `configurations/shared-infra` (VPC, Cloud SQL Postgres, Filestore NFS, GKE Autopilot, Artifact Registry) is applied once by an infrastructure administrator so daily tag rollouts cannot restart shared databases or require project-wide network/API admin roles on the CD service account.
- **Version-pinned Hub + harness set without `:latest`:** Every image, the Helm chart (`deploy/helm/scion-hub`), and the Terraform root are built and applied from the exact checked-out `TARGET_TAG` (`/workspace/release-src`). `scion-hub-gke` is pinned by immutable `sha256` digest and stamped with `pkg/version.Version = TARGET_TAG`; with `hub.image_tag_version_default` enabled by default, the Hub automatically dispatches `scion-<harness>:<TARGET_TAG>` across `stable`, `preview`, and `nightly` releases without ever mutating `:latest`.
- **Declarative, Git-free secrets & configuration:** Operator variables (`admin_emails`, `iap_members`, `iap_oauth_client_id`, `hostname`) live alongside Terraform state in your versioned GCS bucket (`gs://<state_bucket>/<shared_prefix>/hubs/<hub_name>/hub.tfvars`), never in Git.

---

## Step 0: Preflight Checks (Project Isolation, Quotas & CIDRs)

Before applying infrastructure, verify the following in your target GCP project:

1. **Dedicated project vs. shared project:**
   - **Recommended:** Use a dedicated GCP project per trust domain. In shared projects, older unlabelled resources or empty Terraform state buckets are difficult to distinguish from active stacks, and hubs sharing one `shared-infra` form a single trust domain (see [`deploy/terraform/README.md`](../terraform/README.md), "Shared-infra trust domain and sizing").
   - Choose a collision-free `shared_prefix` matching `^[a-z][a-z0-9]{1,7}$` (**2–8 lowercase alphanumeric characters, no hyphens**, e.g. `cdha` or `prod1`). Every resource created by `shared-infra` is prefixed `<shared_prefix>-*`, and every `hub_name` must start with `<shared_prefix>-` (e.g. `cdha-gke-1`).

2. **Quota verification:**
   Confirm regional and global headroom before running `shared-infra`:
   - **Compute / Networking:** at least 1 VPC network, 1 Cloud Router/NAT, and 1 Global Static IPv4 address per hub (`gcloud compute project-info describe --project=<project>`).
   - **Filestore Basic HDD (`1024 GiB` minimum):** check regional Filestore quota using CamelCase quota IDs (`StandardStorageGbPerRegion`):
     ```bash
     gcloud beta quotas info list \
       --service=file.googleapis.com \
       --project=<project> \
       --filter="quotaId:StandardStorageGbPerRegion"
     ```
   - **GKE Autopilot & Standalone NEGs:** sufficient regional NEG quota (`hub-gke` pre-provisions one zonal standalone NEG per zone in the cluster's `node_locations`).
   - **Cloud SQL:** sufficient PostgreSQL 16 instance/vCPU headroom in `<region>` (note: Cloud SQL instance-count caps are not always exposed via the Quotas API; check the Cloud SQL console if creation is rejected).

3. **VPC & Private Services Access (PSA) CIDR non-overlap:**
   `shared-infra` defaults to `subnet_cidr = "10.10.0.0/20"` and allocates a `/16` Private Services Access peering range (`psa_prefix_length = 16`). If you plan to peer `<prefix>-vpc` with existing corporate VPCs, inspect existing ranges first and override `subnet_cidr` in `shared-infra`'s `terraform.tfvars` to avoid overlaps:
   ```bash
   gcloud compute networks subnets list --project=<project>
   gcloud compute addresses list --global --project=<project> --filter="purpose=VPC_PEERING"
   ```

4. **Local bootstrap Terraform version & credentials:**
   - Install **Terraform `1.9.8`** (matching `.terraform.lock.hcl` and CI/CD) and verify the archive checksum against HashiCorp's `SHA256SUMS`.
   - Authenticate locally with Application Default Credentials (`gcloud auth application-default login`) or Service Account impersonation (`GOOGLE_IMPERSONATE_SERVICE_ACCOUNT`) rather than passing a raw `GOOGLE_OAUTH_ACCESS_TOKEN` env var: `shared-infra` takes ~12–15 minutes (GKE Autopilot + Cloud SQL + Filestore + PSA), and a stale short-lived token can expire mid-apply.
   - Always generate a fresh `terraform plan -out=...` immediately before `terraform apply`; never apply a saved plan file created hours earlier.

---

## Step 1: Bootstrap State Bucket, CD Service Account & `shared-infra`

### 1.1 Run `setup-cd.sh` (State Bucket + Dedicated CD Service Account)

Preview the commands first with `--dry-run`, then execute without `--dry-run` (include `--bootstrap-storage-admin` before Pass 1 so the CD service account can create `<project_id>-<hub_name>-artifacts`; you will revoke the unconditioned storage binding right after Pass 1):

```bash
deploy/cd/scripts/setup-cd.sh \
  --project <project_id> \
  --region us-central1 \
  --shared-prefix cdha \
  --hub-name cdha-gke-1 \
  --channel preview \
  --bootstrap-storage-admin \
  --dry-run
```

`setup-cd.sh` creates `gs://<project_id>-<shared_prefix>-tfstate` with **Uniform Bucket-Level Access**, **Object Versioning**, and **Public Access Prevention** enforced, and creates the dedicated `<shared_prefix>-cd-deployer` service account.

### 1.2 Apply `configurations/shared-infra` (One-Time Admin Step)

Copy `deploy/terraform/configurations/shared-infra/terraform.tfvars.example` to a local gitignored `shared-infra.tfvars` (`name_prefix = "cdha"`, `sql_availability_type = "REGIONAL"` for production HA automatic failover, or `"ZONAL"` for lower-cost evaluation):

```bash
terraform -chdir=deploy/terraform/configurations/shared-infra init \
  -backend-config="bucket=<project_id>-cdha-tfstate" \
  -backend-config="prefix=cdha/shared"

terraform -chdir=deploy/terraform/configurations/shared-infra plan \
  -var-file=/path/to/shared-infra.tfvars \
  -out=/tmp/shared-infra.tfplan

terraform -chdir=deploy/terraform/configurations/shared-infra apply /tmp/shared-infra.tfplan
rm -f /tmp/shared-infra.tfplan
```

> **Plan output note:** On projects where GCP APIs are already enabled, `shared-infra` still plans `12` `google_project_service` additions on first apply. These are harmless state registrations (`disable_on_destroy = false`).
>
> **GKE version requirement:** Confirm the created `<prefix>-agents` Autopilot cluster control plane runs **GKE `1.36.2-gke.3104000` or later** (required for empty-zone standalone NEG pre-provisioning; `hub-gke` enforces this via a plan postcondition).

After `shared-infra` creates the `<shared_prefix>-scion` Artifact Registry repository, re-run `setup-cd.sh` once so it binds `roles/artifactregistry.writer` directly on `<shared_prefix>-scion`.

---

## Step 2: Least-Privilege Dedicated CD Service Account IAM

**Never grant admin roles to the default Cloud Build service account (`<project_number>@cloudbuild.gserviceaccount.com`)**—any unrelated trigger or `gcloud builds submit` in the project would inherit cluster and IAM admin privileges. Always attach a dedicated CD service account (`<prefix>-cd-deployer@<project>.iam.gserviceaccount.com`) to the CD trigger (`cloudbuild-gke-cd.yaml` sets `options.logging: CLOUD_LOGGING_ONLY` as required by Cloud Build for user-specified service accounts).

`setup-cd.sh` configures the following steady-state role set:

| Scope | IAM Role | IAM Condition / Scoping | Operations Covered |
|---|---|---|---|
| `gs://<project>-<prefix>-tfstate` | `roles/storage.objectAdmin` | **Bucket-scoped** (not project-wide) | Read/write/lock `<prefix>/hubs/<hub>/default.tfstate`, read `hub.tfvars`, write `deployed.json`. |
| `<prefix>-scion` AR Repo | `roles/artifactregistry.writer` | **Repository-scoped** | Push/tag `scion-hub-gke`, `core-base`, `scion-base`, and `scion-*` harness images; read tags/digests. |
| Project | `roles/secretmanager.admin` | **Condition:** `resource.name.startsWith("projects/<number>/secrets/<hub_name>-")` | Create and manage `<hub>-db-password` & `<hub>-db-dsn` secrets and per-secret IAM without reaching other hubs' secrets or `scion-<hash>-*` signing keys (`secrets.create` evaluates `resource.name` against the new child secret path; empirically verified in `deploy/terraform/modules/hub-identity/main.tf`). |
| Project | `roles/storage.admin` | **Condition:** `resource.name.startsWith("projects/_/buckets/<project>-<hub_name>-")` *(steady-state)* | Manage existing `<project>-<hub>-artifacts` GCS bucket and its IAM without touching unrelated project buckets. *(Note: `storage.buckets.create` in Pass 1 evaluates on the parent project resource—pass `--bootstrap-storage-admin` before Pass 1 and `--revoke-bootstrap-storage-admin` right after Pass 1 creates the bucket.)* |
| Project | `roles/container.admin` | Project | Read `<prefix>-agents` cluster and authenticate `kubernetes` + `helm` providers for `<hub>-system` and `<hub>` namespaces, PVs, RBAC, and `helm_release.hub`. |
| Project | `roles/cloudsql.admin` | Project | Read `<prefix>-pg` and manage `google_sql_database` + `google_sql_user` for `<hub>`. |
| Project | `roles/iam.serviceAccountAdmin` | Project | Manage `<hub>-{hub,transport,agent}` GSAs and Workload Identity / token-creator bindings. |
| Project | `roles/resourcemanager.projectIamAdmin` | Project | Manage additive `google_project_iam_member` bindings in `modules/hub-identity`. |
| Project | `roles/compute.loadBalancerAdmin` | Project | Manage `hub-lb` global IP, managed SSL cert, health check, backend service, URL maps, proxies, forwarding rules, and read zonal NEGs. |
| Project | `roles/compute.securityAdmin` | Project | Manage `<hub>-hub-allow-lb-hc` health-check firewall rule and SSL certs. |
| Project | `roles/compute.networkViewer` | Project | Read `<prefix>-vpc` and `<prefix>-subnet` via `shared-lookup`. |
| Project | `roles/file.viewer` | Project | Read `<prefix>-nfs` Filestore IP via `shared-lookup`. |
| Project | `roles/iap.admin` | Project | Enable IAP on `<hub>-hub-backend` and manage `iap_members` bindings. |
| Project | `roles/logging.logWriter` | Project | Stream Cloud Build logs to Cloud Logging (`CLOUD_LOGGING_ONLY`). |

> **Security note on `projectIamAdmin` + `serviceAccountAdmin`:** Because `modules/hub-identity` manages per-hub GSAs and project IAM bindings in Terraform, the dedicated CD service account holds `roles/resourcemanager.projectIamAdmin` and `roles/iam.serviceAccountAdmin`, making it privilege-equivalent to a project administrator. Restrict `cloudbuild.builds.create` / trigger edit permissions in the project and protect the trigger's source branch (`--trigger-branch`, default `main`) with required reviews.

*(Note: `roles/serviceusage.serviceUsageAdmin`, `roles/compute.networkAdmin`, `roles/servicenetworking.networksAdmin`, and `roles/file.editor` are needed **only** during Step 1.2 `shared-infra` bootstrap—never on the daily CD service account.)*

---

## Step 3: Upload `hub.tfvars` & Two-Pass `iap_oauth_client_id` Bootstrap

1. **Ensure an OAuth Brand (consent screen) exists in the project:**
   GCP allows one OAuth brand per project (**Google Auth Platform -> Overview / Branding** in the Cloud Console). Create the internal/external brand once in the Console if the project has never used IAP before.

2. **Prepare and upload `hub.tfvars` to GCS:**
   Copy [`deploy/cd/hub.tfvars.example`](./hub.tfvars.example) to a local gitignored `hub.tfvars`, fill in `project_id`, `region`, `zone`, `shared_prefix`, `hub_name`, `state_prefix`, `hostname`, `iap_members`, and `admin_emails` (**do not set `hub_image_digest`**), and upload it:
   ```bash
   gcloud storage cp hub.tfvars \
     gs://<project_id>-<shared_prefix>-tfstate/<shared_prefix>/hubs/<hub_name>/hub.tfvars
   ```

3. **Two-pass first install (`iap_oauth_client_id`):**
   - **If you already know the project's Google-managed IAP OAuth client ID** (e.g. `<id>.apps.googleusercontent.com` from an existing IAP service in the same project), set `iap_oauth_client_id` in `hub.tfvars` up front—the first CD run installs everything in a single pass (`hub_installed = true`).
   - **Fresh project (general two-pass flow):**
     1. **Pass 1:** Leave `iap_oauth_client_id` commented out (`null`) in `hub.tfvars` and run the CD trigger (having passed `--bootstrap-storage-admin` to `setup-cd.sh` so Pass 1 can create `<project_id>-<hub_name>-artifacts`). The pipeline builds all images, provisions the load balancer + IAP backend + database + artifacts bucket + namespaces, and exits green with `hub_installed = false`. *(Note: if the very first create of `<hub>-neg` races the GKE NEG controller with a zonal NEG `not found` error, simply re-run the trigger—NEGs already exist and image builds will be skipped automatically.)*
     2. **Lock down Storage IAM after Pass 1:** Once `<project_id>-<hub_name>-artifacts` exists, remove temporary unconditioned `roles/storage.admin` so steady-state CD keeps only the bucket-prefix IAM Condition:
        ```bash
        deploy/cd/scripts/setup-cd.sh \
          --project <project_id> \
          --shared-prefix cdha \
          --hub-name cdha-gke-1 \
          --revoke-bootstrap-storage-admin
        ```
     3. **Discover the client ID (read-only):** In the Cloud Console, open **Security -> Identity-Aware Proxy**, locate `<hub_name>-hub-backend`, and copy its OAuth Client ID (`<id>.apps.googleusercontent.com`; also visible under **Google Auth Platform -> Clients**).
     4. **Pass 2:** Uncomment and set `iap_oauth_client_id = "<id>.apps.googleusercontent.com"` in `hub.tfvars`, re-upload it to GCS, and run the CD trigger again. Pass 1's `hub_installed = false` (and the updated `hub.tfvars` SHA-256) prevents no-op short-circuiting, Step 3 skips all already-built images in ~5 seconds, and Step 4 installs the `scion-hub` Helm release (`hub_installed = true`).

   > **Important:** Once `iap_oauth_client_id` is set, never revert it to `null`—doing so plans a destroy of `helm_release.hub`.

---

## Step 4: DNS & Google-Managed TLS Certificate

Terraform allocates a global static IPv4 address (`lb_ip`, protected by `prevent_destroy = true`) and a Google-managed SSL certificate for `hostname`, but creates no DNS records.

> **Why public DNS is mandatory for agents too:** Until `hostname` publicly resolves to `lb_ip`, Google's Certificate Authority keeps the certificate in `PROVISIONING` (`FAILED_NOT_VISIBLE`). GKE-dispatched agent pods connect back to the Hub through `https://<hostname>` presenting IAP OIDC tokens—so both browser access and agent connections require public DNS and an `ACTIVE` certificate.

Read `lb_ip` from the Terraform state or Cloud Build logs (`gcloud compute addresses list --global --filter="name~^<hub_name>-hub"`), then choose one of two DNS setups:

### Option A: External DNS `A` Record or Cloud DNS (Recommended for Production)

Create an `A` record in your authoritative DNS zone mapping `<hostname>` -> `<lb_ip>` (`TTL 300`).

### Option B: Zero-Domain Cloud Endpoints (`*.endpoints.<project_id>.cloud.goog`)

For evaluations, internal environments, or projects without a custom domain registrar, Google provides authoritative public DNS under `<name>.endpoints.<project_id>.cloud.goog`:

1. Set `hostname = "<hub_name>.endpoints.<project_id>.cloud.goog"` in `hub.tfvars` before Pass 1.
2. Once `lb_ip` is allocated, publish a minimal Cloud Endpoints DNS entry (one-time bootstrap step requiring `roles/servicemanagement.admin` + `roles/endpoints.admin` on the operator):
   ```bash
   cat >/tmp/endpoints-dns.yaml <<EOF
   swagger: "2.0"
   info:
     title: "Scion Hub DNS"
     version: "1.0.0"
   host: "<hub_name>.endpoints.<project_id>.cloud.goog"
   x-google-endpoints:
     - name: "<hub_name>.endpoints.<project_id>.cloud.goog"
       target: "<lb_ip>"
   paths: {}
   EOF

   gcloud endpoints services deploy /tmp/endpoints-dns.yaml --project=<project_id>
   ```

Monitor Google-managed certificate provisioning (~10–20 minutes after DNS propagates):

```bash
gcloud compute ssl-certificates list --project=<project_id> --filter="name~^<hub_name>-hub-"
```

---

## Step 5: Cloud Build Trigger, Substitutions & Daily Cloud Scheduler

Pass `--repository` (2nd-gen Cloud Build repository connection) or `--github-owner` + `--github-repo` (1st-gen GitHub App) to `setup-cd.sh` to create both the Cloud Build Trigger (using `^|^` delimiter escaping so multi-platform `_AGENT_PLATFORMS` values parse cleanly) and the daily Cloud Scheduler job (`0 5 * * *` UTC, posting a `RunBuildTriggerRequest` JSON payload—`{"source":{"branchName":"<branch>"}}` for 1st-gen GitHub triggers or `{}` for 2nd-gen `--repository` manual triggers—to `projects.locations.triggers.run`):

```bash
deploy/cd/scripts/setup-cd.sh \
  --project <project_id> \
  --region us-central1 \
  --shared-prefix cdha \
  --hub-name cdha-gke-1 \
  --channel nightly \
  --github-owner GoogleCloudPlatform \
  --github-repo scion
```

### Configurable Substitutions (`deploy/cd/cloudbuild-gke-cd.yaml`)

| Substitution | Default | Description |
|---|---|---|
| `_CHANNEL` | `'nightly'` | Release channel resolved from `LATEST.json` (`nightly`, `preview`, `stable`, or `''` when pinning `_VERSION_TAG`). |
| `_VERSION_TAG` | `''` | Exact release tag override (e.g. `v0.4.0-preview.4`); when non-empty, overrides `_CHANNEL`. |
| `_STATE_BUCKET` | `''` *(required)* | Versioned GCS Terraform state bucket (`<project>-<shared_prefix>-tfstate`). |
| `_SHARED_PREFIX` | `''` *(required)* | `shared-infra` `name_prefix` (`^[a-z][a-z0-9]{1,7}$`). |
| `_HUB_NAME` | `''` *(required)* | Target hub name starting with `<_SHARED_PREFIX>-` (e.g. `cdha-gke-1`). |
| `_REGISTRY` | `''` *(required)* | Artifact Registry Docker path (`<region>-docker.pkg.dev/<project>/<shared_prefix>-scion`, cross-checked against `<_SHARED_PREFIX>-scion` and `hub.tfvars`). |
| `_AGENT_PLATFORMS` | `'linux/amd64'` | Agent image platform(s). Keep `linux/amd64` (~20–30m uncached, ~12–15m with `core-base` cache) for amd64 GKE Autopilot nodes; set `linux/amd64,linux/arm64` (~2–3h QEMU build) only if scheduling arm64 agent pods. **Note:** If adding `linux/arm64` on an already-built `TARGET_TAG`, pass `_FORCE_REDEPLOY=true` once so `:TARGET_TAG` and `:$SHORT_SHA` manifests are rebuilt multi-arch rather than reused by tag name. |
| `_FORCE_APPLY` | `'false'` | Set `'true'` to bypass the Step 1 no-op short-circuit and re-run `terraform apply` while **reusing** already-pushed Artifact Registry images (~1–2m). *(Note: edits to `hub.tfvars` are already detected automatically via `tfvars_sha256` without needing `_FORCE_APPLY`.)* |
| `_FORCE_REDEPLOY` | `'false'` | Set `'true'` to bypass the GCS no-op check **and** rebuild all images from scratch even if tags already exist (e.g. after changing `_AGENT_PLATFORMS`). |
| `_ALLOW_DOWNGRADE` | `'false'` | Set `'true'` to permit deploying a tag older than `deployed.json`'s `version` when the target is at or above `deployed.json`'s `downgrade_floor` (see Rollback notes below). |
| `_ALLOW_BELOW_DOWNGRADE_FLOOR` | `'false'` | Set `'true'` (together with `_ALLOW_DOWNGRADE=true`) **only after restoring Cloud SQL via PITR/backup** to permit deploying a tag below (or unorderable against) `deployed.json`'s schema `downgrade_floor`. |
| `_REPO_URL` | `'https://github.com/GoogleCloudPlatform/scion.git'` | Git repository cloned at `TARGET_TAG` (and used to derive `LATEST.json` on `main`). |

### Running Ad-Hoc Builds, Applying `hub.tfvars` Edits, or Pinning a Version

```bash
# Run the configured channel check / rollout on demand (also applies any uploaded
# hub.tfvars edits such as iap_members, admin_emails, or db_password_rotation,
# reusing existing images in ~5s without rebuilding):
gcloud builds triggers run <hub_name>-cd \
  --region=us-central1 --project=<project_id> --branch=main

# Reconcile Terraform state drift without rebuilding images:
gcloud builds triggers run <hub_name>-cd \
  --region=us-central1 --project=<project_id> --branch=main \
  --substitutions=_FORCE_APPLY=true

# Pin a specific release tag for one run:
gcloud builds triggers run <hub_name>-cd \
  --region=us-central1 --project=<project_id> --branch=main \
  --substitutions=_VERSION_TAG=v0.4.0-preview.4
```

---

## Step 6: One-Time Post-Apply Hub Configuration

Once the Google-managed SSL certificate reaches `ACTIVE` and the Hub is reachable at `https://<hostname>`, authenticate with the Scion CLI as one of the `admin_emails` and complete these one-time database-backed settings (stored in Cloud SQL and preserved automatically across all future CD upgrades):

1. **Configure Vertex AI project & region for agent preflight:**
   ```bash
   scion hub env set --scope hub --always GOOGLE_CLOUD_PROJECT=<project_id>
   scion hub env set --scope hub --always GOOGLE_CLOUD_REGION=<region>
   ```
   Without these hub-scope environment variables, agent launches using GCP identity fail preflight with missing `GOOGLE_CLOUD_PROJECT` / `GOOGLE_CLOUD_LOCATION`.

2. **Set Project / Agent GCP Identity to `Passthrough` & enable Model Garden models:**
   - New projects and agents default **GCP Identity** to **Block** (which blocks the GKE metadata server inside agent pods). In your Scion Project Settings (**Default Service Account / GCP Identity**), switch to **Passthrough** (or **Assign Service Account**) so Workload Identity (`<hub_name>-agent@<project_id>.iam.gserviceaccount.com`, granted `roles/aiplatform.user`) reaches the agent.
   - Enable the LLM models your agents call in **Vertex AI Model Garden** for `<project_id>`.

3. **Optional Hub experiments (`scion hub experiments enable`):**
   - Release-tagged `scion-hub-gke` builds already enable `hub.image_tag_version_default` by default, so bundled `scion-*` agent harness images automatically resolve to `:<TARGET_TAG>`.
   - To opt into any additional server/web experiments (stored as operational DB settings rather than `settings.yaml`), run:
     ```bash
     scion hub experiments list
     scion hub experiments enable <experiment_name>
     ```
   - If you ever need to override `image_tag` manually via settings/profile rather than using the release binary's built-in tag, enable `scion hub experiments enable hub.image_tag_setting` first.

---

## Step 7: Upgrades, Rollback & `DowngradeFloor`

### Daily Steady State & `hub.tfvars` Updates
- On days when `LATEST.json` has not moved for `_CHANNEL` and `hub.tfvars` in GCS is unchanged, Step 1 (`resolve-and-check-noop`) verifies `deployed.json` + `hub.tfvars` (`tfvars_sha256`) + `default.tfstate` convergence and exits green in **under 15 seconds**.
- When an operator uploads an updated `hub.tfvars` (e.g. modifying `iap_members`, `admin_emails`, `enable_http_redirect`, or rotating the database password via `db_password_rotation`), Step 1 automatically detects the `tfvars_sha256` change, Step 3 reuses all already-pushed images and the existing `scion-hub-gke` digest in ~5 seconds, and Step 4 applies the Terraform update and records the new `tfvars_sha256` in `deployed.json`.
- When a new release tag is published, Step 3 reuses `core-base` via its content+platform hash tag (`cache-<sha256>`) whenever `image-build/core-base` and `image-build/lib` did not change, rebuilding only `scion-hub-gke`, `scion-base`, and the catalog harnesses (~12–15 minutes on `linux/amd64`), then rolls the Hub pod via digest update and verifies zero post-apply Terraform drift.

### Rollback & Database `DowngradeFloor` Hazards
Step 1 refuses any `TARGET_TAG` older than `deployed.json`'s `version` unless `_ALLOW_DOWNGRADE=true`, and **additionally hard-fails** any rollback below (or unorderable against) `deployed.json`'s `downgrade_floor` unless `_ALLOW_BELOW_DOWNGRADE_FLOOR=true` is also set. Before forcing a downgrade, check three constraints:

1. **One-way database migrations (`DowngradeFloor`):**
   Scion tracks destructive schema changes in `pkg/version.DowngradeFloor` (recorded in `deployed.json` as `downgrade_floor` on each deploy). Rolling `scion-hub-gke` back via Terraform bypasses in-hub binary update guards and boots an older Hub binary directly against the newer Cloud SQL schema. Routine rollbacks to a tag **at or above** `downgrade_floor` require only `_ALLOW_DOWNGRADE=true`. **Never set `_ALLOW_BELOW_DOWNGRADE_FLOOR=true` to roll below `downgrade_floor` without first restoring Cloud SQL (Point-in-Time Recovery / backup) to a pre-upgrade snapshot.**
2. **Terraform resource drift across tags:**
   Because Step 4 runs `plan` and `apply` back-to-back from the checked-out `TARGET_TAG`, rolling backward to an older tag whose Terraform modules lack resources added in a newer release will plan and apply **destroys** of those resources in the same step. Always check out the older tag locally and run `terraform plan` manually to inspect the diff before triggering a cross-minor rollback in Cloud Build.
3. **`nightly-YYYYMMDD` 14-day tag retention:**
   `nightly-*` Git tags are pruned from GitHub after **14 days**. For long-lived production stability or reproducible rollbacks beyond 14 days, track `_CHANNEL=preview` or `_CHANNEL=stable` (whose `vX.Y.Z*` Git tags are permanent), and keep Artifact Registry images in your project's `<prefix>-scion` repository.
