---
title: Hub Setup on GCE
description: Deploy a Scion Hub on a Google Compute Engine VM using the starter scripts.
---

## Overview

The quickest path to a deployed Scion Hub is a single Google Compute Engine VM using the starter scripts in `scripts/starter-hub/`. These scripts automate VM provisioning, repository setup, TLS configuration, and Hub startup.

## Prerequisites

- A **GCP project** with billing enabled.
- The **gcloud CLI** installed and configured (`gcloud auth login`, project set).
- A **domain name** (optional but recommended for HTTPS/TLS).

## Steps

The starter scripts are designed to be run in sequence from your local machine.

### 1. Provision the VM

```bash
./scripts/starter-hub/gce-demo-provision.sh
```

Creates a GCE VM with the necessary machine type, disk, firewall rules, and service account.

### 2. Set Up the Repository

```bash
./scripts/starter-hub/gce-demo-setup-repo.sh
```

SSHs into the VM and clones the Scion repository, installing required dependencies.

### 3. Build and Deploy

```bash
./scripts/starter-hub/gce-demo-deploy.sh
```

Builds the Hub server and its dependencies on the VM.

### 4. Configure TLS (Optional)

```bash
./scripts/starter-hub/gce-certs.sh
```

Sets up Caddy as a reverse proxy with automatic TLS certificate provisioning. Requires a domain name pointed at the VM's external IP.

### 5. Generate Hub Configuration

```bash
./scripts/starter-hub/hub-config.sh
```

Generates the `settings.yaml` file with your chosen options (domain, auth settings, etc.).

### 6. Start the Hub

```bash
./scripts/starter-hub/gce-start-hub.sh
```

Starts the Hub service on the VM. The Hub is now ready to accept connections.

## Post-Setup

Once the Hub is running:

1. **Access the Web Dashboard** — Navigate to your domain (or the VM's external IP) in a browser.
2. **Create your first project** — Use the dashboard or `scion project create` from the CLI.
3. **Register a Runtime Broker** — Connect a machine to execute agents. See [Runtime Broker](/scion/hub-user/runtime-broker/) for details on registering your local machine or a remote VM.

For ongoing Hub administration (auth, permissions, observability), see the other guides in the Hub Administration section.
