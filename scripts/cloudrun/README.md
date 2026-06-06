# Scion Hub — Cloud Run Deployment

Deploys the Scion hub as a single Cloud Run instance with IAP authentication
and a co-located GKE broker targeting `scion-demo-cluster`.

## Architecture

```
User → Cloud Run (built-in IAP) → Hub container
┌──────────────────────────┐
│  scion server (combo)    │
│  ├─ Hub API   :8080      │
│  ├─ Web UI    :8080      │
│  └─ Broker    :9810      │──▶ GKE Autopilot (scion-demo-cluster)
│     SQLite: /tmp/scion.db│       namespace: scion-agents
└──────────────────────────┘
```

- **IAP-protected** — Cloud Run's native IAP integration secures all ingress
  paths (including the default `*.run.app` URL). No load balancer required.
- **Authenticated HTTPS only** (`--no-allow-unauthenticated`)
- **SQLite (ephemeral)** — lost on instance restart, acceptable for demo
- **GKE auth via ADC** — Cloud Run service account → Workload Identity → GKE

## Prerequisites

- `gcloud` CLI, authenticated with project `deploy-demo-test`
- `docker` CLI, authenticated to Artifact Registry
- `kubectl` with access to `scion-demo-cluster` (for namespace creation only)
- `openssl` (for session secret generation)
- IAP API enabled (`gcloud services enable iap.googleapis.com`)

## Quick Start

```bash
# Full deploy (build + push + secrets + Cloud Run + IAP)
./scripts/cloudrun/deploy.sh

# Redeploy without rebuilding the image
./scripts/cloudrun/deploy.sh --skip-build
```

## Configuration

Environment variables override defaults:

| Variable               | Default              | Description                     |
|------------------------|----------------------|---------------------------------|
| `SCION_PROJECT`        | `deploy-demo-test`   | GCP project ID                  |
| `SCION_REGION`         | `us-central1`        | GCP region                      |
| `SCION_SERVICE`        | `scion-hub`          | Cloud Run service name          |
| `SCION_GKE_CLUSTER`    | `scion-demo-cluster` | Target GKE cluster              |
| `SCION_SA_NAME`        | `scion-hub-sa`       | Service account name            |
| `SCION_REPO`           | `scion`              | Artifact Registry repo name     |
| `SCION_SESSION_SECRET` | *(auto-generated)*   | JWT session secret (hex string) |

## What the Deploy Script Does

1. Creates a dedicated service account with `container.admin` and
   `secretmanager.secretAccessor` roles (if it doesn't exist)
2. Creates a transport service account for agent IAP traversal (Phase 2)
3. Builds and pushes the container image to Artifact Registry
4. Fetches GKE cluster endpoint + CA cert and generates a kubeconfig
5. Computes the IAP audience (`/projects/NUM/locations/REGION/services/NAME`)
6. Generates hub settings from the template (injects session secret, IAP audience)
7. Stores kubeconfig and settings as Secret Manager secrets
8. Ensures the `scion-agents` namespace exists in GKE
9. Deploys the Cloud Run service with `--iap` flag and secrets mounted as files
10. Grants the IAP service agent `roles/run.invoker` on the service
11. Grants the transport SA `roles/iap.httpsResourceAccessor` for agent callbacks

## Verification

```bash
# Get the service URL
URL=$(gcloud run services describe scion-hub \
  --region us-central1 --project deploy-demo-test \
  --format="value(status.url)")

# Verify IAP is enabled
gcloud run services describe scion-hub \
  --region us-central1 --project deploy-demo-test \
  | grep "Iap Enabled"

# Direct health check (bypasses IAP via identity token)
curl -H "Authorization: Bearer $(gcloud auth print-identity-token)" "${URL}/health"

# Visit the service URL in a browser — should redirect to Google sign-in
```

## Files

| File                          | Purpose                                     |
|-------------------------------|---------------------------------------------|
| `Dockerfile`                  | Multi-stage build: web + Go → slim runtime  |
| `deploy.sh`                   | End-to-end deploy script                    |
| `hub-settings-template.yaml`  | Hub settings (IAP audience, transport SA)   |
| `README.md`                   | This file                                   |

## Notes

- The Cloud Run instance uses `--timeout 3600` for long-lived WebSocket
  connections from agent control channels.
- `--min-instances 1` keeps the instance warm. SQLite state is lost on cold
  starts, so a warm instance is critical.
- The `gke-gcloud-auth-plugin` is installed in the image for robustness, but
  `pkg/k8s/client.go` also has a `fallbackToGCEAuth()` path that uses ADC
  directly if the plugin fails.
- Session secret is stored in Secret Manager and injected into settings at
  deploy time, so it survives instance restarts.
- Cloud Run's native IAP protects all ingress paths without a load balancer,
  managed cert, or static IP. The `*.run.app` URL is directly IAP-protected.
- Agent IAP traversal (transport SA) requires Phase 2 transport token code
  which is not yet merged. The infrastructure and IAM bindings are in place.
