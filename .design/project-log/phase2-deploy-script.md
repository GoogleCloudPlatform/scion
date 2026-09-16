# Phase 2: Deploy Script — VM Provisioning + Binary Install

**Date:** 2026-09-15
**Issue:** ptone/scion#1575 (Single-Node-VM deploy tier)
**Branch:** scion/sn-vm-deploy-script

## What Was Created

### scripts/single-node-vm/deploy.sh
Wizard-style interactive deployment script that provisions a GCE VM, downloads
the scion binary from GitHub Releases, and starts the hub via systemd. The script
runs in three phases:

1. **Prerequisites** — detects GCP project, prompts for hub name/region/machine
   size, resolves the release version from GitHub (or accepts `--version`),
   validates gcloud auth.
2. **GCP Resources** — enables APIs, creates a service account with minimal IAM
   roles (logging, monitoring, tracing), creates the VM with no public IP.
3. **VM Setup** — downloads the scion binary, generates a session secret, writes
   hub.env and settings.yaml from templates, installs the systemd unit, starts
   the service, and runs a health check.

The script is idempotent: it checks whether resources exist before creating them.

### scripts/single-node-vm/cloud-init.yaml
Minimal cloud-init configuration. Installs only Docker and creates the scion user
with required directories. Does NOT install Go, Node.js, Caddy, Certbot, NATS,
or any build tooling — the scion binary is pre-built and downloaded from GitHub
Releases.

### scripts/single-node-vm/config-templates/
- **hub.env.template** — environment file with SESSION_SECRET and GCP project
  placeholders.
- **settings.yaml.tpl** — v1-format settings with local storage, local secrets
  backend, and dev auth mode.
- **scion-hub.service** — systemd unit that starts scion in foreground mode.

## Key Design Decisions

- **No public IP** (`--no-address`): the VM is only accessible via SSH tunnel
  (Phase 2) or IAP proxy (Phase 3). This is the primary security boundary.
- **Dev auth mode**: no IAP proxy exists yet, so auth.mode is "dev". Phase 3
  will change this to "proxy" with IAP configuration.
- **Local storage and local secrets**: no GCS bucket or GCP Secret Manager
  dependencies. Data lives on the VM's local disk.
- **Binary from GitHub Releases**: no source build required. The deploy script
  downloads `scion-linux-amd64.tar.gz` from the release URL.
- **systemd service**: the hub runs as the `scion` user under systemd with
  automatic restart on failure.
- **Machine size choice**: Small (e2-standard-4, ~10 agents) or Medium
  (n2-standard-16, ~50 agents) offered as interactive options.
