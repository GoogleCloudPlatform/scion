---
title: Server Configuration (Hub & Runtime Broker)
description: Configuration reference for Scion Hub and Runtime Broker services.
---

This document describes the configuration for the Scion Hub (State Server) and the Scion Runtime Broker.

## Configuration Location

Server configuration is defined in the `server` section of your `settings.yaml` file.

- **Primary**: `~/.scion/settings.yaml` (Global settings)
- **Legacy**: `~/.scion/server.yaml` (Deprecated, but supported as fallback)

:::tip[Migration]
If you are using `server.yaml`, you can migrate it to `settings.yaml` using:
`scion config migrate --server`
:::

## Structure

```yaml
schema_version: "1"
server:
  env: prod
  log_level: info
  
  hub:
    port: 9810
    host: "0.0.0.0"
    public_url: "https://hub.scion.dev"
    
  broker:
    enabled: true
    port: 9800
    broker_id: "generated-uuid"
    
  database:
    driver: sqlite
    url: "hub.db"
    
  auth:
    dev_mode: false
```

## Section Reference

### Hub Settings (`server.hub`)

Controls the central Hub API server.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `port` | int | `9810` | HTTP port to listen on (standalone mode). In combined mode (`--enable-web`), the Hub API is served on the web port instead and this setting is ignored. |
| `host` | string | `"0.0.0.0"` | Network interface to bind to. |
| `public_url` | string | | The externally accessible URL of the Hub (used for callbacks). |
| `gcp_project_id` | string | | GCP project ID used for minting GCP Service Accounts. Auto-detected if running on GCE/Cloud Run. |
| `gcp_iam_check_mode` | string | `"off"` | Controls whether IAM `actAs` permission is checked when binding a GCP service account to an agent. Supported values: `"off"` (no check; default) or `"enforce"` (uses Policy Troubleshooter to enforce `iam.serviceAccounts.actAs`). See the security/permissions reference for details on roles and caches. |
| `gcp_iam_deny_unknown_policy` | string | `"fail-open"` | Behavior when Policy Troubleshooter cannot evaluate deny policies (e.g. if the Hub lacks org-level reviewer roles). Supported values: `"fail-open"` (allow if no explicit deny is found; default) or `"fail-closed"` (treat as indeterminate and deny). |
| `read_timeout` | duration | `"30s"` | HTTP read timeout. |
| `write_timeout` | duration | `"60s"` | HTTP write timeout. |
| `admin_emails` | list | `[]` | List of emails granted super-admin access. Additive only: listed users are promoted to admin on login, but the list never demotes or rewrites a role already stored in the database (e.g. `admin` or `viewer` set from the admin UI). Changing a role is an explicit admin action. |
| `soft_delete_retention` | duration | | Duration to retain soft-deleted agents (e.g., `"72h"`). |
| `soft_delete_retain_files` | bool | `false` | Preserve workspace files during the soft-delete period. |
| `cors` | object | | CORS configuration (see below). |

#### CORS (`server.hub.cors`)

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `true` | Enable CORS. |
| `allowed_origins` | list | `["*"]` | Allowed origins. |

### Broker Settings (`server.broker`)

Controls the Runtime Broker service.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Whether to start the broker service. |
| `port` | int | `9800` | HTTP port to listen on. |
| `broker_id` | string | | Unique UUID for this broker. |
| `broker_name` | string | | Human-readable name. |
| `broker_nickname` | string | | Short display name. |
| `hub_endpoint` | string | | The Hub URL this broker connects to. |
| `container_hub_endpoint` | string | | Overrides `hub_endpoint` when injecting the Hub URL into agent containers. Use when containers cannot reach the Hub at the broker's address (e.g. `http://host.containers.internal:8080` for local development). |
| `broker_token` | string | | Authentication token for the Hub. |
| `auto_provide` | bool | `false` | Automatically add as provider for new projects. |

### Database (`server.database`)

Persistence settings for the Hub.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `driver` | string | `"sqlite"` | Database driver: `sqlite` or `postgres`. |
| `url` | string | `"hub.db"` | Connection string or file path. |

### Authentication (`server.auth`)

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `mode` | string | `"oauth"` | Selects the exclusive human auth mode: `"oauth"` (default), `"proxy"`, or `"dev"`. |
| `dev_mode` | bool | `false` | Enable insecure development authentication (used in `"dev"` mode). |
| `dev_token` | string | | Static token for dev mode. |
| `authorized_domains` | list | `[]` | Limit access to specific email domains. |

### Proxy Auth (`server.auth.proxy`)

