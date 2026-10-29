# Single-Node VM Deployment Guide

Deploy a Scion Hub on a Google Compute Engine (GCE) VM with Identity-Aware Proxy
(IAP) authentication. This tier uses released binaries, local storage, and a
Cloud Run IAP reverse proxy — no container builds, no GCS, no external database.

> Part of the Single-Node-VM deploy tier (ptone/scion#1575).

> **Adding a GKE target for shared-dir storage?** See the
> [Hybrid Deployment Tier](hybrid-tier.md) page — it extends this same VM with
> a second, Kubernetes-based place to run agents, sharing project scratchpads
> between the two runtimes over an NFS export served from this VM
> (`ptone/scion#1777`).

## Overview

The single-node-VM tier stands up a persistent GCE VM running the `scion` binary
from a GitHub Release. A Cloud Run service acts as an IAP-authenticated reverse
proxy, giving users browser access without exposing the VM directly. Agents
running on the VM connect to the Hub over localhost, bypassing IAP entirely.

Key properties:

- **Binary-based** — downloads a pre-built release; no source checkout or
  container build required.
- **Checksum-verified** — every downloaded binary and chat-plugin tarball is
  verified against the release's `SHA256SUMS` asset before it is extracted;
  see [Quick Start](#quick-start) below.
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
| Permissions | Project Owner, or Editor plus `roles/resourcemanager.projectIamAdmin` and `roles/run.admin` (create VMs, Cloud Run services, service accounts, IAM bindings) — Editor alone lacks both `resourcemanager.projects.setIamPolicy` (the project-level IAM bindings this script creates) and `run.services.setIamPolicy` (the Cloud Run service's own invoker binding); `projectIamAdmin` covers the first but not the second, hence `run.admin` too. Both Owner and Editor already include `iam.serviceAccounts.actAs` on the service accounts the script uses, so no separate Service Account User grant is needed. The script also grants `roles/iap.tunnelResourceAccessor` to the deployer for SSH access to the private VM. With the optional hybrid tier enabled, also `roles/iam.serviceAccountAdmin` (`iam.serviceAccounts.setIamPolicy`, for the transport service account's token-creator binding) and `roles/iap.admin` (`iap.webServices.setIamPolicy`, for its IAP access grant). Owner already covers both. |
| VM OS image | Ubuntu 22.04 LTS — pinned, not currently configurable (see [Architecture](#architecture)). |

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
8. **Update policy** — Notify (check and notify only; the default), Auto
   (install updates automatically), or Disabled. `--update-policy` (or
   `update_policy` in the config file) skips the prompt.

To install a specific release version:

```bash
./scripts/single-node-vm/deploy.sh --version v0.5.0
```

Before downloading anything, the script checks that the chosen release
publishes a `SHA256SUMS` checksums asset, and downloads and verifies every
binary and chat-plugin tarball against it before installing.
A release published before checksum publishing existed has no `SHA256SUMS`
asset; targeting one with `--version` (or letting auto-detect pick a nightly
that predates it) fails immediately, before any GCP resource is created, with
an error naming the release. To install such a release anyway, without
verification, set `ALLOW_UNVERIFIED_RELEASE=true` in the environment — this
prints a loud warning and is not recommended outside of migrating off an old
pinned version.

### Config File (Headless Mode)

To run the deployment without interactive prompts, create a JSON config file
that pre-answers all wizard questions and pass it with `--config`:

```bash
./scripts/single-node-vm/deploy.sh --config my-deploy-config.json
```

See `scripts/single-node-vm/deploy-config.example.json` for a documented
template with all available fields.

When a config file is provided:
- Fields present in the config are used directly (no prompt).
- Missing fields fall back to interactive prompts (if a terminal is
  attached) or sensible defaults.
- In fully non-interactive mode (no terminal on stdin), missing required
  fields cause the script to exit with an error rather than hanging on a
  `read`.

Example minimal config for headless deployment:

```json
{
  "hub_name": "my-hub",
  "region": "us-central1",
  "machine_size": "small",
  "disk_size_gb": 200,
  "container_images": {
    "source": "build"
  }
}
```

This is particularly useful for agent-driven deployments where a
conversational agent gathers answers from a user, writes the config file,
and runs `deploy.sh --config` headlessly.

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
  The VM image is pinned to **Ubuntu 22.04 LTS** (`ubuntu-2204-lts` /
  `ubuntu-os-cloud` in `deploy.sh`); `cloud-init.yaml`'s Docker apt-repo setup
  is written specifically for this image. This is a deliberate design
  decision, not currently configurable — there is no demand for other
  distros, and making the image configurable would multiply an already-zero
  test matrix. Ubuntu 22.04 standard support ends April 2027; a bump to
  24.04 LTS should be its own tested change when the time comes.
- **Agents** — launched as Docker containers on the VM by the runtime broker;
  they connect to the Hub at `localhost:8080`, no IAP needed.

## What Gets Created

The deploy script creates the following GCP resources:

| Resource | Name pattern | Purpose |
|----------|-------------|---------|
| GCE VM | `scion-hub-<hub-name>` | Runs the Scion Hub binary via systemd |
| Service account | `scion-hub-<hub-name>@<project>.iam.gserviceaccount.com` | VM identity with logging/monitoring roles, Vertex AI access, and `roles/iam.serviceAccountAdmin` for [hub-minted service accounts](#hub-minted-service-accounts) |
| Service account | `scion-hub-<hub-name>-proxy@<project>.iam.gserviceaccount.com` (truncated and hashed for long hub names) | Cloud Run proxy identity, with no project IAM roles |
| Cloud Run service | `scion-hub-<hub-name>-iap-proxy` | IAP-authenticated reverse proxy to the VM |
| Firewall rule | `scion-hub-<hub-name>-allow-proxy` | Allows the Cloud Run proxy (Direct VPC egress) to reach the VM on tcp:8080 |
| IAM bindings | IAP `httpsResourceAccessor` for the deployer; `roles/run.invoker` for the IAP service agent on the Cloud Run proxy | Grants the deployer browser access through IAP; lets IAP itself invoke the proxy service |

On the VM itself:

| Path | Purpose |
|------|---------|
| `/opt/scion/releases/<tag>/scion` | Installed Scion releases, one directory per release tag (owned by `scion`) |
| `/opt/scion/current` | Symlink to the active release (`releases/<tag>`); the unit runs `/opt/scion/current/scion` |
| `/opt/scion/previous` | Symlink to the release that was active before the last switch |
| `/opt/scion/snapshots/` | Database snapshots taken by the Hub |
| `/usr/local/bin/scion` | Root-owned symlink to `/opt/scion/current/scion`, for running `scion` on the VM |
| `/home/scion/.scion/settings.yaml` | Hub configuration (auth mode, storage, etc.) |
| `/home/scion/.scion/hub.env` | Environment variables (session secret, project ID) |
| `/home/scion/.scion/workspace-storage/` | Local workspace storage directory |
| `/home/scion/.scion/plugins/broker/` | Chat plugin binaries (if selected) |
| `/etc/systemd/system/scion-hub.service` | systemd unit file |

The unit runs as `User=scion` with `WorkingDirectory=/home/scion`, matching
the paths above — the Hub's daemon working directory is that user's home
directory, not `/`.

## Binary layout and updates

The Hub binary lives in a versioned install root owned by the `scion` user,
so the Hub can update itself without sudo:

```
/opt/scion/                         scion:scion 0755
  releases/nightly-20261023/scion   one directory per installed release tag
  releases/v0.3.0-preview.4/scion
  current  -> releases/nightly-20261023
  previous -> releases/v0.3.0-preview.4
  snapshots/
/usr/local/bin/scion -> /opt/scion/current/scion   (root-owned symlink)
```

The systemd unit runs `ExecStart=/opt/scion/current/scion server start ...`.
To install a release, the binary is verified against the release's
`SHA256SUMS`, placed in `releases/<tag>/`, and `current` is switched to it
with an atomic rename. The release `current` pointed at before becomes
`previous`. `settings.yaml` records the root as
`server.maintenance.install_root: "/opt/scion"`.

`/usr/local/bin/scion` is now a **symlink**, not a binary. Running `scion` on
the VM always runs the active release. Do not copy a binary over
`/usr/local/bin/scion` by hand: that replaces the symlink, and the Hub
(which runs `/opt/scion/current/scion`) does not see the change. To install
a different release, re-run `deploy.sh --version <tag>`, or update from the
Maintenance page. A re-run without `--version` keeps the release `current`
points at (see [Re-running the deploy script](#re-running-the-deploy-script)).

### Converting an existing VM (`migrate-layout`)

VMs deployed before this layout have a plain binary at `/usr/local/bin/scion`
and a unit that runs it directly. Convert one with:

```bash
./scripts/single-node-vm/deploy.sh migrate-layout --config my-deploy-config.json
```

`hub_name` and `project_id` are read from the config file (without one, the
script prompts for the hub name and uses the active gcloud project). The
command:

1. creates `/opt/scion/{releases,snapshots}`, owned by `scion`;
2. moves the **currently installed** binary to `/opt/scion/releases/<its
   version>/scion`, read from `scion version --format json`. A build with no
   release version is stored as `releases/git-<sha12>`. Nothing is
   downloaded, and the version does not change;
3. points `/opt/scion/current` at it;
4. replaces `/usr/local/bin/scion` with a symlink to
   `/opt/scion/current/scion`;
5. edits `settings.yaml` in place, as the file's owner (`scion`): adds
   `server.maintenance.install_root: "/opt/scion"` and applies the update
   policy rule below. Comments (including a trailing comment on the two
   edited lines) and all other keys are left as they are. The edited file is parsed (with the VM's PyYAML) and compared with the
   original before it replaces it;
6. re-renders the systemd unit with
   `ExecStart=/opt/scion/current/scion ...`;
7. runs `systemctl daemon-reload`, restarts the Hub, and waits for
   `/healthz`.

It is safe to re-run. On a converted VM the layout step does nothing, the
settings edit changes only what is out of date, and the unit is re-rendered
and the Hub restarted.

If something goes wrong, the command stops at that step:

- **The installed binary's version cannot be read, or the binary cannot be
  copied completely** (for example, the disk is full). The command stops
  before `current`, `/usr/local/bin/scion` or the unit change, so nothing
  is converted. For an unreadable version, run a full
  `deploy.sh --version <tag>` instead. For a failed copy, free disk space
  and re-run.
- **`/opt/scion`, `/opt/scion/releases` or `/opt/scion/snapshots` is not a
  directory owned by `scion` with mode 0755** (for example, it was changed
  by hand or is a symlink). The script refuses rather than repairing it,
  and nothing changes. Fix the ownership or mode by hand
  (`sudo chown scion:scion …`, `sudo chmod 0755 …`; replace a symlink with
  a real directory) and re-run. `deploy.sh --version` refuses in the same
  way. The exception is an `/opt/scion` that is still owned by root because
  an earlier first run stopped partway. The next run finishes setting it up
  and hands it to `scion`. Its parent, `/opt`, must be owned by root and not
  writable by group or others (the image default, `root 0755`). Otherwise
  `/opt/scion` is neither created nor resumed. The same rule applies to
  `/usr/local/bin` and every directory above it, because root writes the
  `scion` symlink there. The script refuses before changing anything.
- **`/usr/local/bin/scion` is a symlink to a binary only root can read**
  (for example a hand-made `/usr/local/bin/scion -> /usr/local/lib/scion`
  at mode 0700). A binary reached through a symlink is copied by the
  `scion` user, not by root, so `migrate-layout` (and a `deploy.sh` re-run
  over the old layout) stops with "input unreadable by scion". Make the
  binary readable (`sudo chmod 0755 …`), or replace the symlink with the
  plain binary, and re-run.
- **The settings edit is refused.** This happens if the file has a shape the
  script cannot edit safely (for example a flow-style `maintenance: {...}`),
  or if PyYAML is missing (`sudo apt-get install python3-yaml`). By this
  point steps 1-4 are done: `/opt/scion` exists and `/usr/local/bin/scion`
  is already the symlink. `settings.yaml` is unchanged, and the unit change
  and restart (steps 6-7) are skipped. The Hub keeps running the same
  binary through the symlink. To finish, set
  `install_root: "/opt/scion"` and `update_policy` by hand as block-style
  keys under `server:` → `maintenance:` in `settings.yaml`, then re-run
  `migrate-layout`. The re-run finds
  the settings already up to date (no parser needed), then renders the unit
  and restarts the Hub.

A normal re-run of `deploy.sh` also
converts the VM: it keeps the old binary as the `previous` release and
installs the requested version as `current`.

**Update policy on conversion.** `migrate-layout` rewrites an existing
`update_policy: "auto"` to `"notify"`, unless you ask for auto explicitly
with `--update-policy auto` (or `update_policy: "auto"` in the config file).
`notify` and `disabled` are kept. Before this change, `auto` was selected
for you by default, and it did nothing on most VMs because the Hub could
not replace its own binary. After conversion it works, so it should be a
choice you make, not a default.

**Behaviour change for unconverted VMs.** Some VMs updated themselves
before this layout only because the `scion` user happened to have full
passwordless sudo, for example through the GCE `google-sudoers` group, and
had no maintenance repository configured. Those Hubs now report the
installer as `unsupported` on the Maintenance page and do not download or
install updates until you run `deploy.sh migrate-layout`. Hubs set up with
the starter-hub sudoers rules (`/etc/sudoers.d/scion-rebuild-server`) and a
maintenance repository path keep updating through the `sudo-install`
strategy, with no change on the host.

## Configuration

### settings.yaml

The Hub configuration file. The deploy script writes it in two stages:

1. **During VM setup (Phase 3)** — a bootstrap file with `server.auth.mode: dev`,
   no proxy settings and `server.hub.host: 127.0.0.1`, so the Hub can start
   before IAP exists. The loopback bind is needed because the server refuses
   to listen on a non-loopback address without an https base URL, and the
   https URL is derived from the IAP audience that only Phase 5 writes.
   Phase 3 checks only the unauthenticated `/healthz` endpoint over SSH.
2. **After IAP is ready (Phase 5)** — `auth.mode: proxy` with the IAP audience
   string, enabling IAP-based authentication. This file has no
   `server.hub.host`, so after the restart the Hub listens on `0.0.0.0`
   where the IAP proxy can reach it.

The Phase 3 file does **not** turn on dev auth. `server.auth.mode: dev` on its own
does not enable it, so until Phase 5 the Hub answers `/healthz` normally and
every authenticated API call with `401`. Dev auth is turned on only by the
`--dev-auth` flag or `server.auth.dev_mode: true` (environment variable
`SCION_SERVER_AUTH_DEVMODE=true`). The server refuses to start with dev auth
when the web server binds a non-loopback address. From Phase 5 on, the Hub
binds `0.0.0.0` so that the IAP proxy can reach it, so do not enable dev auth
on the VM. If you need authenticated access before Phase 5, configure OAuth
(see [Authentication](../../docs-site/src/content/docs/hosted/single-node/auth.md)).
For a short-lived test only, you can bind the Hub to loopback
(`--host 127.0.0.1`) with dev auth and reach it through an SSH tunnel
(`gcloud compute ssh scion-hub-my-hub -- -L 8080:localhost:8080`). Revert both
before Phase 5, because the IAP proxy cannot reach a loopback-only Hub.

Final configuration:

```yaml
schema_version: "1"
image_registry: "localhost/scion"
server:
  hub:
    hub_name: "my-hub"
    admin_emails:
      - "you@example.com"
  maintenance:
    deployment_tier: "binary"
    release_channel: "nightly"
    update_policy: "notify"
    install_root: "/opt/scion"
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
  # Listen port: set by --web-port in scion-hub.service, not here.
```

Key settings:

- `schema_version` — must be `"1"` (not `settings_version`).
- `image_registry` — required, even for locally built images. Set to
  `localhost/scion` for local builds, or the registry path for remote images.
- `hub.hub_name` — display name for the Hub, also reported as the
  `scion.hub.name` telemetry attribute. The name also appears in health
  output and scopes the diagnostics log queries. The deploy script sets
  it to the hub name you deploy with; when unset, the VM host name is
  used.
- `admin_emails` — email(s) auto-promoted to super-admin on login.
- `storage.local_path` — all workspace data stored on the VM's local disk.
- `secrets.backend: local` — secrets are read from `hub.env` on disk, not from
  GCP Secret Manager.
- `auth.mode: proxy` — the Hub trusts the IAP proxy header for user identity.
- `auth.proxy.iap.audience` — the IAP audience string, computed automatically
  from the project number, region, and proxy service name.
- `maintenance.deployment_tier` — `"binary"` for release-based deployments
  (set automatically by the deploy script). Source deployments use `"source"`.
- `maintenance.release_channel` — which release channel to track for updates:
  `"stable"`, `"preview"` or `"nightly"`. The deploy script's default is
  `"nightly"`.
- `maintenance.install_root` — the versioned install root (see
  [Binary layout and updates](#binary-layout-and-updates)). The deploy
  script sets it to `"/opt/scion"`.
- `maintenance.update_policy` — controls automatic update behavior:
  - `"notify"` — check for updates and show a banner in the admin UI; admin
    must manually trigger the update. This is the deploy script's default.
  - `"auto"` — check for updates on a schedule and install automatically.
    Only used when you choose it (`--update-policy auto` or
    `update_policy: "auto"` in the config file).
  - `"disabled"` — no automatic update checking (manual checks via the admin
    UI still work).

  Like the initial install, an `"auto"` update verifies the downloaded binary
  against the target release's `SHA256SUMS` asset and fails the update (with
  no binary swapped in) if that asset or a matching entry is missing, or the
  hash doesn't match. Unlike the initial install, there is no override: a
  release with no checksums simply isn't auto-installed.

### hub.env

Environment variables for the Hub process, stored at
`/home/scion/.scion/hub.env`:

```bash
SCION_SERVER_SESSION_SECRET=<randomly-generated-base64>
SCION_GCP_PROJECT_ID=<your-project-id>
GOOGLE_CLOUD_PROJECT=<your-project-id>
```

The `SCION_SERVER_SESSION_SECRET` is generated once during initial deployment and preserved
on subsequent runs.

### Hub-scoped agent env vars

`hub.env` configures the Hub process only. The env vars that agents receive
are stored in the hub database. After the Phase 3 health check, the deploy
script writes these hub-scoped env vars (injection mode `always`) into
`/home/scion/.scion/hub.db` with `sqlite3`:

| Key | Value |
|-----|-------|
| `GOOGLE_CLOUD_PROJECT` | your project ID |
| `GOOGLE_CLOUD_LOCATION` | `global` (the global Vertex AI endpoint, intentionally) |

They appear in the admin UI as hub env vars, and you can edit them there.
The deploy only seeds these keys when they are absent. Edits made in the
admin UI are kept across redeploys, and a deleted key is created again on the
next deploy. If the write fails, the deploy continues and prints the command to
run manually.

### Hub-minted service accounts

Project owners can have the hub mint new GCP service accounts for agents.
The hub does this with its own credentials, which on this deployment are the
hub VM's service account. Minting creates the account, sets IAM policy on it,
and deletes it if a follow-up grant fails. The deploy script therefore grants
the hub VM's service account `roles/iam.serviceAccountAdmin` on the project by
default, and enables `iamcredentials.googleapis.com`, which the hub uses to
issue tokens for minted accounts. To skip the role grant, set
`"hub_sa_minting": false` in the deploy config. That does not revoke a grant
from an earlier deploy; the deploy output prints the removal command.

For a deployment made before the script granted this role, re-run the deploy
script, or grant the role by hand (no hub restart needed; IAM changes can take
a few minutes to apply):

```bash
gcloud projects add-iam-policy-binding PROJECT_ID \
  --member="serviceAccount:scion-hub-HUB_NAME@PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/iam.serviceAccountAdmin" --condition=None
gcloud services enable iamcredentials.googleapis.com --project=PROJECT_ID
```

`roles/iam.serviceAccountAdmin` applies to every service account in the
project. The deploy sets the hub's default agent GCP identity mode to
`passthrough`, and passthrough agents use the VM's credentials, so they hold
the same roles as the hub, this one included. Passthrough is meant for
getting started only. Beyond that, set the default to `assign` or `block`
(Admin > Server Config > Agent Defaults), and consider running the hub in a
project that holds no other privileged service accounts.

Minted accounts start with no roles: grant each one whatever its agents need
(for example `roles/aiplatform.user` for Vertex AI) before assigning it.
Teardown does not delete minted accounts.

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
broker plugin within the Hub process. Like the main binary, each plugin
tarball is verified against the release's `SHA256SUMS` asset before
installation (see [Quick Start](#quick-start)).

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
3. The Cloud NAT and Cloud Router (unless they were reused from a pre-existing gateway on the network/region)
4. The hub VM's service account's project role bindings (also any `deleted:serviceAccount:` tombstones of it on those roles)
5. The hub VM's service account
6. The Cloud Run proxy's service account
7. The IAP SSH firewall rule
8. The proxy-to-VM firewall rule (tcp:8080)

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
- **5xx, or agents cannot reach the Hub, after a deploy that did not reach
  "Deployment Complete"** — the Hub is still on the Phase 3 bootstrap bind
  (`server.hub.host: 127.0.0.1`; see [settings.yaml](#settingsyaml)), so
  `/healthz` answers on the VM's localhost but nothing else can connect.
  Only the Phase 5 `settings.yaml` rewrite and restart remove the bind.
  Fix: re-run `deploy.sh` to completion.

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

After the IAP proxy is deployed, the script updates `settings.yaml` from the
Phase 3 bootstrap (`server.auth.mode: dev`, which does not enable dev auth; see
[settings.yaml](#settingsyaml)) to `auth.mode: proxy` and restarts the Hub. If authentication
is not working:

1. Verify the settings on the VM:
   ```bash
   gcloud compute ssh scion-hub-my-hub \
     --zone=us-central1-b \
     --project=PROJECT_ID \
     --command='sudo -u scion cat /home/scion/.scion/settings.yaml'
   ```
2. Confirm `auth.mode` is `proxy` and the `audience` string matches the
   project number and service name.
3. Check that the Hub restarted successfully after the settings update.

### Requests IAP rejects never reach Cloud Run or the Hub logs

The proxy is deployed with `--iap` directly on the Cloud Run service (IAP
enforcing access in front of it, not via a load-balancer backend service). A
request IAP rejects — for example, the caller lacks the
`roles/iap.httpsResourceAccessor` binding — never reaches the Cloud Run
container. It will not appear in the Cloud Run service's own logs, and the
Hub never sees it either, so from the operator's side the request simply
vanishes with no local trace. (A mismatched `auth.proxy.iap.audience` is a
different case: IAP lets that request through and the Hub itself rejects it,
so it *does* show up in the Hub's logs — `sudo journalctl -u
scion-hub.service` on the VM, as in *Health check fails after deployment*
above. The audience check itself is covered in the previous section.)

To get visibility into IAP's own decisions, enable [Data Access audit
logs](https://cloud.google.com/logging/docs/audit/configure-data-access) for
the `iap.googleapis.com` service:

- **Console (simpler, recommended):** IAM & Admin > Audit Logs, select the
  project, then in the **Data Access audit logs configuration** table select
  **Identity-Aware Proxy** (use the filter to find it), open the **Permission
  types** tab, enable **Data Read**, then **Save**.
- **IAM policy (`auditConfigs`):** editing the policy directly requires a
  read-modify-write so you don't drop existing bindings. `set-iam-policy`
  replaces the *whole* policy, and the `etag` makes a concurrent change fail
  loudly instead of being silently overwritten:

  ```bash
  gcloud projects get-iam-policy PROJECT_ID --format=yaml > /tmp/policy.yaml
  # Edit /tmp/policy.yaml: add or merge into an `iap.googleapis.com` entry
  # under `auditConfigs`. Leave `bindings` and `etag` untouched.
  gcloud projects set-iam-policy PROJECT_ID /tmp/policy.yaml
  ```

  The `auditConfigs` entry to add or merge in:

  ```yaml
  auditConfigs:
    - service: iap.googleapis.com
      auditLogConfigs:
        - logType: DATA_READ
        # - logType: ADMIN_READ  # optional: IAP settings/IAM policy reads,
        #                        # not needed to see rejected requests
  ```

Once enabled, IAP's per-request authorization decisions appear as Data Access
entries in Cloud Logging. To see denials specifically:

```
log_id("cloudaudit.googleapis.com/data_access")
protoPayload.serviceName="iap.googleapis.com"
protoPayload.authorizationInfo.granted=false
```

**Caveat:** Google's [IAP audit logging
guide](https://cloud.google.com/iap/docs/audit-log-howto) documents only
admin API methods (for example `SetIamPolicy`, `GetIamPolicy`,
`GetIapSettings`, `UpdateIapSettings`) and tunnel-destination-group methods
in its audited-method list — not a per-request method name. It does,
however, document `authorizationInfo.granted` as "a boolean representing
whether IAP permitted the requested access," which is what the filter above
relies on.
In practice, per-request entries observed in Cloud Logging carry
`protoPayload.methodName="AuthorizeUser"`; treat that specific method name as
empirical rather than a documented guarantee. (Google's [access
management guide](https://cloud.google.com/iap/docs/managing-access) does
confirm the general behavior: granting public access, for example, means
"IAP won't generate Cloud Audit Logs logs for the request".) For enabling
IAP directly on a Cloud Run service — the mode this deployment uses — see
[Identity-Aware Proxy for Cloud
Run](https://cloud.google.com/run/docs/securing/identity-aware-proxy-cloud-run).

**Cost note:** unlike Admin Activity audit logs (always on, no extra charge),
Data Access audit logs are billed like other Cloud Logging ingestion beyond
the free monthly allotment. Enable them deliberately rather than by default,
and turn them off again once you're done diagnosing.

### Re-running the deploy script

The script is idempotent. Re-running it will:
- Skip creating the VM and service account if they already exist
- Preserve the existing `hub.env` (and its `SCION_SERVER_SESSION_SECRET`)
- Seed the hub-scoped `GOOGLE_CLOUD_*` env vars only if they are absent (admin edits are preserved)
- Overwrite `settings.yaml` with the current configuration. The update
  policy comes from `--update-policy`, then the config file's
  `update_policy`, and otherwise the prompt, which defaults to `notify`. An
  earlier `auto` is not carried over unless you ask for it again.
- Install the requested release into `/opt/scion/releases/<tag>/` and switch
  `/opt/scion/current` to it. On a VM with the old layout, the old binary is
  kept as the `previous` release (see
  [Binary layout and updates](#binary-layout-and-updates)).
- **Without `--version`, keep the release the VM already runs** when
  `/opt/scion/current` exists. The Hub may have updated itself past the
  release that auto-detect would pick (auto-detect reads GitHub's latest
  release, not the Hub's channel), so the script neither downloads nor
  switches the binary, and says so. Chat plugins are still installed from
  that release. Pass `--version <tag>` to change releases. If the VM runs a
  git build (`releases/git-<sha>`), the script stops and asks for
  `--version`.
- **With `--version` older than the running release**, print a `DOWNGRADE`
  warning and proceed, because the version was chosen explicitly. A
  downgrade can fail to start if the newer release already migrated the Hub
  database. The newer release stays installed as `/opt/scion/previous`.
- Re-deploy the Cloud Run proxy (converges to the same state)
