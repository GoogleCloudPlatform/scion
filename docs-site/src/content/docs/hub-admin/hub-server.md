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

scion --global server start --foreground --production --debug --enable-hub --enable-runtime-broker --enable-web --runtime-broker-port 9800 --web-port 8080 --storage-bucket \${SCION_HUB_STORAGE_BUCKET} --session-secret \${SESSION_SECRET} --auto-provide

```
This is often best managed through something like systemd

### Hub vs. Broker Processes
While they can run in the same process—known as **Combo Mode** (the default for `scion server start --workstation`)—they serve distinct roles:
- **The Hub** is the stateless control plane. It provides the API and Web Dashboard, and should be accessible via a public or internal URL.
- **The Broker** is the execution host. It registers with a Hub and executes agents. Brokers can run behind NAT or firewalls, as they establish outbound connections to the Hub. You can connect multiple external brokers to a single Hub.

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

See the [Server Configuration Reference](/scion/reference/server-config/) for all available fields.

## Authentication

The Hub supports multiple end-user authentication modes to balance ease of development with production security.

### OAuth 2.0 (Production)
Scion supports Google and GitHub as identity providers. Configuration requires creating OAuth Apps in the respective provider consoles.
See the [Authentication Guide](/scion/hub-admin/auth/) for detailed setup instructions.

### Dev Auth (Local Development, workstation mode)
For local testing, the Hub can auto-generate a development token:
```yaml
server:
  auth:
    dev_mode: true
```
The token is written to `~/.scion/dev-token` on startup. The CLI and Web Dashboard automatically detect this token when running on the same machine.

### Personal Access Tokens (Programmatic)

The Hub supports long-lived Personal Access Tokens (PATs) for CI/CD or other programmatic integrations.

## GCP Identity & Hub-Minted Service Accounts

The Scion Hub can manage and provision Google Cloud Platform (GCP) Service Accounts directly. This allows agents to authenticate to Google Cloud services via metadata server emulation, avoiding the need to distribute static credential files. 

### Configuration

To enable GCP identity management, the Hub itself must run with a GCP identity (e.g., attached to its GCE instance or GKE pod) that has the `iam.serviceAccounts.getAccessToken` permission for the target Service Accounts.

Administrators can configure Service Accounts via the Web Dashboard:
1. Navigate to the **Service Accounts** section in the Admin dashboard.
2. View the service account quota dashboard and configure minting capability controls.
3. Register existing GCP Service Accounts by email, or configure the Hub to mint new ones dynamically.

### Default Project Identities

Projects can be configured with default GCP identities that are automatically verified upon registration and automatically applied in the agent creation form. 

### Security & Authorization

Administrative actions for GCP Service Account management require `project-owner` (`ActionManage`) permissions to enforce strict security boundaries. Direct API access to Hub secrets from agents is explicitly blocked to prevent credential leakage.

For more details on how agents assume these identities via metadata server emulation, see the [Authentication Guide](/scion/hub-admin/auth/#gcp-identity--metadata-emulation).

## Project Settings & Agent Limits

The Hub provides a comprehensive UI for configuring project-level settings, ensuring administrators have control over resource allocation and project configurations. Access these settings via the Web Dashboard for any project you manage.

### Configuration Tabs

The Project Settings UI is organized into three primary tabs:

- **General**: Configure the project's display name, description, and template sync settings. For git-backed projects, you can specify default branches. For hub-native projects, you can configure external git repositories to load templates from.
- **Limits**: Define constraints on agent execution to prevent resource exhaustion.
  - **Hub-level Defaults**: Administrators can configure global default limits that apply to all projects.
  - **Project-level Limits**: Overrides can be set per-project.
  - Limits automatically pre-populate the agent creation form and restrict maximum concurrency, runtime duration, and storage.
- **Resources**: Manage the compute and plugin environments available to the project.
  - **Runtime Brokers**: Link and manage the auxiliary runtimes (e.g., specific Kubernetes clusters or remote Docker hosts) authorized to execute agents for this project.
  - **Plugins**: Enable and configure message broker plugins or other extensions for agents running within the project.

### Template Synchronization

Projects support loading templates from external Git repositories, which is especially useful for non-Git-backed (hub-native) projects. The UI accepts bare host/org/repo URLs (e.g., `github.com/org/repo`) and automatically normalizes them, appending `/.scion/templates/` unless a deeper path is specified. This synchronization can be manually triggered via the UI to immediately pull the latest templates.

## Server Maintenance & Updates

The Scion Hub provides a built-in maintenance administration panel in the Web Dashboard for managing routine server operations, updates, and synchronization.

### Maintenance Panel Operations

Administrators can trigger critical infrastructure operations directly from the dashboard:

- **Check for Updates**: Checks for available updates and allows administrators to execute an "Update Now" action to perform a direct server rebuild.
- **Rebuild Server (`rebuild-server`)**: Initiates a fire-and-forget server rebuild and restart sequence. It uses staging paths and sudoers implementation to ensure reliable updates even while the server is running.
- **Rebuild Web (`rebuild-web`)**: Recompiles the web frontend assets.
- **Pull Images (`pull-images`)**: Triggers the Docker/Podman executor to pull the latest agent container images.

### Operation Execution & History

All maintenance tasks executed through the panel are tracked. The maintenance interface provides:
- Real-time log capture for active operations.
- Execution history detailing operation duration, completion status, and log archives.
- Automated cleanup of stalled operations during server startup.

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

## Deployment

### GCE VM

The most direct path to getting a deployed demonstration hub, is to use the GCE setup scripts in `/scripts/starter-hub`

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

To enable log forwarding, set `SCION_OTEL_LOG_ENABLED=true` and `SCION_OTEL_ENDPOINT`. See the [Observability Guide](/scion/hub-admin/observability/) for full details on centralizing system logs and agent metrics.

## Monitoring

The Hub exposes health check endpoints:
- `/healthz`: Basic liveness check.
- `/readyz`: Readiness check (verifies database connectivity).

Logs are output to `stdout` in either `text` (default) or `json` format, suitable for collection by systems like Fluentd, Cloud Logging, or Prometheus.