Proxy authentication configuration (consulted when `server.auth.mode` is set to `"proxy"`). See [Proxy Auth (Google IAP)](/scion/hosted/ha/auth-proxy-iap/) for the full deployment guide.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `provider` | string | | Selects the proxy auth provider: `"iap"` or `"header"`. |
| `require_trusted_proxy_ip` | bool | `false` | Enables defense-in-depth IP allowlisting. Uses the trusted_proxies CIDR list. |

#### Google IAP Settings (`server.auth.proxy.iap`)

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `audience` | string | | **MANDATORY for IAP.** The expected audience claim (`aud`) in the IAP-signed JWT assertion. Supported formats are Cloud Run native path or GCE/GKE GCLB backend service path. |
| `issuer` | string | `"https://cloud.google.com/iap"` | The expected JWT issuer. Override only for mock/testing setups. |
| `jwks_url` | string | `"https://www.gstatic.com/iap/verify/public_key-jwk"` | The URL to retrieve public keys for signature verification. Override only for testing. |

### Transport Auth (`server.auth.transport`)

Transport auth configuration for the platform guard (IAP or Cloud Run invoker). See [Proxy Auth (Google IAP)](/scion/hosted/ha/auth-proxy-iap/) for the full deployment guide.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `mode` | string | `"none"` | Transport auth mode: `none`, `iap`, or `cloudrun_invoker`. |
| `oidc_audience` | string | | OIDC audience for transport tokens. For `iap`: the IAP OAuth client ID. For `cloudrun_invoker`: the Hub URL (auto-derived from `hub.public_url` if empty). |
| `platform_auth_sa` | string | | Dedicated service account the Hub impersonates to mint OIDC ID tokens for agents. |

#### Agent transport environment variables

When transport auth is configured, the Hub injects these environment variables into agent containers at dispatch time:

| Variable | Description |
| :--- | :--- |
| `SCION_TRANSPORT_TOKEN` | Initial Google OIDC ID token for the transport layer. |
| `SCION_TRANSPORT_AUDIENCE` | Audience the transport token was minted for. |
| `SCION_TRANSPORT_TOKEN_EXPIRY` | Token expiry in RFC 3339 format. |
| `SCION_TRANSPORT_MODE` | Transport mode (`iap` or `cloudrun_invoker`). Injected alongside the other three transport vars so that in-agent clients can select the correct header placement. |

#### Broker transport configuration

Brokers are long-lived originators that mint their own OIDC tokens (via GKE Workload Identity or ambient GCE SA). Transport settings are configured via environment variables or per-connection credentials-file fields.

**Environment variables** (for containerized brokers):

| Variable | Description |
| :--- | :--- |
| `SCION_TRANSPORT_MODE` | Transport mode: `iap` or `cloudrun_invoker`. |
| `SCION_TRANSPORT_AUDIENCE` | OIDC audience — the custom OAuth 2.0 Client ID (for `iap`) or Hub URL (for `cloudrun_invoker`). |

**Credentials-file fields** (per hub connection, persisted by `scion hub brokers register`):

| Field | Type | Description |
| :--- | :--- | :--- |
| `transportMode` | string | Transport mode: `iap` or `cloudrun_invoker`. |
| `transportAudience` | string | OIDC audience for the transport token. |

Environment variables override credentials-file values. Per-connection credentials-file fields support the multi-hub scenario where each hub has a different IAP OAuth client ID.

### OAuth (`server.oauth`)

OAuth provider credentials.

```yaml
server:
  oauth:
    web:
      google: { client_id: "...", client_secret: "..." }
      github: { client_id: "...", client_secret: "..." }
    cli:
      google: { client_id: "...", client_secret: "..." }
```

### Storage (`server.storage`)

Backend for storing templates and artifacts.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `provider` | string | `"local"` | Storage provider: `local` or `gcs`. |
| `bucket` | string | | GCS bucket name. |
| `local_path` | string | | Local path for storage. |

### Secrets (`server.secrets`)

Backend for managing encrypted secrets. The `local` backend is read-only and rejects secret write operations. Configure `gcpsm` to enable full secret management.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `backend` | string | `"local"` | Secrets backend: `local` or `gcpsm`. The `local` backend rejects writes; use `gcpsm` for production. |
| `gcp_project_id` | string | | GCP Project ID for Secret Manager. Required when `backend` is `gcpsm`. |
| `gcp_credentials` | string | | Path to GCP service account JSON or the JSON content itself. Optional if using Application Default Credentials. |

### Workspace Storage (`server.workspace_storage`)

