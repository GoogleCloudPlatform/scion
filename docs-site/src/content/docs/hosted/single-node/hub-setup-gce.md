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

### 4. Configure TLS

```bash
./scripts/starter-hub/gce-certs.sh
```

Sets up Caddy as a reverse proxy with automatic TLS certificate provisioning. Requires a domain name pointed at the VM's external IP.

:::note[Internal or private deployments]
If your VM has no external IP — or TLS is terminated upstream by a load balancer, reverse proxy, or similar appliance — skip this step and see [Internal Deployments (BYO TLS)](#internal-deployments-byo-tls) below.
:::

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
3. **Register a Runtime Broker** — Connect a machine to execute agents. See [Runtime Broker](/scion/hosted/ha/runtime-broker/) for details on registering your local machine or a remote VM.

For ongoing Hub administration (auth, permissions, observability), see the other guides in the Hub Administration section.

## Internal Deployments (BYO TLS)

The steps above assume a public-facing VM with an external IP and public DNS. If your VM is internal-only — for example, on a private VPC with no external IP — the Hub works identically, but TLS must be provided by you or terminated upstream.

### What to skip

| Step | Script | Skip? |
|------|--------|-------|
| 1. Provision the VM | `gce-demo-provision.sh` | **No** — run the script as-is. It creates firewall rules for inbound HTTP/HTTPS (tcp:80, tcp:443) that are unnecessary if the VM is not publicly reachable; you can remove them afterward or let your network team manage internal firewall rules instead. |
| 4. Configure TLS | `gce-certs.sh` | **Yes** — this script fetches the VM's external IP, creates public Cloud DNS records, and obtains Let's Encrypt certificates via DNS challenge. All of this requires a public IP and will fail without one. |

Steps 2, 3, 5, and 6 work without modification.

### Set `SCION_SERVER_BASE_URL`

The Hub uses `SCION_SERVER_BASE_URL` to construct OAuth redirect URIs and set the session cookie's `Secure` flag. When Step 4 is skipped, you must set this variable yourself.

In your `hub.env` file (see `scripts/starter-hub/hub.env.sample`):

```bash
# The URL that browsers and agents use to reach the Hub.
# Must include the scheme (https://) — the Hub derives cookie
# security from the URL scheme.
SCION_SERVER_BASE_URL=https://hub.internal.example.com
```

:::caution[HTTPS is strongly recommended]
If `SCION_SERVER_BASE_URL` uses `http://`, the Hub will not set the `Secure` flag on session cookies. Use `https://` in production even when TLS is terminated upstream.
:::

### TLS options

Choose the option that matches your environment:

**Option A — Caddy with your own certificates**

If you still want Caddy as a local reverse proxy but with your own certificate and key instead of Let's Encrypt, create a Caddyfile on the VM:

```caddy
hub.internal.example.com {
    tls /path/to/your/cert.pem /path/to/your/key.pem
    reverse_proxy localhost:8080
}
```

Then start Caddy manually (`sudo caddy start --config /etc/caddy/Caddyfile`) instead of running `gce-certs.sh`.

**Option B — TLS terminated upstream**

If TLS is terminated by an upstream load balancer, reverse proxy, or appliance (e.g., an F5, nginx, or GCP HTTPS Load Balancer), no local TLS configuration is needed. The upstream proxy forwards plain HTTP to the Hub on port 8080. Ensure `SCION_SERVER_BASE_URL` is still set to the `https://` URL that clients use.

**Option C — Identity-Aware Proxy (GCP)**

For GCP deployments, you can front the Hub with [Identity-Aware Proxy (IAP)](/scion/hosted/ha/auth-proxy-iap/) instead of managing certificates directly. IAP handles both TLS and user authentication at the network edge. The IAP guide covers HA deployments but the same pattern works for a single VM behind an internal load balancer.
