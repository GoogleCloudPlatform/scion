# scion-plugin-teams

Microsoft Teams message broker plugin for the Scion hub. Provides bidirectional messaging between Teams channels/conversations and Scion agents via the Azure Bot Framework.

**Two operating modes:**
- **Plugin mode** (default) — runs as a [go-plugin](https://github.com/hashicorp/go-plugin) subprocess managed by the hub
- **Standalone mode** — runs as an independent gRPC service with HA support via Postgres advisory locks

**Outbound:** Hub publishes `StructuredMessage`s -> plugin formats them as Adaptive Cards and sends them to linked Teams conversations via the Bot Connector REST API, with card-level agent attribution (agent name in card header).
**Inbound:** Teams messages (via Bot Framework webhook) -> plugin validates the JWT, converts Activities to `StructuredMessage`s -> delivered to agents via the hub's inbound endpoint.

## Quick Start

1. **Create an Azure App Registration** — get your Application (Client) ID, Tenant ID, and App Secret
2. **Create an Azure Bot Resource** and set the Messaging Endpoint to `https://<your-domain>/api/messages`
3. **Enable the Teams channel** on the Azure Bot resource
4. **Configure the plugin** via the Scion Admin UI and download the app package
5. **Upload the app package** to Teams Admin Center or sideload it in Teams
6. **Add the bot to a Teams channel** and run `@BotName setup` to link to a Scion project

## Prerequisites

- Scion hub running with FanOutBroker support (`server.message_broker.types`)
- An **Azure subscription** with permission to create App Registrations and Bot resources
- An **Azure AD tenant** (the tenant ID is needed for single-tenant auth)
- A **Microsoft Teams instance** with admin access for app sideloading or publishing
- A **public HTTPS endpoint** reachable from Microsoft's servers (for the bot's messaging endpoint), or a tunneling solution for development (ngrok, Cloudflare Tunnel, Azure Dev Tunnels)
- Go 1.26+ (for building from source)
- **Standalone mode only:** PostgreSQL database shared with the hub

## Setup Guide

### Admin UI Setup (Recommended)

If you are running a Scion hub with the admin web UI, this is the fastest way to get the Teams plugin configured:

1. Navigate to your Scion hub admin UI at `/admin/integrations`
2. Find "Microsoft Teams" and click **Install** (or **Configure** if already installed)
3. Enter your **Application (Client) ID** and **Tenant ID** from the Azure App Registration (see Step 1 below for how to create one)
4. Enter your **App Secret** (the client secret value from Azure)
5. Click **Save** to store the configuration
6. Click **"Download App Package"** to get the pre-built Teams app manifest (.zip)
7. Upload the .zip to the Teams Admin Center or sideload it in Teams (see Step 4 below)
8. **IMPORTANT:** Set the Messaging Endpoint in your Azure Bot resource (see Step 2 below) — without this, Teams cannot send messages to your bot

> **Note:** You still need to create the Azure App Registration (Step 1) and Azure Bot Resource (Step 2) in the Azure Portal. The admin UI handles plugin configuration and manifest packaging, but the Azure-side resources must be created manually.

---

### Manual / Advanced Setup

The following steps describe the full manual setup process. If you used the Admin UI above, you can skip Steps 3, 5, and 6 (the admin UI handles manifest creation, plugin installation, and configuration) — but you still need Steps 1, 2, 4, 7, and 8.

### 1. Create an Azure App Registration

The App Registration provides the bot's identity (App ID + client secret) for authenticated two-way communication with Teams.