Configures the backend and mount settings for storing and managing agent workspaces. This is a critical setting for high-availability deployments where multiple Hub and Broker replicas need shared, durable access to project workspaces.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `backend` | string | `"local"` | Storage backend pivot: `"local"` (node-local directories), `"nfs"` (Network File System mounts), `"cloudrun-volume"` (Cloud Run platform-managed volume mounts), or `"gke-shared-volume"` (GKE shared CSI-backed PVC mounts). |
| `nfs.mount_root` | string | | The host base directory under which NFS exports are mounted. |
| `nfs.mount_options` | string | `"vers=3,hard,nconnect=4,_netdev"` | Standard mount options passed to the `mount.nfs` utility. |
| `nfs.uid` | integer | `1000` | Node-independent owner UID for NFS-backed workspace trees to ensure consistent container write permissions. |
| `nfs.gid` | integer | `1000` | Node-independent owner GID for NFS-backed workspace trees. |
| `nfs.storage_class` | string | | The Kubernetes StorageClass name used to dynamically allocate volumes on GKE. |
| `nfs.subpath_root` | string | `"projects"` | The default base folder name within the share for project workspaces. |
| `nfs.shares` | list of objects | `[]` | List of NFS share objects. Each share requires: `id` (stable ID), `server` (IP address or hostname), `export` (exported path, e.g., `/scion-workspaces`), and optional `pv_name` (for GKE). |
| `cloudrun_volume.volume_name` | string | | The name of the platform volume declared in the Cloud Run service specification. The Hub resolves workspaces under `/mnt/<volume_name>`, which is where Cloud Run mounts a declared volume. |
| `cloudrun_volume.subpath_root` | string | `"projects"` | Sub-directory prefix within the Cloud Run volume. |
| `gke_shared_volume.volume_name` | string | | The K8s volume name referencing the persistent volume claim (PVC). **The pod spec must mount that volume at `/mnt/<volume_name>`**: the Hub derives every workspace path from it, and a pod that mounts the PVC elsewhere fails readiness (`GET /readyz` returns `503`) rather than writing workspaces to ephemeral container storage. |
| `gke_shared_volume.pv_claim_name` | string | | The name of the GKE-managed PVC bound to the shared storage backend (e.g. Filestore). |
| `gke_shared_volume.subpath_root` | string | `"projects"` | Sub-directory prefix within the GKE volume. |

#### Ephemeral Storage & 503 Safety Gate

To protect deployments from silent data loss, the Hub implements a strict **503 Safety Gate**:
* If the Hub is deployed on serverless environments like Google Cloud Run with the `local` backend selected, its local workspace paths map to ephemeral, non-durable container storage.
* The Hub detects this non-durable state and automatically intercepts all file write and modification endpoints (including WebDAV, inline file editing, and git cloning).
* Affected endpoints will return `503 Service Unavailable` with a descriptive message rather than allowing writes to persist ephemerally on the container's scratch space, enforcing the transition to a durable backend (`nfs`, `cloudrun-volume`, or `gke-shared-volume`) for production.

### Scheduler (`server.scheduler`)

Controls the background task scheduler in the Hub. This regulates the tick interval and concurrency of recurring maintenance tasks (such as telemetry aggregation, session cleanups, and heartbeats) to match database capacity.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `interval_seconds` | integer | `60` | The root ticker interval in seconds. All recurring background tasks fire at multiples of this interval. Increasing this value reduces database connection pressure on smaller deployments. |
| `max_concurrency` | integer | `2` | Limits the number of recurring maintenance tasks that can execute concurrently in a single tick. By default, this is capped at `2` to avoid database connection pool saturation. Set to `0` for unlimited concurrency (legacy behavior) or a higher value for larger deployments. |

:::note[Database Stability]
Configuring a modest concurrency limit (such as the default `2`) is highly recommended for small or single-node database instances to prevent sudden spikes in database connection usage.
:::

### OIDC Identity Provider (`server.oidc`)

Configuration for the Hub's built-in OIDC Identity Provider feature.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Enable the OIDC Identity Provider endpoints. |
| `issuer_url` | string | | The public issuer URL of this Hub. If empty, the hub public URL is used. |
| `token_lifetime` | duration | `"15m"` | Validity duration for minted OIDC identity tokens (e.g. `"15m"`, `"1h"`). |

### OIDC Federation (`server.federation`)

Configuration for inbound OIDC-based federation authentication.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Enable OIDC federation authentication. |
| `trusted_issuers` | list of objects | `[]` | List of trusted OIDC issuers (see below). |
| `algorithms` | list of strings | `["RS256"]` | Supported cryptographic signing algorithms. |
| `cache.refresh_interval` | duration | `"1h"` | How often to refresh cached issuer public keys (JWKS). |
| `cache.debounce_interval` | duration | `"1s"` | Min interval between JWKS reload attempts to prevent DDOS. |

