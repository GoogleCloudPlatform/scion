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
| `read_timeout` | duration | `"30s"` | HTTP read timeout. |
| `write_timeout` | duration | `"60s"` | HTTP write timeout. |
| `admin_emails` | list | `[]` | List of emails granted super-admin access. |
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
| `auto_provide` | bool | `false` | Automatically add as provider for new groves. |

### Database (`server.database`)

Persistence settings for the Hub.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `driver` | string | `"sqlite"` | Database driver: `sqlite` or `postgres`. |
| `url` | string | `"hub.db"` | Connection string or file path. |

### Authentication (`server.auth`)

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `dev_mode` | bool | `false` | Enable insecure development authentication. |
| `dev_token` | string | | Static token for dev mode. |
| `authorized_domains` | list | `[]` | Limit access to specific email domains. |

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

:::caution
The `local` backend does not store secret values. Any attempt to create or update secrets will fail with a 501 error. Configure `gcpsm` to use the secret management features.
:::

## Environment Variables

All server settings can be overridden via environment variables using the `SCION_SERVER_` prefix and snake_case naming.

**Examples:**
- `server.hub.port` -> `SCION_SERVER_HUB_PORT`
- `server.broker.enabled` -> `SCION_SERVER_BROKER_ENABLED`
- `server.broker.container_hub_endpoint` -> `SCION_SERVER_BROKER_CONTAINERHUBENDPOINT`
- `server.database.url` -> `SCION_SERVER_DATABASE_URL`
- `server.auth.dev_mode` -> `SCION_SERVER_AUTH_DEVMODE`
- `server.secrets.backend` -> `SCION_SERVER_SECRETS_BACKEND`
- `server.secrets.gcp_project_id` -> `SCION_SERVER_SECRETS_GCP_PROJECT_ID`
- `server.secrets.gcp_credentials` -> `SCION_SERVER_SECRETS_GCP_CREDENTIALS`

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

See the [Local Development Logging guide](/scion/development/logging/) for details on log formats, request log fields, and Cloud Logging integration.

### Hub Endpoint Resolution

When `server.hub.public_url` is not explicitly set, the Hub endpoint injected into agents is resolved in this order:

1. `SCION_SERVER_HUB_PUBLIC_URL` or `server.hub.public_url` — explicit Hub public URL.
2. Grove-level `hub.endpoint` setting.
3. `SCION_SERVER_BASE_URL` — the server's public base URL (also used for OAuth redirects).
4. Auto-computed `http://localhost:{port}` (last resort).

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

**Truncation:** Discord caps embed descriptions at 2048 characters and total
embed payload at 6000 characters. Messages longer than that are truncated
with a `…(truncated)` marker — use the webhook channel type if you need the
full structured payload without truncation.

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
