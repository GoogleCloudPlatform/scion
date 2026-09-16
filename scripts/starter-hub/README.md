# Developer Hub — GCE Demo Deployment

> **Note:** The directory is still named `scripts/starter-hub/` for backward
> compatibility. The deployment tier is now called **Developer Hub**.

Scripts for provisioning and operating a Scion Hub on a Google Compute Engine VM.
This tier builds from source, uses GCS for storage, and GCP Secret Manager for
secrets. For a simpler binary-based deployment, see the
[Single-Node VM](../../docs/deploy/single-node-vm.md) tier.

## Prerequisites

- Google Cloud SDK (`gcloud`) authenticated with a project
- A registered domain with DNS delegated to Cloud DNS (see `gce-certs.sh`)

## Configuration

All scripts source `hub-config.sh`, which derives resource names, domains, and
file paths from two primary variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `HUB_NAME` | `demo` | Deployment name — drives GCE instance, SA, firewall rule, cluster, and DNS names |
| `BASE_DOMAIN` | `scion-ai.dev` | Root domain — combined with `HUB_NAME` to form `hub.<name>.<base>` |
| `ENABLE_GKE` | `false` | Set to `true` to provision a GKE cluster, grant `container.admin`, configure credentials, and use Kubernetes as the default runtime. |
| `REGION` | `us-central1` | GCP region for GKE and resource locations |
| `ZONE` | `us-central1-a` | GCP zone for the GCE VM instance |
| `MACHINE_TYPE` | *(derived)* | Compute machine type to use (overrides `SIZE_CHOICE`) |

To stand up a second hub (e.g., "staging"):

```bash
export HUB_NAME=staging
# All scripts now target scion-staging, hub.staging.scion-ai.dev, etc.
```

Any derived variable can also be overridden individually via the environment.
See `hub-config.sh` for the full list.

## Environment Setup

1. Copy the sample env file and fill in your values:

   ```bash
   mkdir -p .scratch
   cp scripts/starter-hub/hub.env.sample .scratch/hub-demo.env
   # Edit .scratch/hub-demo.env with your secrets
   # For a second hub: cp hub.env.sample .scratch/hub-staging.env
   ```

2. Configure OAuth credentials for Google and/or GitHub. See the
   [Authentication & Identity docs](https://scion-ai.dev/hub-admin/auth/)
   for how to create OAuth client IDs and secrets for both web and CLI flows.

## Initial Provision (one-time)

Run the all-in-one deploy script from the repo root:

```bash
./scripts/starter-hub/gce-demo-deploy.sh
```

This runs six steps in sequence:

| Step | Script | What it does |
|------|--------|-------------|
| 0 | `gce-demo-preflight.sh` | Validates local tools, env file, GCP auth, APIs, IAM permissions, and DNS readiness |
| 1 | `gce-demo-provision.sh` | Creates GCE VM, service account, firewall rules, and (optionally) a GKE cluster |
| 2 | `gce-demo-telemetry-sa.sh` | Creates a least-privilege GCP service account for agent telemetry export |
| 3 | `gce-demo-setup-repo.sh` | Clones the repo on the VM |
| 4 | `gce-certs.sh` | Cloud DNS zone + Let's Encrypt wildcard certificates |
| 5 | `gce-start-hub.sh --full` | Uploads config, builds the binary & web assets, installs Caddy + systemd, and starts the hub |

You can also run the preflight check standalone to verify prerequisites
without provisioning anything:

```bash
./scripts/starter-hub/gce-demo-preflight.sh
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
| `gce-demo-deploy.sh` | All-in-one initial deployment (runs preflight then the five scripts below) |
| `gce-demo-preflight.sh` | Preflight validation — checks tools, env file, GCP auth/APIs/IAM, and DNS before deploying |
| `gce-demo-provision.sh` | Provision/delete GCE VM and GCP resources |
| `gce-demo-cluster.sh` | Create/delete GKE Autopilot cluster for agent workloads |
| `gce-demo-setup-repo.sh` | Clone the repo on the VM |
| `gce-demo-telemetry-sa.sh` | Telemetry service account and key management |
| `gce-start-hub.sh` | Build, deploy, and restart the hub service |
| `gce-certs.sh` | Cloud DNS setup and Let's Encrypt wildcard certificates |
| `gce-demo-cloud-init.yaml` | Cloud-init config installed on the VM at provision time |
| `gce-setup-nats.sh` | *(Archived)* Standalone NATS server setup — superseded by in-process events |
| `hub-config.sh` | Shared configuration — all scripts source this for parameterized naming |
| `hub.env.sample` | Template for the environment file (copy to `.scratch/hub-<name>.env`) |