#### Trusted Issuer Settings (`server.federation.trusted_issuers[]`)

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `issuer_url` | string | | **MANDATORY.** The exact OIDC issuer URL (matching token `iss` claim). |
| `jwks_url` | string | | The URL to fetch signing public keys. Discovered via OIDC discovery if empty. |
| `expected_audience` | string | | The expected audience `aud` claim in tokens. |
| `allowed_projects` | list of strings | | If set, restricts tokens to specific project UUIDs. |
| `allowed_root_users` | list of strings | | If set, restricts tokens to specific root user emails. |
| `default_scopes` | list of strings | | Default JWT scopes granted to federated agents. |
| `issuer_type` | string | `"hub"` | Type of issuer: `"hub"`, `"service_account"`, or `"user"`. |
| `default_role` | string | `"viewer"` | Default role for federated users (`issuer_type: user`). |
| `allowed_emails` | list of strings | | Restrict user tokens to specific email claims (supports wildcards e.g. `*@example.com`). |

### OIDC Login (`server.oidc_login`)

Configuration for an external OIDC provider used for Web UI user login.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Enable the external OIDC login provider. |
| `display_name` | string | | The human-readable label shown on the login button (e.g. `"Corporate SSO"`). |
| `issuer_url` | string | | The exact OIDC issuer URL. Used to perform discovery via `{issuer_url}/.well-known/openid-configuration`. |
| `client_id` | string | | The OAuth2/OIDC Client ID. |
| `client_secret` | string | | The OAuth2/OIDC Client Secret (can be empty for public OIDC clients). |
| `scopes` | list of strings | `["openid", "email", "profile"]` | Overrides the default scopes requested during login. |

### Project Defaults (`project_defaults`)

Configuration for project-level default behaviors across the Hub. Unlike most other server configurations, `project_defaults` is declared as a **top-level section** in `settings.yaml` (outside of the `server:` block).

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `default_scratchpad` | bool | `true` | If enabled, automatically provisions a default `scratchpad` shared directory when a new project is created. |

**Example:**
```yaml
# Declared at the top level of settings.yaml
project_defaults:
  default_scratchpad: true
```

## Environment Variables

:::tip[Database Mode]
When running with a postgres database, operational settings (Layer-1) can be configured via `SCION_SEED_*` environment variables and managed in the admin UI. See the [Admin Settings Model](/reference/admin-settings/) for details on the seeded/managed lifecycle and the `SCION_SEED_*` namespace.
:::

All server settings can be overridden via environment variables using the `SCION_SERVER_` prefix and snake_case naming.

**Examples:**
- `server.hub.port` -> `SCION_SERVER_HUB_PORT`
- `server.hub.gcp_project_id` -> `SCION_SERVER_HUB_GCPPROJECTID`
- `server.hub.gcp_iam_check_mode` -> `SCION_SERVER_HUB_GCPIAMCHECKMODE`
- `server.hub.gcp_iam_deny_unknown_policy` -> `SCION_SERVER_HUB_GCPIAMDENYUNKNOWNPOLICY`
- `server.broker.enabled` -> `SCION_SERVER_BROKER_ENABLED`
- `server.broker.container_hub_endpoint` -> `SCION_SERVER_BROKER_CONTAINERHUBENDPOINT`
- `server.database.url` -> `SCION_SERVER_DATABASE_URL`
- `server.auth.dev_mode` -> `SCION_SERVER_AUTH_DEVMODE`
- `server.secrets.backend` -> `SCION_SERVER_SECRETS_BACKEND`
- `server.secrets.gcp_project_id` -> `SCION_SERVER_SECRETS_GCPPROJECTID`
- `server.secrets.gcp_credentials` -> `SCION_SERVER_SECRETS_GCPCREDENTIALS`
- `server.scheduler.interval_seconds` -> `SCION_SERVER_SCHEDULER_INTERVAL_SECONDS`
- `server.scheduler.max_concurrency` -> `SCION_SERVER_SCHEDULER_MAX_CONCURRENCY`

### Logging Environment Variables

These environment variables control server-side logging behavior. They are not part of the `settings.yaml` structure.

| Variable | Description | Default |
| :--- | :--- | :--- |
| `SCION_LOG_GCP` | Enable GCP Cloud Logging JSON format on stdout | `false` |
| `SCION_LOG_LEVEL` | Log level: `debug`, `info`, `warn`, `error` | `info` |
| `SCION_CLOUD_LOGGING` | Send logs directly to Cloud Logging via client library | `false` |
| `SCION_CLOUD_LOGGING_LOG_ID` | Log name in Cloud Logging for application logs | `scion` |
| `SCION_GCP_PROJECT_ID` | GCP project ID for Cloud Logging (priority 1) | auto-detect |
| `GOOGLE_CLOUD_PROJECT` | GCP project ID for Cloud Logging (priority 2) | - |
| `SCION_SERVER_REQUEST_LOG_PATH` | Write HTTP request logs to a file at this path. Each line is a JSON object in `HttpRequest` format. When not set, request logs follow the default routing (stdout in background mode, suppressed in foreground mode, Cloud Logging when enabled). | (disabled) |

