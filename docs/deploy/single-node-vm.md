# Single-Node VM Deployment Guide

Deploy a Scion Hub on a Google Compute Engine (GCE) VM with Identity-Aware Proxy
(IAP) authentication. This tier uses released binaries, local storage, and a
Cloud Run IAP reverse proxy — no container builds, no GCS, no external database.

> Part of the Single-Node-VM deploy tier (ptone/scion#1575).

## Overview

The single-node-VM tier stands up a persistent GCE VM running the `scion` binary
from a GitHub Release. A Cloud Run service acts as an IAP-authenticated reverse
proxy, giving users browser access without exposing the VM directly. Agents
running on the VM connect to the Hub over localhost, bypassing IAP entirely.

Key properties:

- **Binary-based** — downloads a pre-built release; no source checkout or
  container build required.
- **Local storage** — Hub state lives in embedded SQLite on the VM disk; no GCS
  or Cloud SQL.
- **Local secrets** — secrets are stored on disk (`hub.env`); no Secret Manager.
- **IAP auth** — users authenticate via Google IAP through the Cloud Run proxy.
- **Idempotent** — re-running the deploy script converges without duplication.

## Prerequisites

| Requirement | Details |
|-------------|---------|
| GCP project | A Google Cloud project with billing enabled |
| `gcloud` CLI | Authenticated (`gcloud auth login`) with a project set (`gcloud config set project PROJECT_ID`) |
| Required APIs | `compute`, `run`, `iap`, `cloudbuild`, `artifactregistry` — enabled automatically by the script |
| Permissions | Project Editor or equivalent (create VMs, Cloud Run services, service accounts, IAM bindings). The script also grants `roles/iap.tunnelResourceAccessor` to the deployer for SSH access to the private VM. |

## Quick Start

From the repository root:

```bash
./scripts/single-node-vm/deploy.sh
```

The script runs interactively, prompting for:

1. **Hub name** (default: `my-hub`) — used to derive resource names
2. **GCP region** (default: `us-central1`)
3. **Machine size** — Small (e2-standard-4) or Medium (n2-standard-16)
4. **Disk size** — 200 GB, 500 GB, or custom
5. **Chat plugins** — Telegram, Discord, Slack, Teams, or none
6. **Container images** — provide a registry path or build locally on the VM
7. **Admin email** — email address to grant super-admin access (defaults to deployer identity)

To install a specific release version:

```bash
./scripts/single-node-vm/deploy.sh --version v0.5.0
```

## Architecture

```
                 ┌─────────────────────────────┐
                 │         GCP Project          │
                 │                              │
  User ──────►  │  Cloud Run IAP Proxy ──────► │ ──► GCE VM (no public IP)
  (browser)     │  (IAP-authenticated)         │     ┌──────────────────┐
                 │                              │     │ scion binary     │
                 │                              │     │ systemd service  │
                 │                              │     │ SQLite + local   │
                 │                              │     │ storage          │
                 │                              │     │                  │
                 │                              │     │ Agents ──► localhost:8080
                 │                              │     └──────────────────┘
                 └─────────────────────────────┘
```

- **User (browser)** — connects to the Cloud Run proxy URL; Google IAP
  authenticates the request before it reaches the proxy.
- **Cloud Run IAP Proxy** — a lightweight reverse proxy deployed from
  `extras/cloudrun-iap-proxy`. Forwards authenticated requests to the VM's
  internal IP on port 8080.
- **GCE VM** — runs the Scion Hub via a systemd unit (`scion-hub.service`)
  with the runtime broker co-located in the same process (`--enable-runtime-broker`).
  Has no public IP address. Sits on the default VPC so the Cloud Run proxy
  can reach it via internal networking. Docker is installed by cloud-init.
- **Agents** — launched as Docker containers on the VM by the runtime broker;
  they connect to the Hub at `localhost:8080`, no IAP needed.

## What Gets Created

The deploy script creates the following GCP resources:

| Resource | Name pattern | Purpose |
|----------|-------------|---------|
| GCE VM | `scion-hub-<hub-name>` | Runs the Scion Hub binary via systemd |
| Service account | `scion-hub-<hub-name>@<project>.iam.gserviceaccount.com` | VM identity with logging/monitoring roles |
| Cloud Run service | `scion-hub-<hub-name>-iap-proxy` | IAP-authenticated reverse proxy to the VM |
| IAM bindings | IAP `httpsResourceAccessor` for the deployer | Grants the deployer browser access through IAP |

On the VM itself:

| Path | Purpose |
|------|---------|
| `/usr/local/bin/scion` | The Scion Hub binary |
| `/home/scion/.scion/settings.yaml` | Hub configuration (auth mode, storage, etc.) |
| `/home/scion/.scion/hub.env` | Environment variables (session secret, project ID) |
| `/home/scion/.scion/workspace-storage/` | Local workspace storage directory |
| `/home/scion/.scion/plugins/broker/` | Chat plugin binaries (if selected) |
| `/etc/systemd/system/scion-hub.service` | systemd unit file |

## Configuration

### settings.yaml

The Hub configuration file. The deploy script writes it in two stages:

1. **During VM setup (Phase 3)** — `auth.mode: dev` for initial health checks
   via SSH tunnel.
2. **After IAP is ready (Phase 5)** — `auth.mode: proxy` with the IAP audience
   string, enabling IAP-based authentication.

Final configuration:

```yaml
schema_version: "1"
image_registry: "localhost/scion"
server:
  hub:
    name: "my-hub"
    admin_emails:
      - "you@example.com"
  storage:
    local_path: /home/scion/.scion/workspace-storage
  secrets:
    backend: local
  auth:
    mode: proxy
    proxy:
      provider: iap
      iap:
        audience: "/projects/PROJECT_NUMBER/locations/REGION/services/SERVICE"
  listen_port: 8080
```

Key settings:

- `schema_version` — must be `"1"` (not `settings_version`).
- `image_registry` — required, even for locally built images. Set to
  `localhost/scion` for local builds, or the registry path for remote images.
- `admin_emails` — email(s) auto-promoted to super-admin on login.
- `storage.local_path` — all workspace data stored on the VM's local disk.
- `secrets.backend: local` — secrets are read from `hub.env` on disk, not from
  GCP Secret Manager.
- `auth.mode: proxy` — the Hub trusts the IAP proxy header for user identity.
- `auth.proxy.iap.audience` — the IAP audience string, computed automatically
  from the project number, region, and proxy service name.

### hub.env

Environment variables for the Hub process, stored at
`/home/scion/.scion/hub.env`:

```bash
SESSION_SECRET=<randomly-generated-base64>
SCION_GCP_PROJECT_ID=<your-project-id>
GOOGLE_CLOUD_PROJECT=<your-project-id>
```

The `SESSION_SECRET` is generated once during initial deployment and preserved
on subsequent runs.

## Chat Plugins

During deployment, the script prompts you to select chat integrations:

| Option | Plugin | Binary installed |
|--------|--------|-----------------|
| 1 | Telegram | `scion-plugin-telegram` |
| 2 | Discord | `scion-plugin-discord` |
| 3 | Slack | `scion-plugin-slack` |
| 4 | Teams | `scion-plugin-teams` |
| 5 | None | (no plugins) |

You can select multiple plugins by entering comma-separated numbers (e.g.,
`1,2` for Telegram and Discord).

Plugins are downloaded from the same GitHub Release as the main binary and
installed to `/home/scion/.scion/plugins/broker/`. Each plugin runs as a
broker plugin within the Hub process.

## Container Images

Agents need container images to run (core-base, scion-base, and harness images
like scion-antigravity). The deploy wizard offers two options:

### Option 1: Provide a registry path

If you have already pushed images to a container registry (e.g., Artifact
Registry), select this option and provide the registry path. The Hub will
pull images from the registry at runtime.

Example registry path: `us-docker.pkg.dev/my-project/scion`

### Option 2: Build images locally on the VM

This option clones the Scion source repository onto the VM and builds the
minimal set of images needed for deployment: core-base, scion-base, and
scion-antigravity (the default harness).

The build runs in the background on the VM (via `nohup`) so it survives
SSH disconnects. The deploy script polls for completion and shows progress.

**Requirements:**
- Approximately 30-45 minutes of build time
- Approximately 30 GB of additional disk space
- The VM must have outbound internet access (provided by Cloud NAT)

Images are tagged under the `localhost/scion` prefix (e.g.,
`localhost/scion/scion-antigravity:latest`) and `image_registry` is set to
`localhost/scion` in the Hub configuration. The runtime broker resolves
images through this registry prefix.

## Access Patterns

### Web access (browser via IAP)

Open the Cloud Run proxy URL printed at the end of deployment:

```
https://scion-hub-my-hub-iap-proxy-HASH-REGION.a.run.app
```

You will be prompted to authenticate with your Google account. IAP enforces
access — only users with the `roles/iap.httpsResourceAccessor` binding on the
proxy service can reach the Hub.

To grant additional users access:

```bash
gcloud iap web add-iam-policy-binding \
  --resource-type=cloud-run \
  --service=scion-hub-my-hub-iap-proxy \
  --region=us-central1 \
  --project=PROJECT_ID \
  --member=user:colleague@example.com \
  --role=roles/iap.httpsResourceAccessor
```

### Agent access (localhost on the VM)

Agents running on the VM connect directly to `localhost:8080`. No IAP
authentication is needed — the agents are co-located with the Hub.

### CLI access (via IAP)

To use the `scion` CLI from your local machine against the deployed Hub,
configure it to authenticate through IAP using the Cloud Run proxy URL.

### SSH access (via gcloud)

Since the VM has no public IP, use `gcloud compute ssh` to connect:

```bash
gcloud compute ssh scion-hub-my-hub \
  --zone=us-central1-b \
  --project=PROJECT_ID
```

To view Hub logs:

```bash
gcloud compute ssh scion-hub-my-hub \
  --zone=us-central1-b \
  --project=PROJECT_ID \
  --command='sudo journalctl -u scion-hub.service -f'
```

## Teardown

To delete all resources created by a previous deployment:

```bash
./scripts/single-node-vm/deploy.sh --delete
```

The teardown flow prompts for the hub name and region, then deletes:

1. The Cloud Run IAP proxy service
2. The GCE VM instance
3. The service account

**Note:** IAP access bindings are scoped to the region, not to the service.
Teardown does not remove them. To clean up IAP bindings manually:

```bash
gcloud iap web get-iam-policy \
  --project=PROJECT_ID \
  --region=us-central1 \
  --resource-type=cloud-run
```

## Troubleshooting

### Health check fails after deployment

The Hub takes a few seconds to start. If the health check does not pass within
60 seconds, check the service logs:

```bash
gcloud compute ssh scion-hub-my-hub \
  --zone=us-central1-b \
  --project=PROJECT_ID \
  --command='sudo journalctl -u scion-hub.service --no-pager -n 50'
```

Common causes:
- Cloud-init did not finish (Docker or scion user not yet created)
- Port conflict on 8080
- Invalid `hub.env` or `settings.yaml`

### Cannot reach the Cloud Run proxy URL

- **403 Forbidden** — you do not have the IAP access binding. Run the
  `gcloud iap web add-iam-policy-binding` command above.
- **IAP not yet enforcing** — IAP takes 30-60 seconds to activate after being
  enabled. The deploy script waits 60 seconds, but in rare cases it may take
  longer.
- **VPC connectivity** — the Cloud Run proxy must be on the same VPC as the VM.
  The script deploys with `--network=default --subnet=default --vpc-egress=all-traffic`.

### Cloud-init takes too long

Cloud-init installs Docker and creates the `scion` user. The script waits up to
5 minutes. If it times out, the VM may still be provisioning. SSH into the VM
and check:

```bash
gcloud compute ssh scion-hub-my-hub \
  --zone=us-central1-b \
  --project=PROJECT_ID \
  --command='tail -20 /var/log/cloud-init-output.log'
```

### Hub starts but IAP authentication does not work

After the IAP proxy is deployed, the script updates `settings.yaml` from
`auth.mode: dev` to `auth.mode: proxy` and restarts the Hub. If authentication
is not working:

1. Verify the settings on the VM:
   ```bash
   gcloud compute ssh scion-hub-my-hub \
     --zone=us-central1-b \
     --project=PROJECT_ID \
     --command='cat /home/scion/.scion/settings.yaml'
   ```
2. Confirm `auth.mode` is `proxy` and the `audience` string matches the
   project number and service name.
3. Check that the Hub restarted successfully after the settings update.

### Re-running the deploy script

The script is idempotent. Re-running it will:
- Skip creating the VM and service account if they already exist
- Preserve the existing `hub.env` (and its `SESSION_SECRET`)
- Overwrite `settings.yaml` with the current configuration
- Re-deploy the Cloud Run proxy (converges to the same state)
