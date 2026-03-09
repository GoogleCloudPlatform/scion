# Starter Hub — GCE Demo Deployment

Scripts for provisioning and operating a Scion Hub on a Google Compute Engine VM.

## Prerequisites

- Google Cloud SDK (`gcloud`) authenticated with a project
- GitHub CLI (`gh`) authenticated (for deploy key setup)
- A registered domain with DNS delegated to Cloud DNS (see `gce-certs.sh`)

## Environment Setup

1. Copy the sample env file and fill in your values:

   ```bash
   mkdir -p .scratch
   cp scripts/starter-hub/hub.env.sample .scratch/hub.env
   # Edit .scratch/hub.env with your secrets
   ```

2. Configure OAuth credentials for Google and/or GitHub. See the
   [Authentication & Identity docs](https://scion-ai.dev/hub-admin/auth/)
   for how to create OAuth client IDs and secrets for both web and CLI flows.

## Initial Provision (one-time)

Run the all-in-one deploy script from the repo root:

```bash
./scripts/starter-hub/gce-demo-deploy.sh
```

This runs four steps in sequence:

| Step | Script | What it does |
|------|--------|-------------|
| 1 | `gce-demo-provision.sh` | Creates GCE VM, service account, firewall rules, and (optionally) a GKE cluster |
| 2 | `gce-demo-telemetry-sa.sh` | Creates a least-privilege GCP service account for agent telemetry export |
| 3 | `gce-demo-setup-repo.sh` | Generates an SSH deploy key, registers it on GitHub, and clones the repo on the VM |
| 4 | `gce-start-hub.sh --full` | Uploads config, builds the binary & web assets, installs Caddy + systemd, and starts the hub |

After provisioning, set up DNS and TLS certificates:

```bash
./scripts/starter-hub/gce-certs.sh
```

## Redeploying / Restarting

To push the latest code and restart the hub (fast path — no config changes):

```bash
./scripts/starter-hub/gce-start-hub.sh
```

To also re-upload config files, update systemd/Caddy, and refresh GKE credentials:

```bash
./scripts/starter-hub/gce-start-hub.sh --full
```

To wipe the hub database on restart:

```bash
./scripts/starter-hub/gce-start-hub.sh --reset-db
```

## Teardown

```bash
./scripts/starter-hub/gce-demo-provision.sh delete
```

This removes the VM, service account, firewall rules, GKE cluster (if created), and
the telemetry service account.

## Script Reference

| Script | Purpose |
|--------|---------|
| `gce-demo-deploy.sh` | All-in-one initial deployment (runs the four scripts below) |
| `gce-demo-provision.sh` | Provision/delete GCE VM and GCP resources |
| `gce-demo-cluster.sh` | Create/delete GKE Autopilot cluster for agent workloads |
| `gce-demo-setup-repo.sh` | SSH deploy key + repo clone on the VM |
| `gce-demo-telemetry-sa.sh` | Telemetry service account and key management |
| `gce-start-hub.sh` | Build, deploy, and restart the hub service |
| `gce-certs.sh` | Cloud DNS setup and Let's Encrypt wildcard certificates |
| `gce-demo-cloud-init.yaml` | Cloud-init config installed on the VM at provision time |
| `gce-setup-nats.sh` | *(Archived)* Standalone NATS server setup — superseded by in-process events |
| `hub.env.sample` | Template for the environment file (copy to `.scratch/hub.env`) |
| `hub.env.example` | Minimal env example (kept for reference) |