See the [Local Development Logging guide](/scion/contributing/logging/) for details on log formats, request log fields, and Cloud Logging integration.

### Boolean Environment Variable Parsing (`parseBoolEnv`)

For server and infrastructure configurations, Scion parses several boolean environment variables using a robust, operator-friendly `parseBoolEnv` parser:

- **Supported Truthy Values:** `true`, `1`, `t`, `yes`, `y`, `on` (case-insensitive, whitespace-trimmed).
- **Supported Falsy Values:** `false`, `0`, `f`, `no`, `n`, `off` (case-insensitive, whitespace-trimmed).
- **Safety Warnings:** Unset, empty, or unparseable values default to `false`. However, to prevent configuration typos from silently disabling critical features, any **unrecognized non-empty value** (e.g., `SCION_LOG_GCP=trur`) will trigger an explicit **warning at startup** and default to `false`.

#### Tracked Boolean Variables:

| Variable | Description | Default |
| :--- | :--- | :--- |
| `SCION_SERVER_ADMIN_MODE` | Forces the server into emergency maintenance mode (break-glass removal). | `false` |
| `SCION_TRACING_ENABLED` | Enables OpenTelemetry tracing when a GCP Project ID is configured. | `false` |
| `SCION_LOG_GCP` | Enables Google Cloud Logging JSON format on standard output. | `false` |
| `SCION_REQUIRE_STABLE_SIGNING_KEY` | Demands a persistent session/JWT signing key. When enabled, startup aborts (fail-closed) if no stable key/secret can be resolved in hosted mode, preventing JWT signature mismatches across replica restarts. | `false` |

### Hub Endpoint Resolution

When `server.hub.public_url` is not explicitly set, the Hub endpoint injected into agents is resolved in this order:

1. `SCION_SERVER_HUB_PUBLIC_URL` or `server.hub.public_url` — explicit Hub public URL.
2. Project-level `hub.endpoint` setting.
3. `SCION_SERVER_BASE_URL` — the server's public base URL (also used for OAuth redirects).
4. **IAP Audience Derivation** (in Hosted HA mode with IAP authentication):
   - For **Cloud Run** IAP audiences (`/projects/<number>/locations/<region>/services/<service>`), Scion can auto-derive the Hub's URL using the legacy Cloud Run URL format (`https://<service>-<number>.<region>.run.app`). Newer Cloud Run services use a different URL format (`https://<service>-<hash>-<region>.a.run.app`) where the hash cannot be derived from the project number — for those services, set `SCION_SERVER_BASE_URL` explicitly instead of relying on auto-derivation.
   - For **GKE/GCLB** backend-service IAP audiences (`/projects/<number>/global/backendServices/<id>`), a URL cannot be derived from the ID. If `SCION_SERVER_BASE_URL` (or other explicit URL settings) is not set, Scion will log a warning at startup and fall back to `localhost`, which is likely unreachable from dispatched agents.
5. Auto-computed `http://localhost:{port}` (last resort).

For local development where the Hub runs on `localhost` but agents are in containers, set `server.broker.container_hub_endpoint` to a container-accessible address like `http://host.containers.internal:8080`.

## Notification channels

Notification channels deliver agent messages to external systems. Configure them
under `server.hub.notification_channels` as a list of channel objects. Each object
has a `type`, a `params` map, and optional filters.

```yaml
server:
  hub:
    notification_channels:
      - type: <channel-type>
        params:
          # channel-specific key/value pairs
        filter_urgent_only: false   # if true, only deliver urgent messages
        filter_types:               # if set, only deliver these message types
          - input-needed
          - state-change
```

### Slack channel

Delivers notifications via a Slack incoming webhook using Slack's `text` payload
format.

**Type:** `slack`

**Parameters:**

| Param              | Required | Description |
|--------------------|----------|-------------|
| `webhook_url`      | yes      | Slack incoming webhook URL (must use `https://`). |
| `channel`          | no       | Override the webhook's default channel. |
| `mention_on_urgent`| no       | Mention string added when `msg.Urgent == true` (e.g. `@here`, `@channel`). |

**Example:**

```yaml
notification_channels:
  - type: slack
    params:
      webhook_url: https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXX
      mention_on_urgent: "@here"
```

### Webhook channel

