---
title: Deploy via Helm (GKE)
description: Complete guide to deploying the Scion HA Hub to Google Kubernetes Engine (GKE) using the official Helm chart.
---

Scion supports deploying the **Scion HA Hub** directly to Google Kubernetes Engine (GKE) using the official Scion Helm chart. This Helm-based deployment (introduced in Phase 0) provides a robust, standardized path for provisioning and upgrading the control plane in highly available enterprise environments.

:::note[Availability Tier]
Helm-based deployment is designed for **HA hosted** mode, utilizing external managed services (such as Cloud SQL PostgreSQL and Google Cloud Storage) to ensure multi-replica scalability, zero-downtime rolling upgrades, and cluster-wide consistency.
:::

---

## Key Features & Safeguards

The Scion Helm chart incorporates several operator-facing validation checks and safety systems built specifically for Kubernetes production workloads:

### 1. Robust Schema Validation
The chart includes a complete `values.schema.json` file. Helm validates your configuration at install/upgrade time against this schema, catching syntax errors, missing fields, or invalid types before sending resources to the Kubernetes API.

### 2. Guardrails Against Unsupported Shapes
To prevent runtime configuration mismatches or silent data loss, the Helm templates implement **16 operator-facing validation claims**. If your configuration defines an unsupported architectural shape (e.g., mismatching storage backends, invalid database configurations, or invalid auth modes), the chart will actively refuse to render, printing a clear explanation of the requirement.

### 3. Stable `hub.hubId` Across Upgrades
The Hub's identity (`hub.hubId`) remains completely stable and immutable across Helm upgrades and container rescheduling. This avoids identity loss, prevents JWT signature validation issues, and ensures that active agent communication is never disrupted during platform maintenance.

### 4. Re-Engineered `/readyz` Multi-Backend Probe
The Hub exposes a detailed readiness endpoint at `/readyz` targeted by GKE readiness probes. This endpoint handles check-resolution for all configured backend services, including the newly supported `gke-shared-volume` workspace backend. If any critical dependency (database, object storage, or shared volumes) becomes unreachable, the pod is automatically marked unready to prevent bad traffic routing.

### 5. Deterministic Cluster-Scoped RBAC Names
When deploying Scion in multi-tenant or multi-namespace clusters, name collisions can present severe security risks.
- **The Challenge:** Kubernetes `ClusterRole` and `ClusterRoleBinding` resources must have cluster-unique names. In large environments, long generated names can exceed the 63-character limit and truncate. Truncated names can collide across namespaces, potentially repointing critical `pods/exec` and `secrets` authority to the wrong workspace.
- **The Fix:** The Scion Helm chart automatically disambiguates truncated cluster-scoped resource names. If a name truncates to 63 bytes, it appends a short cryptographic digest over the fullname and namespace. This guarantees unique cluster-scoped bindings even under aggressive truncation.

---

## 1. Prerequisites

Before installing the chart, ensure you have:

1. A running GKE cluster (Standard or Autopilot).
2. `kubectl` and `helm` (v3+) configured with access to the cluster.
3. An external PostgreSQL database (e.g., GCP Cloud SQL).
4. A Google Cloud Storage (GCS) bucket for storing templates and workspace artifacts.
5. Workload Identity configured on GKE to allow Hub replicas to access GCP resources (GCS, Secret Manager, Cloud SQL) using ambient service account credentials.

---

## 2. Configuration (`values.yaml`)

Create a custom `values.yaml` file to configure your deployment. Below is a production-grade template:

```yaml
# values.yaml

image:
  repository: us-central1-docker.pkg.dev/your-project/scion/hub
  tag: latest
  pullPolicy: IfNotPresent

replicaCount: 2

# Core Hub Configuration
hub:
  publicUrl: "https://hub.scion.example.com"
  sessionSecret: "your-long-secure-session-secret"

# Database Configuration
database:
  driver: postgres
  # In GKE, connect via the Cloud SQL Auth Proxy sidecar or explicit connection string
  url: "postgres://scion:DB_PASSWORD@/scionhub?host=/cloudsql/PROJECT_ID:REGION:scion-hub-db"

# Storage Backend (GCS)
storage:
  provider: gcs
  bucket: "scion-hub-your-project"

# Secrets Backend (Google Cloud Secret Manager)
secrets:
  backend: gcpsm
  gcpProjectId: "your-project-id"

# Workspace Backend Configuration
workspaceStorage:
  backend: gke-shared-volume  # Fully supported by the Hub readiness check

# GKE Service Account Annotations (Workload Identity)
serviceAccount:
  create: true
  name: scion-hub-runner
  annotations:
    iam.gke.io/gcp-service-account: "scion-hub-runner@your-project-id.iam.gserviceaccount.com"
```

---

## 3. Installation & Upgrades

To install or upgrade the Scion Hub in your cluster:

```bash
# Add the Scion Helm repository (or run from local chart directory)
helm repo add scion https://googlecloudplatform.github.io/scion
helm repo update

# Install the chart
helm install scion-hub scion/scion-hub \
  --namespace scion-system \
  --create-namespace \
  -f values.yaml
```

### Dry-Run Template Rendering
To inspect the rendered Kubernetes manifests and run the template-level validation assertions without modifying cluster state:

```bash
helm template scion-hub scion/scion-hub \
  --namespace scion-system \
  -f values.yaml \
  --debug
```

---

## 4. Verification

The Helm chart includes a built-in **verification suite** to check the deployment's integrity. To run the verification tests:

```bash
helm test scion-hub --namespace scion-system
```

This verification suite asserts:
- Successful database schema migrations.
- `/readyz` readiness endpoints are reporting healthy.
- Direct connectivity to GCS and Secret Manager.
- The RBAC role and binding allocations are correctly applied to the namespace.

---

## 5. Troubleshooting & Diagnostics

If your deployment fails to start or GKE probes report unhealthy:

### 1. Readiness Failures (503 on `/readyz`)
If the readiness probe fails, check the pod logs:
```bash
kubectl logs -n scion-system -l app.kubernetes.io/name=scion-hub -c hub
```
The `/readyz` probe performs real-time checks on the `gke-shared-volume` backend and database. Look for error logs prefixed with `readiness_check_failed` or `volume_mount_failed`.

### 2. Preflight Placeholder Warnings
If your OAuth or IAP client ID is not yet provisioned during GKE's two-step bootstrap flow, you can temporarily deploy with placeholder audience strings. The Hub will emit a warning at startup indicating that the audience looks like a placeholder, but it will allow the boot sequence to complete so that GKE services can initialize and acquire their stable endpoints:
```
Warning: IAP audience "/projects/1234/global/backendServices/placeholder" appears to be a synthetic bootstrap placeholder. Correct this value once GKE load balancer provisioning completes.
```
Once your cluster's GCLB is fully provisioned, update `values.yaml` with the real audience and run a `helm upgrade`.
