---
title: Setting up the Scion Hub
description: Installation and configuration of the Scion Hub (State Server).
---

**What you will learn**: How to deploy, secure, and operate the Scion Hub infrastructure, including setting up persistence, configuring runtime brokers, and managing user access.

The **Scion Hub** is the central brain of a hosted Scion architecture. It maintains the state of all agents, projects, and runtime brokers, and provides the API used by the CLI and Web Dashboard.

## Core Responsibilities

- **Central Registry**: Maintains a record of all Projects (projects), Runtime Brokers, and Templates.
- **Identity Provider**: Manages user authentication (OAuth) and issues scoped JWTs for Agents and Brokers.
- **State Store**: Tracks the lifecycle, status, and metadata of all agents.
- **Task Dispatcher**: Routes agent commands from the CLI or Dashboard to the correct Runtime Broker via persistent WebSocket tunnels.

## Running the Hub

The Hub is part of the main `scion` binary. You can start it using the `server start` command. A full and complete production startup command will look something like this:

```bash
# Start the Hub, Web Dashboard, and a local Runtime Broker

SESSION_SECRET=\${SESSION_SECRET} scion --global server start --foreground --production --debug --enable-hub --enable-runtime-broker --enable-web --runtime-broker-port 9800 --web-port 8080 --storage-bucket \${SCION_HUB_STORAGE_BUCKET} --auto-provide

```

:::caution[Session Secret Security]
Pass the session secret via the `SESSION_SECRET` environment variable (e.g., through a systemd `EnvironmentFile`), **not** via the `--session-secret` CLI flag. CLI arguments are visible to any local user via `ps(1)` and `/proc/pid/cmdline`.
:::

This is often best managed through something like systemd

### Hub vs. Broker Processes
While they can run in the same process—known as **Combo Mode** (the default for `scion server start` with no flags, which runs in workstation mode)—they serve distinct roles:
- **The Hub** is the stateless control plane. It provides the API and Web Dashboard, and should be accessible via a public or internal URL.
- **The Broker** is the execution host. It registers with a Hub and executes agents. Brokers can run behind NAT or firewalls, as they establish outbound connections to the Hub. You can connect multiple external brokers to a single Hub.

In combo mode on GCP (Cloud Run Instances or GCE), the co-located broker
automatically detects the host's GCP service account email and project ID
from the GCE metadata server at registration time. There is no need to
configure these manually.

If you prefer to run the server in the background:
```bash
scion server start
```

To manage the background daemon, use:
- `scion server status`
- `scion server restart`
- `scion server stop`



## Configuration

The Hub is configured via the `server` section in `~/.scion/settings.yaml`.

### Basic Example
```yaml
schema_version: "1"
server:
  log_level: info
  hub:
    port: 9810       # Used in standalone mode only
    host: 0.0.0.0
  database:
    driver: sqlite
    url: hub.db
  auth:
    dev_mode: true
```

:::note[Combined Mode]
When running with `--enable-web`, the Hub API is mounted on the web server's port (default 8080) and the standalone Hub listener is not started. The `hub.port` setting only applies when the Hub runs without `--enable-web`.
:::

:::note[Cloud Run Instance defaults]
On Cloud Run Instances, the Hub automatically detects the environment and
applies the correct `cloudrun-sandbox` runtime profile as the default. You
do not need to configure `profiles` or `runtimes` manually — the embedded
defaults match the tier.
:::

See the [Server Configuration Reference](/scion/reference/server-config/) for all available fields.

## Authentication

The Hub supports multiple end-user authentication modes to balance ease of development with production security.

### OAuth 2.0 (Production)
Scion supports Google and GitHub as identity providers. Configuration requires creating OAuth Apps in the respective provider consoles.
See the [Authentication Guide](/scion/hosted/single-node/auth/) for detailed setup instructions.

### Dev Auth (Local Development, workstation mode)
For local testing, the Hub can auto-generate a development token:
```yaml
server:
  auth:
    dev_mode: true
```
The token is written to `~/.scion/dev-token` on startup. The CLI and Web Dashboard automatically detect this token when running on the same machine.

### User Access Tokens (Programmatic)

The Hub supports long-lived **user access tokens (UATs)** for CI/CD or other programmatic integrations. See [User Access Tokens](/scion/hosted/user/personal-access-tokens/).

## GCP Identity & Hub-Minted Service Accounts

The Scion Hub can manage and provision Google Cloud Platform (GCP) Service Accounts directly. This allows agents to authenticate to Google Cloud services via metadata server emulation, avoiding the need to distribute static credential files. 