Delivers notifications as a raw HTTP POST to an arbitrary URL. Use this when you
need the full structured payload without truncation or when integrating with a
custom receiver.

**Type:** `webhook`

**Parameters:**

| Param         | Required | Description |
|---------------|----------|-------------|
| `webhook_url` | yes      | Destination URL (must use `https://`). |

**Example:**

```yaml
notification_channels:
  - type: webhook
    params:
      webhook_url: https://example.com/scion-notifications
```

### Email channel

Delivers notifications by email.

**Type:** `email`

**Parameters:**

| Param    | Required | Description |
|----------|----------|-------------|
| `to`     | yes      | Recipient email address. |
| `from`   | no       | Sender address override. |
| `smtp`   | no       | SMTP server host:port. |

**Example:**

```yaml
notification_channels:
  - type: email
    params:
      to: oncall@example.com
```

### Discord channel

Delivers notifications via a Discord incoming webhook using Discord's native
webhook format (rich embeds, colour-coded severity, allowed-mentions-controlled
role/user pings). Unlike the Slack channel, the Discord channel targets the
Discord-native endpoint — the `/slack`-compatibility suffix is explicitly
rejected because it dilutes what each channel type means and silently hides
the user's real intent.

**Type:** `discord`

**Parameters:**

| Param               | Required | Description |
|---------------------|----------|-------------|
| `webhook_url`       | yes      | Discord incoming webhook URL. Must use `https://` and one of the allowed Discord hosts: `discord.com`, `discordapp.com`, `ptb.discord.com`, `canary.discord.com`. Path must begin with `/api/webhooks/` and must not end with `/slack`. |
| `mention_on_urgent` | no       | Mention string applied when `msg.Urgent == true`. Use Discord mention syntax: `<@&ROLE_ID>` for a role, `<@USER_ID>` for a user. `@here` and `@everyone` are intentionally **not** supported — the channel sets `allowed_mentions.parse: []` so Discord will not resolve them even if present. |
| `username`          | no       | Override the webhook's default username for delivered messages. |
| `avatar_url`        | no       | Override the webhook's default avatar for delivered messages. |

**Embed colours by message type:**

| Type                  | Colour | Hex       |
|-----------------------|--------|-----------|
| `state-change`        | blue   | `#3498db` |
| `input-needed`        | yellow | `#f1c40f` |
| `instruction`         | grey   | `#95a5a6` |
| *(urgent — any type)* | red    | `#e74c3c` (overrides the type colour) |

**Truncation:** Discord caps embed descriptions at 2048 characters. Messages
longer than that are truncated with a `…(truncated)` marker — use the webhook
channel type if you need the full structured payload without truncation.

**Example:**

```yaml
notification_channels:
  - type: discord
    params:
      webhook_url: https://discord.com/api/webhooks/123456789012345678/abcDEFghiJKLmnoPQR_stu
      mention_on_urgent: "<@&987654321098765432>"
      username: Scion Hub
    filter_urgent_only: false
    filter_types:
      - input-needed
      - state-change
```