1. Sign in to the [Azure Portal](https://portal.azure.com/)
2. Navigate to **Azure Active Directory** > **App registrations** > **New registration**
3. Configure the registration:
   - **Name:** e.g., "Scion Teams Bot"
   - **Supported account types:** "Accounts in this organizational directory only" (single-tenant)
   - **Redirect URI:** Leave blank (not needed for bot-only auth)
4. Click **Register**
5. On the app's **Overview** page, note:
   - **Application (client) ID** — this is the `app_id` for plugin configuration
   - **Directory (tenant) ID** — this is the `tenant_id` for plugin configuration
6. Go to **Certificates & secrets** > **Client secrets** > **New client secret**
   - **Description:** e.g., "Scion bot secret"
   - **Expires:** Choose an appropriate expiry (recommended: 24 months)
   - Click **Add** and **copy the secret value immediately** (it will not be shown again) — this is the `app_secret`

### 2. Create an Azure Bot Resource

The Azure Bot resource links the App Registration to the Bot Framework Service and configures where Teams sends webhook events.

1. In the Azure Portal, click **Create a resource** and search for **Azure Bot**
2. Click **Create** and configure:
   - **Bot handle:** e.g., "scion-teams-bot"
   - **Subscription** and **Resource group:** Choose your Azure subscription and resource group
   - **Type of App:** Single Tenant
   - **Creation type:** "Use existing app registration"
   - **App ID:** Paste the Application (client) ID from step 1
   - **App tenant ID:** Paste the Directory (tenant) ID from step 1
3. Click **Review + create**, then **Create**
4. Once deployed, go to the Bot resource and open **Configuration**:
   - Set the **Messaging endpoint** to your public HTTPS URL followed by `/api/messages`, e.g.:
     ```
     https://scion-teams.example.com/api/messages
     ```
   - Click **Apply**
5. Go to **Channels** > **Microsoft Teams** > Click **Apply** to enable the Teams channel

> **Note:** The messaging endpoint must be publicly reachable over HTTPS. See [Messaging Endpoint Setup](#messaging-endpoint-setup) for options.

> **⚠️ WARNING: The messaging endpoint is critical.** If you skip setting the messaging endpoint, your bot will appear in Teams but **will not respond to any messages**. The endpoint URL must be set in the Azure Bot resource Configuration page and must follow the format `https://<your-domain>/api/messages`. To verify the endpoint is reachable, run:
> ```
> curl -i -X POST https://<your-domain>/api/messages
> ```
> You should receive a **401 Unauthorized** response (missing Authorization header), which confirms the endpoint is reachable and the plugin is running.

### 3. Create the Teams App Manifest

The Teams App Manifest defines how the bot appears and behaves within Microsoft Teams.

1. Create the manifest files. An example manifest is provided in this repository at [`manifest/manifest.json.example`](manifest/manifest.json.example) — copy it to `manifest.json` and customize:

   - Replace `<YOUR_APP_ID>` with your Application (client) ID
   - Replace `<YOUR_BOT_DOMAIN>` with your public domain (e.g., `scion-teams.example.com`)
   - Update `developer` fields with your organization's details
   - Optionally customize the `name` and `description` fields

2. Prepare the app icons:
   - **color.png** — 192x192 pixels, full-color app icon
   - **outline.png** — 32x32 pixels, transparent outline icon (white with transparency)

3. Package the app as a ZIP file containing exactly three files:
   ```bash
   cd manifest/
   zip ../scion-teams-app.zip manifest.json color.png outline.png
   ```

### 4. Install the Teams App

Choose one of the following installation methods:

**Option A: Sideload for testing (per-user or per-team)**

1. Open **Microsoft Teams** (desktop or web client)
2. Click **Apps** in the left sidebar
3. Click **Manage your apps** > **Upload an app** > **Upload a custom app**
4. Select the `scion-teams-app.zip` file
5. Review the app details and click **Add**

> **Note:** Sideloading must be enabled in your tenant's Teams admin policies. If you don't see the upload option, ask your Teams admin to enable custom app sideloading.

**Option B: Publish via Teams Admin Center (org-wide)**

1. Sign in to the [Teams Admin Center](https://admin.teams.microsoft.com/)
2. Navigate to **Teams apps** > **Manage apps** > **Upload new app**
3. Upload the `scion-teams-app.zip` file
4. Once uploaded, find the app in the list and set its **Status** to **Allowed**
5. Optionally create a setup policy to auto-install the app for specific users or groups

### 5. Build and Install

The plugin binary must be built separately from the hub. The hub discovers it by name (`scion-plugin-teams`) on `$PATH` or via an explicit `path` in `settings.yaml`.

```bash
cd extras/scion-teams
go build -o scion-plugin-teams ./cmd/scion-plugin-teams
sudo install scion-plugin-teams /usr/local/bin/
```

### 6. Configure settings.yaml

Add the Teams plugin to the hub's `settings.yaml` (note that `plugins` MUST be nested under the `server` block):

```yaml
server:
  message_broker:
    enabled: true
    types:
      - teams

  plugins:
    broker:
      teams:
        config:
          app_id: "your-azure-app-id"
          tenant_id: "your-azure-tenant-id"

          # Webhook server listen address (plain HTTP; TLS is handled by the reverse proxy).
          listen_address: ":3978"

          # SQLite database for channel links, user mappings, and state.
          # Default: teams.db (relative to hub working directory).
          db_path: /var/lib/scion/teams.db

          # Optional tuning.
          # db_type: sqlite          # "sqlite" (default) or "postgres"
          # db_dsn: ""               # PostgreSQL DSN (required when db_type=postgres)
          # mention_routing: true    # enable @-mention routing for messages
          # send_queue_size: 100     # max queued outbound messages per conversation
          # send_min_delay: 200ms    # minimum delay between sends (rate-limit protection)
```

Set the client secret via Scion's secret management (never put secrets directly in `settings.yaml`):

```bash
scion secret set TEAMS_APP_SECRET "your-azure-client-secret"
```

### 7. Start the Hub

```bash
sudo systemctl restart scion-hub

# Or manually
./scion server
```

The hub will discover and launch `scion-plugin-teams` as a managed subprocess. Look for `Teams broker configured` in the logs to confirm startup.

### 8. Link a Teams Channel

1. In a Teams channel where the bot is installed, @-mention the bot with **setup**:
   ```
   @ScionBot setup
   ```
2. Select a project from the interactive card that appears
3. Optionally set a default agent for the channel

## Messaging Endpoint Setup

The Teams Bot Framework Service sends HTTP POSTs to your bot's registered messaging endpoint. This endpoint must be publicly reachable over HTTPS with a valid TLS certificate.

### Production: Reverse Proxy

The plugin's webhook server listens on a local port (default `:3978`) over plain HTTP. Use a reverse proxy to handle TLS termination.

**Caddy (recommended for simplicity):**
```
scion-teams.example.com {
    reverse_proxy localhost:3978
}
```

**Nginx:**
```nginx
server {
    listen 443 ssl;
    server_name scion-teams.example.com;

    ssl_certificate     /etc/ssl/certs/scion-teams.crt;
    ssl_certificate_key /etc/ssl/private/scion-teams.key;

    location / {
        proxy_pass http://localhost:3978;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Register the public URL as the messaging endpoint in your Azure Bot resource's Configuration page:
```
https://scion-teams.example.com/api/messages
```

### Development: Tunneling

For local development, use a tunneling solution to expose the webhook server:

**ngrok:**
```bash
ngrok http 3978
# Copy the HTTPS forwarding URL (e.g., https://abc123.ngrok-free.app)
# Update your Azure Bot's messaging endpoint to: https://abc123.ngrok-free.app/api/messages
```

**Cloudflare Tunnel:**
```bash
cloudflared tunnel --url http://localhost:3978
```

**Azure Dev Tunnels:**
```bash
devtunnel host -p 3978
```

> **Note:** Free tunnel URLs change on restart. Update the Azure Bot messaging endpoint each time, or use a paid/persistent tunnel for a stable URL.

### Local-Only: Bot Framework Emulator

For smoke-testing without an Azure subscription or public endpoint, use the [Bot Framework Emulator](https://github.com/microsoft/BotFramework-Emulator):

1. Download and install the emulator
2. Connect to `http://localhost:3978/api/messages`
3. Enter your App ID and App Secret (or leave blank for open mode)

The emulator simulates Teams activities locally but does not render Adaptive Cards identically to the Teams client. Use it for testing the Activity handling pipeline, not for UI validation.

## User Guide

### Bot Commands

Commands are sent by @-mentioning the bot followed by the command name:

| Command | Description |
|---------|-------------|
| `@ScionBot setup` | Link this channel/conversation to a Scion project |
| `@ScionBot unlink` | Unlink this channel from its project |
| `@ScionBot agents` | List agents in the linked project with real-time state |
| `@ScionBot status [agent]` | Show project or agent status |
| `@ScionBot register` | Link your Teams account to your Scion hub identity |
| `@ScionBot unregister` | Remove your Teams account link |
| `@ScionBot help` | Show available commands |

Commands are also available via the Teams command menu (type `/` or the bot name to see suggestions).

### Registration Flow

1. @-mention the bot with **register** in any channel or chat
2. The bot replies with a 6-character code and a link to your hub's profile page
3. Open the link and enter the code to confirm the binding
4. The bot confirms the link in chat

Registration codes expire after 15 minutes. Run the register command again for a fresh code.

### Sending Messages to Agents

Messages are routed based on @-mentions and conversation context:

| Pattern | Routing |
|---------|---------|
| `@ScionBot hello, can you help?` | Routes to the default agent (if set) |
| `@ScionBot agent-name do something` | Routes to the named agent |
| *(reply to a bot message)* | Continues the conversation with the same agent |

The bot strips its own @-mention from the message text before forwarding to the agent.

### Receiving Messages from Agents

- **Agent responses** appear as Adaptive Cards with the agent's name in a bold header and the project slug as context
- **Ask-user prompts** include interactive Approve/Reject buttons directly in the card
- **Status updates** appear as compact notification cards
- **Plain text fallback:** Short or simple messages may be sent as plain text with an `[agent-name]` prefix instead of a full card

### Agent Identity

Unlike Discord (where each agent gets a distinct username and avatar via webhooks), Teams bots have a single identity defined in the app manifest. Agent attribution is provided at the card level — every outbound Adaptive Card includes a header showing the agent name (bold, accent-colored) and the project slug. Users quickly learn to identify which agent sent a message by reading the card header.

## Configuration Reference

### Plugin Config Keys

These keys go in `plugins.broker.teams.config` in `settings.yaml`:

| Key | Required | Default | Description |
|-----|----------|---------|-------------|
| `app_id` | **Yes** | -- | Azure App Registration client ID |
| `tenant_id` | **Yes** | -- | Azure AD tenant ID (GUID) |
| `listen_address` | No | `:3978` | Webhook server bind address (plain HTTP) |
| `db_path` | No | `teams.db` | Path to SQLite database for persistent state |
| `db_type` | No | `sqlite` | Database backend: `sqlite` or `postgres` |
| `db_dsn` | No | -- | PostgreSQL DSN (required when `db_type=postgres`) |
| `mention_routing` | No | `true` | Enable @-mention routing for messages |
| `send_queue_size` | No | `100` | Max queued outbound messages per conversation |
| `send_min_delay` | No | `200ms` | Minimum delay between sends (rate-limit protection) |

### Secrets

Secrets are managed via `scion secret set` and are never stored in `settings.yaml`:

| Secret Key | Description |
|------------|-------------|
| `TEAMS_APP_SECRET` | Azure App Registration client secret |

### Hub Connection Keys (Phase 2 / Standalone)

These are set automatically in plugin mode (the hub injects them). In standalone mode, set them via environment variables:

| Key / Env Var | Description |
|---------------|-------------|
| `hub_url` / `TEAMS_HUB_URL` | Scion Hub API URL |
| `hmac_key` / `TEAMS_HMAC_KEY` | Base64-encoded HMAC key for hub authentication |
| `broker_id` / `TEAMS_BROKER_ID` | Broker identity string for HMAC signing |

### Example settings.yaml (Complete)

```yaml
server:
  message_broker:
    enabled: true
    types:
      - broker-log
      - teams

  plugins:
    broker:
      broker-log:
        self_managed: true
        address: "localhost:9091"
      teams:
        config:
          app_id: "12345678-abcd-efgh-ijkl-123456789012"
          tenant_id: "abcdef12-3456-7890-abcd-ef1234567890"
          listen_address: ":3978"
          db_path: /var/lib/scion/teams.db
```

## Architecture

```
Microsoft Teams (Bot Framework Service)
     |
     v
 +---------------------+   HTTP POST /api/messages  +------------------------+
 |  Teams Channels      | <-----------------------  |  scion-plugin-         |
 |  & Conversations     |  -----------------------> |  teams                 |
 +---------------------+   Bot Connector REST API   |                        |
                                                    |  +- WebhookServer      |
                                                    |  +- JWTValidator       |
                                                    |  +- ActivityHandler    |
                                                    |  +- CommandHandler     |
                                                    |  +- CallbackHandler   |
                                                    |  +- AdaptiveCards     |
                                                    |  +- SendQueue         |
                                                    |       |               |
                                                    |  SQLite (state)       |
                                                    +--------+--------------+
                                                             | go-plugin RPC
                                                             v
                                                    +------------------------+
                                                    |      Scion Hub         |
                                                    |    (FanOutBroker)      |
                                                    |                        |
                                                    |  +- broker-log         |
                                                    |  +- teams        <-----|
                                                    |  +- discord            |
                                                    +------------------------+
```

**Key architectural differences from the Discord plugin:**

- **Webhook model vs Gateway:** The Teams plugin receives inbound messages via HTTP POST webhooks (stateless) rather than a persistent WebSocket connection. This simplifies HA (multiple instances can receive behind a load balancer) but requires a publicly reachable HTTPS endpoint.
- **JWT validation:** Every inbound POST is authenticated by validating a JWT signed by Microsoft. The plugin fetches OpenID metadata and signing keys from `https://login.botframework.com/v1/.well-known/openidconfiguration` and caches them.
- **OAuth2 outbound auth:** Outbound API calls use an OAuth2 bearer token obtained via client_credentials grant from `https://login.microsoftonline.com/{tenant_id}/oauth2/v2.0/token`, rather than a static bot token.
- **Adaptive Cards:** Rich messages use Adaptive Cards (declarative JSON) instead of Discord embeds. Interactive components (buttons, forms) use `Action.Submit` which triggers `invoke` activities.
- **Single bot identity:** Agent attribution is in the card header, not via per-message identity switching.
- **ServiceURL tracking:** The `serviceUrl` in each inbound Activity varies by region and must be stored and used for outbound replies to that conversation.

For full design details, see the [Teams Integration Design Document](/.design/teams-integration-design.md).

## Standalone Mode (HA Deployment)

Standalone mode runs the Teams bot as an independent service, communicating with the hub via gRPC.

### How It Works

- The binary detects standalone mode via `--standalone` flag or `TEAMS_STANDALONE=true` env var
- Unlike Discord (which requires an advisory lock to serialize Gateway connections), the Teams webhook model allows multiple instances to receive inbound messages simultaneously behind a load balancer
- An advisory lock MAY be used to serialize outbound `Publish()` calls to prevent duplicate sends (later-phase concern)
- All instances serve gRPC and respond to health checks

### Quick Start (Standalone)

```bash
# Build
cd extras/scion-teams
go build -o scion-plugin-teams ./cmd/scion-plugin-teams

# Run
export DATABASE_URL="postgres://user:pass@localhost:5432/scion?sslmode=disable"
export TEAMS_APP_ID="your-azure-app-id"
export TEAMS_APP_SECRET="your-azure-client-secret"
export TEAMS_TENANT_ID="your-azure-tenant-id"
export TEAMS_HUB_URL="https://your-hub.example.com"
export TEAMS_BROKER_ID="your-broker-uuid"
export TEAMS_HMAC_KEY="your-base64-hmac-key"
./scion-plugin-teams --standalone
```

### Hub Configuration for gRPC Mode

In the hub's `settings.yaml`, configure the Teams integration with `mode: grpc`:

```yaml
server:
  message_broker:
    enabled: true
    types:
      - teams

  plugins:
    broker:
      teams:
        mode: grpc
        address: "teams-bot:50051"  # hostname:port of the standalone service
```

Without `mode: grpc`, the hub launches Teams as a go-plugin subprocess (plugin mode).

## Troubleshooting

### JWT Validation Failures (401 Unauthorized)

If the plugin rejects all inbound messages with 401 errors:

1. **Verify the App ID:** The `app_id` in `settings.yaml` must exactly match the Application (client) ID in your Azure App Registration. The JWT `aud` claim is validated against this value.
2. **Verify the Tenant ID:** Ensure `tenant_id` matches the Directory (tenant) ID from the Azure Portal.
3. **Check clock skew:** JWT validation checks `exp` and `nbf` claims. Ensure your server's clock is synchronized (use NTP).
4. **JWKS cache:** On first startup, the plugin fetches signing keys from Microsoft. If the initial fetch fails (network issue), restart the plugin.

### Token Refresh Problems (Outbound Messages Fail)

If the bot receives messages but cannot send responses:

1. **Verify the client secret:** Run `scion secret get TEAMS_APP_SECRET` to confirm it is set and not expired.
2. **Regenerate the secret:** In the Azure Portal > App Registration > Certificates & secrets, create a new client secret and update it with `scion secret set TEAMS_APP_SECRET "new-secret"`.
3. **Check token endpoint:** The plugin acquires tokens from `https://login.microsoftonline.com/{tenant_id}/oauth2/v2.0/token`. Ensure outbound HTTPS to `login.microsoftonline.com` is not blocked by a firewall.

### Webhook Not Reachable (No Inbound Messages)

If the bot appears online in Teams but never receives messages:

1. **Verify the messaging endpoint:** In the Azure Portal > Bot resource > Configuration, confirm the messaging endpoint is set to your public URL with the `/api/messages` path (e.g., `https://scion-teams.example.com/api/messages`).
2. **Test HTTPS connectivity:** `curl -I https://scion-teams.example.com/api/messages` should return a response (even a 401 is fine — it means the server is reachable).
3. **Check the reverse proxy:** Ensure your reverse proxy is forwarding to the plugin's listen address (default `localhost:3978`).
4. **Tunnel URLs:** If using ngrok or similar, ensure the tunnel is running and the URL in the Azure Bot Configuration matches the current tunnel URL.

### Bot Not Appearing in Teams

If the bot does not appear in the Teams client after installing the app:

1. **Manifest validation:** Ensure the `manifest.json` is valid — the `botId` must match your App ID, and the `$schema` URL must be correct.
2. **App icons:** Both `color.png` (192x192) and `outline.png` (32x32) must be present in the ZIP package.
3. **Sideloading policy:** Your Teams tenant must allow custom app sideloading. Check Teams Admin Center > Teams apps > Permission policies.
4. **Cache:** Teams may cache app state. Try removing and re-adding the app, or wait a few minutes.

### Bot Framework Emulator

For local testing without Azure infrastructure:

1. Download the [Bot Framework Emulator](https://github.com/microsoft/BotFramework-Emulator/releases)
2. Start the plugin locally (it will listen on `http://localhost:3978`)
3. In the Emulator, connect to `http://localhost:3978/api/messages`
4. Enter your App ID and App Secret for authenticated testing, or leave blank for open mode

> **Note:** The Emulator does not perfectly replicate Teams behavior. Adaptive Cards render differently, and Teams-specific features (channel context, team membership) are not available. Use the Emulator for smoke-testing the Activity handling pipeline only.

### Common Log Messages

| Log Message | Meaning |
|-------------|---------|
| `Teams broker configured` | Plugin started successfully in plugin mode |
| `Webhook server listening on :3978` | HTTP server is running and ready for Activities |
| `JWT validation failed` | Inbound request had an invalid or expired token |
| `Token refresh failed` | Could not acquire an OAuth2 token for outbound calls |
| `ServiceUrl updated for conversation` | Normal — the plugin tracks per-conversation service URLs |
| `Channel link created` | A Teams channel was successfully linked to a Scion project |