### Configuration

To enable GCP identity management, the Hub itself must run with a GCP identity (e.g., attached to its GCE instance or GKE pod) that has the `iam.serviceAccounts.getAccessToken` permission for the target Service Accounts.

Minting new Service Accounts needs more than that. The Hub creates each account with its own identity, sets IAM policy on it, and deletes it again if a follow-up grant fails, so the Hub's identity needs `roles/iam.serviceAccountAdmin` on the Hub's GCP project (`roles/iam.serviceAccountCreator` alone is not enough). The Hub also needs `iamcredentials.googleapis.com` enabled to issue tokens for the accounts it mints. The single-node VM deploy script (`scripts/single-node-vm/deploy.sh`) grants the role to the hub VM's service account and enables the API by default. On other deployments, grant them yourself:

```bash
gcloud projects add-iam-policy-binding PROJECT_ID \
  --member="serviceAccount:HUB_SA_EMAIL" \
  --role="roles/iam.serviceAccountAdmin" --condition=None
gcloud services enable iamcredentials.googleapis.com --project=PROJECT_ID
```

`roles/iam.serviceAccountAdmin` applies to every Service Account in the project. Agents in `passthrough` GCP identity mode use the same credentials as a co-located Hub, so they hold this role too. Use `passthrough` for getting started only, and `assign` or `block` beyond that.

Administrators can configure Service Accounts via the Web Dashboard:
1. Navigate to the **Service Accounts** section in the Admin dashboard.
2. View the service account quota dashboard and configure minting capability controls.
3. Register existing GCP Service Accounts by email, or configure the Hub to mint new ones dynamically.

### Default Project Identities

Projects can be configured with default GCP identities that are automatically verified upon registration and automatically applied in the agent creation form. 

### Security & Authorization

Administrative actions for GCP Service Account management require `project-owner` (`ActionManage`) permissions to enforce strict security boundaries. Direct API access to Hub secrets from agents is explicitly blocked to prevent credential leakage.