:::note[Migrating from a Slack-compat Discord webhook]
Earlier scion releases had no Discord channel type — operators could route
notifications to a Discord webhook by using `type: slack` with a webhook URL
ending in `/slack` (Discord's Slack-compatibility endpoint). That approach
produces plain-text messages with no embeds, colours, or mentions.

To migrate:

1. Remove the `/slack` suffix from the webhook URL.
2. Change `type: slack` to `type: discord`.
3. If you previously used `mention_on_urgent: "@here"`, replace it with a
   Discord role mention (`"<@&ROLE_ID>"`) — `@here` is not supported via the
   native Discord webhook format (the channel sets `allowed_mentions.parse: []`
   which prevents Discord from resolving `@here` and `@everyone`).
4. Reload the hub config. Validation will reject the old `/slack`-suffixed
   URL so a misconfiguration will surface on startup rather than silently
   falling back.
:::

## Two-Tier Settings Architecture (HA Deployments)

In HA deployments where multiple Hub replicas share a Postgres database, settings are split into two tiers to prevent node drift while keeping bootstrap settings file-managed.

### Layer 0 — Bootstrap (file + env only)

Settings required before the database connection exists, or that are restart-bound. Managed exclusively via `settings.yaml` and `SCION_SERVER_*` environment variables. **Cannot be written via the admin API** — `PUT /api/v1/admin/server-config` returns `422` if any Layer-0 key is present.

| Group | Keys (`server.` prefix unless noted) |
| :--- | :--- |
| Database | `database.*` |
| Listeners | `hub.port`, `hub.host`, `hub.read_timeout`, `hub.write_timeout`, `broker.*` |
| Auth stack | `auth.mode`, `auth.dev_mode`, `auth.dev_token`, `auth.dev_token_file`, `auth.proxy.*`, `auth.transport.*`, `oauth.*`, `oidc_login.*` |
| Secrets/storage | `secrets.*`, `storage.*`, `workspace_storage.*` |
| Identity/mode | `mode`, `env`, `hub.hub_id`, `hub.gcp_project_id` |
| Logging | `log_level`, `log_format` |
| CORS | `hub.cors.*`, `broker.cors` |
| Messaging/plugins | `message_broker.*`, `plugins.*` |

### Layer 1 — Operational (Postgres `hub_settings` table)

Settings that can be changed at runtime and are shared across all replicas. Stored as section-per-row in the `hub_settings` table. In SQLite/workstation mode, these fall back to `settings.yaml` (unchanged behavior), except for the `maintenance` section which is runtime/API-only and has no `settings.yaml` representation (ephemeral in file/SQLite mode).

| Section | Contents |
| :--- | :--- |
| `access` | `admin_emails`, `user_access_mode`, `authorized_domains` |
| `lifecycle` | `auto_suspend_stalled`, `soft_delete_retention`, `soft_delete_retain_files` |
| `maintenance` | `admin_mode`, `maintenance_message` (durable + cluster-wide) |
| `telemetry` | Full `telemetry.*` subtree (enabled, cloud, hub, local, filter, resource) |
| `agent_defaults` | `default_template`, `default_harness_config`, `default_max_turns`, `default_max_model_calls`, `default_max_duration`, `default_resources`, `default_model`, `default_thinking_level`, `default_max_agent_role`, `default_agent_role` |
| `federation` | `enabled`, `trusted_issuers[]`, `algorithms`, `refresh_interval`, `debounce_interval` |
| `endpoints` | `hub.public_url`, `image_registry` |
| `github_app` | `app_id`, `api_base_url`, `webhooks_enabled`, `installation_url`, `private_key_path` |
| `notifications` | `notification_channels[]` |
| `project_defaults` | `default_scratchpad` |
| *(reserved)* `global_defaults` | Reserved for future hub-resource design — not implemented |

### Precedence

In Postgres mode, the effective value for any Layer-1 key is resolved in this order (highest priority first):

1. **`SCION_SERVER_*` environment variable** — node-local escape hatch
2. **`hub_settings` DB row** — cluster-shared, set via admin API
3. **`settings.yaml` Layer-1 fields** — fallback when key absent in DB
4. **Compiled defaults**

### Seeding and Migration

- **First startup**: the first replica to start seeds `hub_settings` from its local `settings.yaml` (Layer-1 keys only) under an advisory lock. Subsequent replicas see the seed marker and skip.
- **Seeding reads file values only** — environment overrides are not baked into shared state.
- **DB wins**: once a section is seeded/written to DB, the DB row fully owns that section. Omitted fields within the section fall to compiled defaults, not to the file.
- **Rollback safety**: older builds ignore the `hub_settings` table entirely and read files — rolling back reverts to pre-change behavior.

### Environment Override Warnings

Because env overrides on Layer-1 keys reintroduce per-node drift, the system warns administrators:

- `GET /api/v1/admin/server-config` includes an `env_overrides` array listing which Layer-1 keys are overridden by env vars on the serving node.
- A startup `WARN` log lists any overridden Layer-1 keys.
- The admin UI renders a visible warning banner when env overrides are detected.

### Admin API Behavior Notes

**PUT partitioning**: The request body is partitioned by the section registry. Layer-1 fields (including `runtimes`, `profiles`, and `harness_configs`) are written to DB sections in the `hub_settings` table as whole-map JSONB documents. Layer-0 fields trigger a `422` rejection. Unclassified fields (non-registered settings) are ignored and reported in `ignored_keys`.

**Revision CAS**: The request body may include `expected_revisions` — a map of section name to expected revision number. On mismatch, the response is `409 Conflict` with the conflicting sections and their current revisions. Omitted sections use last-writer-wins semantics. Sections are written in alphabetical order for deterministic partial-apply behavior.

**Presence-aware clearing**: The PUT handler distinguishes **omitted** fields (preserve current DB value) from **explicitly-sent empty values** (`""`, `[]`, `null`) which **clear** the field. This enables clearing admin_emails, user_access_mode, authorized_domains, notification_channels, and public_url without sending every field.

**Maintenance durability**: `PUT /api/v1/admin/maintenance` writes to the `maintenance` section in DB, making admin/maintenance mode durable across restarts and propagated to all replicas. `SCION_SERVER_ADMINMODE` env var still force-enables per node for break-glass access. In file/SQLite mode, maintenance changes are ephemeral (in-memory only, lost on restart). Use `SCION_SERVER_ADMINMODE=true` env var for persistent control.

**Schema endpoint**: `GET /api/v1/admin/server-config/schema` returns JSON-schema fragments and koanf key paths per section for UI form generation and CLI validation.

:::caution[Go Zero-Value Limitations]
Due to Go's `omitempty` JSON behavior, boolean `false` is indistinguishable from an omitted field in some contexts. This affects:

- `auto_suspend_stalled` (Layer 1, lifecycle section) — `false` may be treated as omitted
- `github_app.webhooks_enabled` (Layer 1, github_app section) — `false` may be treated as omitted

When these fields are explicitly set to `false` in the DB, they are correctly applied via the snapshot. However, the raw JSON representation may omit them. The admin API handles this correctly via the presence-aware clearing mechanism.
:::

## GCP IAM Check Mode

The `gcp_iam_check_mode` setting controls whether the Hub verifies that a caller holds the `iam.serviceAccounts.actAs` IAM permission on a GCP service account before allowing it to be assigned to an agent. This uses the [GCP Policy Troubleshooter v3 API](https://cloud.google.com/policy-intelligence/docs/troubleshoot-access).

### Values

| Value | Behaviour |
| :--- | :--- |
| `"off"` (default) | No IAM check. Any member who can see a service account can assign it. Assignment is gated by Hub policy only. |
| `"enforce"` | The Hub calls Policy Troubleshooter to verify the caller has `actAs`. Denials are enforced. |

### Configuration

```yaml
# settings.yaml
server:
  hub:
    gcp_iam_check_mode: "off"   # or "enforce"
```

Or via environment variable:

```bash
export SCION_SERVER_HUB_GCPIAMCHECKMODE=enforce
```

### Enablement Checklist

Before setting `gcp_iam_check_mode: enforce`:

1. **Enable the Policy Troubleshooter API** on the Hub's GCP project:
   ```bash
   gcloud services enable policytroubleshooter.googleapis.com
   ```

2. **Grant the Hub SA `roles/iam.securityReviewer`**:
   - On the Hub's own GCP project (minimum; covers Hub-minted SAs).
   - On each org or project containing BYOSA service accounts, if applicable.

3. **(Recommended)** Grant `roles/iam.denyReviewer` and `roles/browser` at the org level for full deny-policy and resource hierarchy evaluation.

4. **(Optional)** Configure domain-wide delegation with Workspace `groups.read` for group-binding resolution. Without this, group-granted `serviceAccountUser` bindings produce an indeterminate result (denied by default).

5. **Test with a known-good SA assignment** before enabling in production.

### Group-Binding Limitation

When `roles/iam.serviceAccountUser` is granted to a Google Workspace **group**, the Hub SA must have domain-wide delegation with the `groups.read` privilege to resolve the membership. Without it, Policy Troubleshooter returns `MEMBERSHIP_UNKNOWN_INFO`, which under fail-closed rules is treated as a denial.

This denies legitimately authorized users whose `actAs` grant arrives via a group binding, even when the PT API is fully available and functioning correctly.

| Mitigation | Cost | Resolves groups? |
| :--- | :--- | :--- |
| Grant Hub SA domain-wide delegation + `groups.read` | High (org-admin consent per Workspace) | Yes |
| Grant `actAs` directly to users, not via groups | Low (per-SA IAM binding) | Avoided |
| Leave `gcp_iam_check_mode: "off"` | Zero | N/A (check disabled) |

### BYOSA Cross-Org Access

For BYOSA service accounts (accounts in a customer's org, not the Hub's), the Hub SA needs `roles/iam.securityReviewer` in the customer's organisation or at minimum on the project containing the SA. Some customers may refuse this grant. Their options are:

1. Leave `gcp_iam_check_mode: "off"` (the default).
2. Grant `securityReviewer` on the specific project (not org-wide).
3. Accept that BYOSA assignments will fail closed until the grant is made.

### Required IAM Permissions Summary

| Role / Permission | Scope | Purpose | Required? |
| :--- | :--- | :--- | :--- |
| `roles/iam.securityReviewer` | Org or project containing the target SA | Read allow policies, role bindings, and role definitions | **Yes** |
| `roles/iam.denyReviewer` | Org or folder | Read IAM Deny policies | Recommended |
| `roles/browser` | Org | Read project/folder hierarchy for policy inheritance | Recommended |
| Workspace Admin `groups.read` (via domain-wide delegation) | Google Workspace domain | Resolve group memberships in IAM bindings | Only if group-granted actAs must be resolved |
| PT API enabled on Hub's GCP project | Hub's GCP project | `policytroubleshooter.googleapis.com` | **Yes** |
