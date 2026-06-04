# Scion Hub — Cloud Run Deployment

Deploys the Scion hub as a single Cloud Run instance with a co-located GKE
broker targeting `scion-demo-cluster`.

## Architecture

```
Cloud Run (min=max=1)
┌──────────────────────────┐
│  scion server (combo)    │
│  ├─ Hub API   :8080      │
│  ├─ Web UI    :8080      │
│  └─ Broker    :9810      │──▶ GKE Autopilot (scion-demo-cluster)
│     SQLite: /tmp/scion.db│       namespace: scion-agents
└──────────────────────────┘
```

- **Authenticated HTTPS only** (`--no-allow-unauthenticated`)
- **SQLite (ephemeral)** — lost on instance restart, acceptable for demo
- **GKE auth via ADC** — Cloud Run service account → Workload Identity → GKE

## Prerequisites

- `gcloud` CLI, authenticated with project `deploy-demo-test`
- `docker` CLI, authenticated to Artifact Registry
- `kubectl` with access to `scion-demo-cluster` (for namespace creation only)
- `openssl` (for session secret generation)

## Quick Start

```bash
# Full deploy (build + push + secrets + Cloud Run service)
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
2. Builds and pushes the container image to Artifact Registry
3. Fetches GKE cluster endpoint + CA cert and generates a kubeconfig
4. Generates hub settings from the template (injects session secret)
5. Stores kubeconfig and settings as Secret Manager secrets
6. Ensures the `scion-agents` namespace exists in GKE
7. Deploys the Cloud Run service with secrets mounted as files

## Verification

```bash
# Get the service URL
URL=$(gcloud run services describe scion-hub \
  --region us-central1 --project deploy-demo-test \
  --format="value(status.url)")

# Health check (requires IAM authentication)
curl -H "Authorization: Bearer $(gcloud auth print-identity-token)" "${URL}/healthz"

# Point the scion CLI at the Cloud Run hub
scion hub set --url "${URL}" --auth gcloud
```

## Files

| File                          | Purpose                                     |
|-------------------------------|---------------------------------------------|
| `Dockerfile`                  | Multi-stage build: web + Go → slim runtime  |
| `deploy.sh`                   | End-to-end deploy script                    |
| `hub-settings-template.yaml`  | Hub settings (session secret placeholder)   |
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