For more details on how agents assume these identities via metadata server emulation, see the [Authentication Guide](/scion/hosted/single-node/auth/#gcp-identity--metadata-emulation).

### gs:// links in chat

An agent's assigned GCP Identity has a second use beyond Vertex AI: when the
`web.gcs_links` experiment is enabled on the Hub, a `gs://bucket/object`
reference that an **agent** posts in Native Web Chat becomes a clickable
link. Clicking it fetches the object through the Hub using the sending
agent's *current* assigned Service Account, through a token minted on
demand with only the read-only `devstorage.read_only` scope — never the
broader scope the agent itself uses for Vertex AI or other Google Cloud
calls, and never a token the browser can see or reuse directly.

- **What links, and when**: only a `gs://` reference inside a message an
  agent sent. The same text in a message you send does not link — the
  Hub only ever reads an object on an agent's behalf, never a human's.
- **Which identity is used**: the sending agent's *current* assigned Service
  Account at the moment you click, resolved fresh from the message each
  time — not a snapshot from when the message was originally posted. If the
  agent's identity was later reassigned or removed, the fetch uses the new
  identity (or fails, if none is assigned).
- **Size limits and previews**: objects up to 10 MiB can be opened. PNG,
  JPEG, GIF and WebP objects preview as images; other text — including SVG,
  which is always shown as source rather than rendered — previews inline up
  to 512 KB (larger text offers Download only); binary objects show "This
  file can't be previewed." with Download.
- **Errors and the Cloud Console fallback**: an object that can't be opened
  (not found, access denied, or no Service Account currently assigned) shows
  a single generic message rather than revealing which case applied; an
  object over 10 MiB shows a size-limit message instead. Every such case also
  offers an **Open in Cloud Console** link, so you can still inspect the
  object with your own Google identity and permissions.

## Project Settings & Agent Limits

The Hub provides a comprehensive UI for configuring project-level settings, ensuring administrators have control over resource allocation and project configurations. Access these settings via the Web Dashboard for any project you manage.

### Configuration Tabs

The Project Settings UI is organized into three primary tabs:

- **General**: Configure the project's display name, description, and template sync settings. For git-backed projects, you can specify default branches. For hub-managed projects, you can configure external git repositories to load templates from.
- **Limits**: Define constraints on agent execution to prevent resource exhaustion.
  - **Hub-level Defaults**: Administrators can configure global default limits that apply to all projects.
  - **Project-level Limits**: Overrides can be set per-project.
  - Limits automatically pre-populate the agent creation form and restrict maximum concurrency, runtime duration, and storage.
- **Resources**: Manage the compute and plugin environments available to the project.
  - **Runtime Brokers**: Link and manage the auxiliary runtimes (e.g., specific Kubernetes clusters or remote Docker hosts) authorized to execute agents for this project.
  - **Plugins**: Enable and configure message broker plugins or other extensions for agents running within the project.

### Template Synchronization

Projects support loading templates from external Git repositories, which is especially useful for non-Git-backed (hub-managed) projects. The UI accepts bare host/org/repo URLs (e.g., `github.com/org/repo`) and automatically normalizes them, appending `/.scion/templates/` unless a deeper path is specified. This synchronization can be manually triggered via the UI to immediately pull the latest templates.

## Server Maintenance & Updates

The Scion Hub provides a built-in maintenance administration panel in the Web Dashboard for managing routine server operations, updates, and synchronization.

### Maintenance Panel Operations

Administrators can trigger critical infrastructure operations directly from the dashboard:

- **Check for Updates**: Checks for available updates and allows administrators to execute an "Update Now" action. The update banner on the maintenance and server configuration pages runs the operation that matches the deployment tier: **Update Binary (`update-binary`)** on binary-tier deployments (single-node VMs installed from releases), which downloads and verifies the release binary, swaps it in, and restarts the Hub; and `rebuild-server` on source-tier deployments. See [Maintenance (`server.maintenance`)](/scion/reference/server-config/#maintenance-servermaintenance).
- **Rebuild Server (`rebuild-server`)**: Initiates a fire-and-forget server rebuild and restart sequence. It uses staging paths and sudoers implementation to ensure reliable updates even while the server is running.
- **Rebuild Web (`rebuild-web`)**: Recompiles the web frontend assets.
- **Pull Images (`pull-images`)**: Triggers the Docker/Podman executor to pull the latest agent container images.
- **Restart Hub**: Initiates a fire-and-forget server restart (`POST /api/v1/admin/maintenance/restart`) via systemd, restricted to administrators. A modal confirmation dialog prevents accidental triggers of restarts.

### Migrations

The panel also lists one-time data migrations, with **Run** for a pending migration and **Retry** for a failed one. The timezone-related ones, `utc-timestamp-normalize` and `applied-config-tz-cleanup`, are described in [Times and timezones: Operator steps](/scion/reference/times-and-timezones/#operator-steps). On SQLite, the automatic start-up repair of unreadable timestamp rows first writes a one-time snapshot next to the database file (`<db>.pre-utc-timestamp-normalize-<time>.bak`). It needs about the database's size in free disk space and contains secrets, so store it like the database and delete it once the repair is verified. A dry run of `utc-timestamp-normalize` (`{"params":{"dryRun":true}}`) reports without writing.

### Operation Execution & History

All maintenance tasks executed through the panel are tracked. The maintenance interface provides:
- Real-time log capture for active operations.
- Execution history detailing operation duration, completion status, and log archives.
- Automated cleanup of stalled operations during server startup.

#### Concurrency Guard

To guarantee server stability, the Hub enforces a strict **concurrency guard** on all maintenance operations:
- **Mutual Exclusion:** Running multiple operations (such as parallel `go build` compilations) simultaneously is rejected.
- **Conflict Rejection:** Any new maintenance request initiated while another is active returns a `409 Conflict` HTTP status code. This prevents resource exhaustion or server outages from concurrent builds.
- **Status Monitoring:** Operators can trace progress and view active logs on the dashboard's maintenance interface.

### WebDAV Synchronization

The Hub provides robust WebDAV endpoints for transparent file access across native, shared, and remote linked projects.
- WebDAV synchronization utilizes checksum comparisons for reliable file transfers.
- A local storage HTTP proxy facilitates efficient remote file synchronization.

## Persistence

The Hub requires a database to store its state.

### SQLite (Default)
Ideal for local development or single-node deployments. The database is a single file.
```yaml
server:
  database:
    driver: sqlite
    url: /path/to/your/hub.db
```

### PostgreSQL (Production)
**NOT IMPLEMENTED**

Recommended for high-availability or multi-node deployments.
```yaml
server:
  database:
    driver: postgres
    url: "postgres://user:password@localhost:5432/scion?sslmode=disable"
```

## Storage Backends

The Hub stores agent templates and other artifacts.

- **Local File System**: Default. Stores files in `~/.scion/storage`.
- **Google Cloud Storage (GCS)**: Recommended for cloud deployments. Set the `SCION_SERVER_STORAGE_BUCKET` environment variable.

For Hub-managed workspaces, the Hub uploads the workspace to its GCS bucket and sends that bucket name to the Runtime Broker in the agent create request, so the Runtime Broker downloads from the same bucket (the bucket is also kept across reincarnation). A Runtime Broker that receives no bucket falls back to its own GCS storage bucket setting. With neither, the create request fails up front with `422 workspace_storage_unconfigured` instead of a generic gateway error.

## Deployment

### GCE VM

The most direct path to getting a deployed demonstration hub is to use the GCE setup scripts in `/scripts/starter-hub` (the Developer Hub tier)

### Cloud Run, GKE (GCP) *Future*
The Hub is designed to be stateless and is highly compatible with Google Cloud Run. 
- Use **Cloud SQL** (PostgreSQL) for the database.
- Use **Cloud Storage** for template persistence.
- Connect the Hub to Cloud SQL using the Cloud SQL Auth Proxy or a VPC connector.

## Discord Integration

The Hub supports native Discord webhooks to broadcast persistent agent messages, notifications, and `ask_user` requests to a Discord channel.

To configure Discord notifications, set the `discord_webhook_url` in your `server` configuration block (or via the `SCION_DISCORD_WEBHOOK_URL` environment variable):

```yaml
server:
  discord_webhook_url: "https://discord.com/api/webhooks/YOUR_WEBHOOK_ID/YOUR_WEBHOOK_TOKEN"
```

Once configured, the Hub will automatically forward messages with severity-based color coding. Urgent messages or explicit requests for user input can also trigger role or user mentions, ensuring that critical requests receive immediate attention.

## Observability

The Hub supports structured logging and can forward its internal logs and traces to an OpenTelemetry-compatible backend (like Google Cloud Logging/Trace).

To enable log forwarding, set `SCION_OTEL_LOG_ENABLED=true` and `SCION_OTEL_ENDPOINT`. See the [Observability Guide](/scion/hosted/single-node/observability/) for full details on centralizing system logs and agent metrics.

## Monitoring

The Hub exposes health check endpoints:
- `/healthz`: Basic liveness check. Always `200`; the body's `status` is `healthy`, `degraded` (the server is up, but a non-critical check such as the co-located broker is failing), or `unhealthy` (a critical check — the database, or the configured shared workspace storage mount — is failing), with `checks` naming any failing subsystem. On a single-node setup (Hub + co-located runtime broker), the co-located broker check reports `healthy`, `unhealthy: registration failed`, or `unhealthy: registration pending` — a failed registration is not retried, so `status` stays `degraded` until the broker configuration is fixed and the server is restarted. The single-node workstation setup runs combined (web server + Hub on one port), so the web server answers `/healthz` and nests the Hub's own health under `hub` — the check is at `hub.checks.colocated_broker`, not top-level `checks.colocated_broker` (that path is only for a standalone Hub with no web server). `scion server status` reports a degraded server as running and names the failing checks (in either shape); an unhealthy one is reported as `unhealthy` with its checks. `scion server start` names them too, but only when it waits to open a browser (an interactive, non-headless terminal with web enabled, and `SCION_NO_BROWSER` unset): it waits up to 20 seconds for `healthy`, then opens the browser anyway with a warning if the server is still degraded. Other `start` invocations print nothing about it.
- `/readyz`: Readiness check (verifies database connectivity and, when a non-`local` workspace storage backend is configured, that its mount is available). Unaffected by the co-located broker check above — use `/readyz`, not `/healthz`, for Kubernetes/Cloud Run readiness probes.
- `/health`: Legacy/alternative liveness check endpoint.

### Reverse Proxy / GFE Interception Handling

In distributed or hosted deployments (such as Google Cloud Run behind a Google Front End), reverse proxies may intercept calls to `/healthz` and return a non-JSON response (e.g. `text/plain`). 

To prevent connectivity and status reporting issues:
- **Hub Client Fallback**: The Scion Hub client automatically detects non-JSON `2xx` responses from reverse proxies on `/healthz` and transparently falls back to `/health`. If both fail, it appends a helpful diagnostic tip suggesting a Cloud Run or GFE configuration review.
- **Broker Client Diagnostics**: If the Runtime Broker client receives a non-JSON `2xx` response on `/healthz`, it blocks the call and returns a precise error indicating that a reverse proxy (e.g., GFE) is likely intercepting `/healthz` and provides troubleshooting guidance.

Logs are output to `stdout` in either `text` (default) or `json` format, suitable for collection by systems like Fluentd, Cloud Logging, or Prometheus.
