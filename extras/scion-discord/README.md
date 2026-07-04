# scion-plugin-discord

Discord message broker plugin for the Scion hub. Provides bidirectional messaging between Discord channels and Scion agents.

**Two operating modes:**
- **Plugin mode** (default) — runs as a [go-plugin](https://github.com/hashicorp/go-plugin) subprocess managed by the hub
- **Standalone mode** — runs as an independent gRPC service with HA support via Postgres advisory locks

**Outbound:** Hub publishes `StructuredMessage`s → plugin formats and sends them to linked Discord channels via the Bot API, using per-agent webhooks for distinct agent identities (custom name + avatar).
**Inbound:** Discord messages (via Gateway) → plugin converts to `StructuredMessage`s → delivered to agents via the hub's inbound endpoint.

## Prerequisites

- Scion hub running with FanOutBroker support (`server.message_broker.types`)
- A Discord account with permission to create applications at [discord.com/developers](https://discord.com/developers/applications)
- Go 1.26+ (for building from source)
- **Standalone mode only:** PostgreSQL database shared with the hub

## Setup Guide

### 1. Create the Discord Bot

1. Go to [discord.com/developers/applications](https://discord.com/developers/applications) and click **New Application**
2. Name it (e.g., "Scion"), then go to the **Bot** tab
3. Click **Reset Token** and copy the bot token (you'll need it for `settings.yaml`)
4. Copy the **Application ID** and **Public Key** from the **General Information** tab

#### Enable Privileged Gateway Intents

Under the **Bot** tab, scroll to **Privileged Gateway Intents** and enable:

- **Message Content Intent** — required for reading @-mention message text. There is no slash-command-only fallback mode.
- **Server Members Intent** — required for resolving user information

> **Note:** Scion bots are self-hosted and typically serve <100 guilds, so privileged intents are straightforward to enable without Discord's verification process.

### 2. Invite the Bot to Your Server

Go to the **OAuth2** tab, then **URL Generator**:

1. Select scopes: `bot` and `applications.commands`
2. Select the bot permissions listed below (or use the permissions integer `329101954112`)
3. Copy the generated URL and open it to invite the bot

#### Required Bot Permissions

| Permission | Purpose |
|-----------|---------|
| Send Messages | Post agent responses in channels |
| Send Messages in Threads | Reply within conversation threads |
| Create Public Threads | Create thread-per-conversation |
| Embed Links | Rich embed formatting for agent responses |
| Read Message History | Access thread context for conversations |
| View Channels | Discover and read linked channels |
| Use Application Commands | Register and respond to `/scion` slash commands |
| Manage Threads | Archive/unarchive conversation threads |
| Manage Webhooks | **Required for per-agent identity** — each agent appears with its own name and avatar via Discord webhooks |
| Add Reactions | Acknowledge messages (optional) |

> **Manage Webhooks** must be granted either via Server Settings → Roles → [Bot role] → enable Manage Webhooks, or included in the OAuth2 invite URL permissions. Without it, all messages will be sent as the bot user instead of with per-agent personas.

### 3. Build and Install

The plugin binary must be built separately from the hub. The hub discovers it by name (`scion-plugin-discord`) on `$PATH` or via an explicit `path` in `settings.yaml`.

```bash
cd extras/scion-discord
go build -o scion-plugin-discord ./cmd/scion-plugin-discord
sudo install scion-plugin-discord /usr/local/bin/
```

### 4. Configure settings.yaml

Add the Discord plugin to the hub's `settings.yaml` (note that `plugins` MUST be nested under the `server` block):

```yaml
server:
  message_broker:
    enabled: true
    types:
      - discord

  plugins:
    broker:
      discord:
        config:
          bot_token: "your-bot-token"
          application_id: "your-application-id"
          public_key: "your-public-key"

          # Guild-scoped command registration (instant updates, good for dev).
          # Leave empty for global commands (can take up to 1 hour to propagate).
          guild_id: ""

          # SQLite database for channel links, user mappings, and state.
          # Default: discord.db (relative to hub working directory).
          db_path: /var/lib/scion/discord.db

          # Optional tuning.
          # send_queue_size: 100     # max queued messages per channel
          # send_min_delay: 50ms     # minimum delay between sends (rate limiting)
          # agent_cache_ttl: 5m      # how long to cache agent lists from hub
```

### 5. Start the Hub

```bash
sudo systemctl restart scion-hub

# Or manually
./scion server
```

The hub will discover and launch `scion-plugin-discord` as a managed subprocess. Look for `Discord broker configured` in the logs to confirm startup.

### 6. Link a Discord Channel

1. **Invite the bot** to your Discord server using the OAuth2 URL
2. **Run `/scion setup`** in any channel → select a project from the list
3. **Register your identity:** run `/scion register` → click the link → authenticate on your hub's profile page (`/profile/discord`)

## Agent-Led Installation and Setup

If you are using an AI coding assistant or deployment agent (like Antigravity) to set up and configure this plugin on your Scion instance, you can guide the agent with the following instructions:

### 1. Interactive Information Gathering
An agent should proactively ask the user for:
- **Discord Bot Token:** (e.g. `MTUxNDcwOD...`)
- **Discord Application ID:** (e.g. `1514708...`)
- **Discord Public Key (Optional):**

Upon receiving the **Application ID**, the agent can automatically construct and output the Discord Server Invitation URL using the required permissions integer `329101954112` (which covers all mandatory permissions, including `Manage Webhooks`):
```text
https://discord.com/api/oauth2/authorize?client_id=<APPLICATION_ID>&permissions=329101954112&scope=bot%20applications.commands
```

### 2. Remote Configuration via gcloud ssh
The agent can automatically configure your remote GCE server:
1. **Identify GCE Instance:** Determine the running instance name, zone, and project ID.
2. **Build and Install Plugin:** Compile the binary locally or directly on the remote VM, and install to `/usr/local/bin/scion-plugin-discord`.
3. **Inject Settings:** Append or modify the YAML configuration inside the remote settings file (located at `/home/scion/.scion/settings.yaml`).
4. **Service Restart & Verification:** Safely restart the service and stream the logs.

### 3. Agent Prompts
You can copy and paste the following prompt to have an agent execute this installation:

> **Agent Prompt:**
> Please configure the Discord plugin on our active Scion Hub instance.
> 
> 1. Ask me for my Discord Bot Token and Application ID.
> 2. Once I provide the Application ID, generate and output my Discord bot server invite link with permissions set to `329101954112`.
> 3. SSH into the active GCE VM and configure the `/home/scion/.scion/settings.yaml` file:
>    - Ensure `- discord` is enabled under `server.message_broker.types`.
>    - Add the `server.plugins.broker.discord` block with the provided token and app-id (ensure `plugins` is nested under `server:` and not at the root level).
>    - Set `db_path` to `/home/scion/.scion/discord.db`.
> 4. Run `sudo systemctl restart scion-hub` and check the logs via `journalctl` to verify that the message `Discord gateway connected` or `Discord bot ready` is present.

### 4. Verification Checklist (for the Agent)
The agent should verify the following to confirm a successful installation:
- [ ] `which scion-plugin-discord` returns `/usr/local/bin/scion-plugin-discord`.
- [ ] The SQLite database directory for `db_path` exists and is writable by the `scion` user.
- [ ] `/home/scion/.scion/settings.yaml` is valid YAML and includes the `discord` broker type.
- [ ] The `plugins:` block is properly nested under the `server:` block in `/home/scion/.scion/settings.yaml`.
- [ ] `systemctl is-active scion-hub` returns `active`.


## User Guide

### Slash Commands

All commands are subcommands of `/scion`:

| Command | Description |
|---------|-------------|
| `/scion setup` | Link this channel to a Scion project |
| `/scion unlink` | Unlink this channel from its project |
| `/scion agents` | List agents in the linked project with real-time state |
| `/scion default [agent]` | Set, change, or show the default agent |
| `/scion status <agent>` | Show detailed status for an agent |
| `/scion register` | Link your Discord account to your Scion hub identity |
| `/scion unregister` | Remove your Discord account link |
| `/scion info` | Show your registration status |
| `/scion settings` | Configure channel notification settings |
| `/scion help` | Show available commands |

Commands that modify configuration (`setup`, `unlink`) require Discord's **Manage Channels** permission.

### Registration Flow

1. Run `/scion register` in any channel (response is ephemeral — only you can see it)
2. Click the profile link button in the response
3. Authenticate on the hub and confirm the 6-character code
4. The plugin detects confirmation and stores the link

Registration codes expire after 15 minutes. Run `/scion register` again for a fresh code.

### Sending Messages to Agents

Messages are routed based on @-mentions. If a default agent is set and the message is plain text (no `@mention`), it is automatically routed to the default agent.

| Pattern | Routing |
|---------|---------|
| `hello, can you help?` | Routes to the default agent (if set) |
| `@BotName message` | Routes to the default agent |
| `@agentslug message` | Routes to the named agent |
| `@all message` | Broadcasts to ALL agents in the linked project |
| *(reply to a bot message)* | Continues the conversation with the same agent |

The bot strips @-mentions from the message text before forwarding to the agent. Use `/scion default` to set, change, or clear the default agent.

### Receiving Messages from Agents

- **Agent replies** appear in the linked channel with the agent's own name and avatar (via webhooks)
- **Rich formatting** uses Discord embeds for structured responses
- **Agent avatars** are generated via [RoboHash](https://robohash.org/) based on the agent slug
- Messages exceeding Discord's 2000-character limit are split or truncated
- Embed descriptions exceeding 4096 characters are truncated with `[truncated]`

### Agent Identity (Webhooks)

Each agent appears in Discord with a distinct username and avatar, powered by Discord webhooks. The plugin lazily creates one webhook per channel ("Scion Agent Relay") and sends messages through it with per-agent `username` and `avatar_url` parameters. This requires the **Manage Webhooks** permission.

If the permission is not granted, messages fall back to the bot's own identity.

## Configuration Reference

### Plugin Config Keys

These keys go in `plugins.broker.discord.config` in `settings.yaml`:

| Key | Required | Default | Description |
|-----|----------|---------|-------------|
| `bot_token` | **Yes** | — | Discord bot token |
| `application_id` | **Yes** | — | Discord application ID |
| `public_key` | No | — | Discord application public key |
| `guild_id` | No | — | Guild ID for guild-scoped slash commands (empty = global) |
| `db_path` | No | `discord.db` | Path to SQLite database for persistent state |
| `mention_routing` | No | `true` | Enable @-mention routing for messages |
| `send_queue_size` | No | `100` | Max queued outbound messages per channel |
| `send_min_delay` | No | `50ms` | Minimum delay between sends (rate-limit protection) |
| `agent_cache_ttl` | No | `5m` | TTL for cached agent lists from the hub |

### Example settings.yaml (Complete)

```yaml
server:
  message_broker:
    enabled: true
    types:
      - broker-log
      - discord

  plugins:
    broker:
      broker-log:
        self_managed: true
        address: "localhost:9091"
      discord:
        config:
          bot_token: "MTIzNDU2Nzg5.example.token"
          application_id: "123456789012345678"
          public_key: "abcdef1234567890abcdef1234567890abcdef1234567890"
          guild_id: "987654321098765432"
          db_path: /var/lib/scion/discord.db
```

## Architecture

```
Discord Gateway API
     │
     ▼
 ┌──────────────────┐   Gateway events     ┌──────────────────────┐
 │  Discord Channels │ ◄───────────────── │  scion-plugin-       │
 │  & DMs            │ ──────────────────►│  discord              │
 └──────────────────┘   Bot API / Webhooks│                      │
                                          │  ┌─ CommandHandler   │
                                          │  ├─ CallbackHandler  │
                                          │  ├─ RegistrationHndlr│
                                          │  ├─ WebhookManager   │
                                          │  └─ SendQueue        │
                                          │        │             │
                                          │  SQLite (state)      │
                                          └──────────┬───────────┘
                                                     │ go-plugin RPC
                                                     ▼
                                          ┌──────────────────────┐
                                          │     Scion Hub        │
                                          │   (FanOutBroker)     │
                                          │                      │
                                          │  ┌─ broker-log       │
                                          │  ├─ discord    ◄─────│
                                          │  └─ chat-app         │
                                          └──────────────────────┘
```

- **FanOutBroker spoke:** The plugin runs as one of potentially several broker spokes. The hub publishes messages to all configured spokes concurrently.
- **Gateway mode:** The plugin connects to Discord via WebSocket Gateway (not HTTP interactions), receiving real-time message events.
- **Registration** uses a hub-issued 6-character code. The user generates a code via `/scion register`, then confirms it on the hub's `/profile/discord` page.
- **SQLite state** persists channel links, user mappings, conversation contexts, notification preferences, and pending ask-user callbacks across restarts.
- **Send queue** uses per-channel worker goroutines with configurable rate limiting to avoid Discord 429 errors.
- **Webhook identity** gives each agent a unique name and RoboHash avatar in Discord, managed per-channel with automatic recreation if deleted.

## Standalone Mode (HA Deployment)

Standalone mode runs the Discord bot as an independent service, communicating with the hub via gRPC. This enables high-availability deployments with automatic failover.

### How It Works

- The binary detects standalone mode via `--standalone` flag or `DISCORD_STANDALONE=true` env var
- A Postgres advisory lock held on a dedicated database connection ensures only one instance opens the Discord Gateway at a time (Discord enforces one WebSocket session per bot token)
- The lock holder periodically verifies the connection is alive; on loss, it closes the Gateway and re-enters standby
- All instances serve gRPC and respond to health checks regardless of lock state
- The lock holder runs the Gateway; standby instances retry every ~30s
- On primary failure, Postgres releases the session-level lock and a standby promotes after a takeover delay (~60–90s) to prevent dual-Gateway storms
- Outbound messages (hub → Discord) are delivered via REST API, which works from any instance — only the Gateway connection is serialized

### Quick Start (Standalone)

```bash
# Build
cd extras/scion-discord
go build -o scion-plugin-discord ./cmd/scion-plugin-discord

# Run
export DATABASE_URL="postgres://user:pass@localhost:5432/scion?sslmode=disable"
export DISCORD_BOT_TOKEN="your-bot-token"
export DISCORD_APPLICATION_ID="your-app-id"
export DISCORD_HUB_URL="https://your-hub.example.com"
export DISCORD_BROKER_ID="your-broker-uuid"
export DISCORD_HMAC_KEY="your-base64-hmac-key"
./scion-plugin-discord --standalone
```

### Docker

```bash
# Build from repository root
docker build -f extras/scion-discord/Dockerfile -t scion-discord .

# Run
docker run -e DATABASE_URL=... -e DISCORD_BOT_TOKEN=... scion-discord
```

### Required Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | **Yes** | — | Postgres connection string (shared with hub) |
| `DISCORD_BOT_TOKEN` | **Yes** | — | Discord bot token from Developer Portal |
| `DISCORD_APPLICATION_ID` | Recommended | — | For slash command registration |
| `DISCORD_PUBLIC_KEY` | No | — | For interaction verification |
| `DISCORD_HUB_URL` | **Yes** | — | Hub base URL for registration API / inbound delivery |
| `DISCORD_BROKER_ID` | **Yes** | — | Registered broker ID (UUID) |
| `DISCORD_HMAC_KEY` | **Yes** | — | Base64-encoded HMAC secret for hub authentication |
| `DISCORD_GUILD_ID` | No | — | Guild ID for guild-scoped commands |
| `GRPC_PORT` | No | `50051` | gRPC listen port |
| `CONFIG_FILE` | No | — | Path to local YAML config file |
| `UPDATE_HOOK` | No | — | Command to run on update signal (default: exit for platform restart) |

### Hub Configuration for gRPC Mode

In the hub's `settings.yaml`, configure the Discord integration with `mode: grpc`:

```yaml
server:
  message_broker:
    enabled: true
    types:
      - discord

  plugins:
    broker:
      discord:
        mode: grpc
        address: "discord-bot:50051"  # hostname:port of the standalone service
```

Without `mode: grpc`, the hub launches Discord as a go-plugin subprocess (plugin mode).

### Broker Auth Credentials

The `/scion register` command makes direct HTTP calls to the hub API, authenticated via HMAC-SHA256. The standalone bot needs `DISCORD_HUB_URL`, `DISCORD_BROKER_ID`, and `DISCORD_HMAC_KEY` to authenticate.

To register a broker:
```bash
# Register via hub API
curl -X POST https://your-hub/api/v1/brokers -d '{"name":"plugin-discord"}'

# Complete join to get HMAC secret
curl -X POST https://your-hub/api/v1/brokers/join -d '{"broker_id":"<id-from-step-1>"}'
```

### Privileged Gateway Intents

The bot requires privileged Gateway intents enabled in the [Discord Developer Portal](https://discord.com/developers/applications) → Bot → Privileged Gateway Intents:

- **MESSAGE CONTENT INTENT** — required for reading message text
- **SERVER MEMBERS INTENT** — required for resolving user info
- **PRESENCE INTENT** — required by the bot's intent flags

Without Message Content Intent, the bot connects but messages arrive with empty content. Without any required intent, the Gateway rejects the connection entirely (error 4014).

### Bot Invite Permissions

Minimum required permissions (integer: 84992): Send Messages, Read Message History, View Channels, Embed Links.

For per-agent webhook identity, add **Manage Webhooks** permission. Without it, all messages appear as the bot user instead of distinct per-agent personas.

### One Bot Token = One Hub

Discord enforces exactly ONE Gateway WebSocket session per bot token. If the same token is used by two independent deployments, they fight over the session (opcode 9 INVALID SESSION reconnect storm).

Within a single deployment, the advisory lock handles this — only one replica connects at a time. Across different hub deployments, use different bot tokens (create separate Discord Applications).

### message_broker.types Requirement

The hub's outbound message dispatch only routes to integrations listed in `message_broker.types`. Loading the Discord plugin is not enough — `discord` must also appear in this list:

```yaml
message_broker:
  types: [discord]
```

### HA Failover Behavior

1. Instance A acquires the advisory lock on a dedicated DB connection → opens the Discord Gateway
2. Instance B fails to acquire → enters standby, retries every 30s
3. Instance A crashes → Postgres releases the session-level lock (dedicated connection dies)
4. Instance B observes the lock acquirable on two consecutive ticks (takeover delay) → opens Gateway within ~60–90s
5. Messages queued during the gap are delivered on reconnect (deferred message reconciliation)
6. If Instance A's lock connection dies without a crash, Instance A detects it on the next verify tick, closes the Gateway, and re-enters standby

### Config Layering

In standalone mode, configuration is resolved with the following priority (highest wins):

1. **DB config** (`integration_configs` table) — managed by admin UI
2. **Environment variables** (`DISCORD_*` prefix)
3. **Local YAML file** (`CONFIG_FILE` env var)

Config changes made via the admin UI are delivered to the running service via Postgres LISTEN/NOTIFY without restart.

## Troubleshooting

### Disallowed Gateway Intents (Error 4014)

If the hub logs contain an error similar to:
```text
websocket: close 4014: Disallowed intent(s).
```
This means the bot has not been granted the required privileged intents in the Discord Developer Portal.

**Solution:**
1. Navigate to [discord.com/developers/applications](https://discord.com/developers/applications) and select your application.
2. Go to the **Bot** tab on the left-side menu.
3. Scroll down to the **Privileged Gateway Intents** section.
4. Enable both **Server Members Intent** and **Message Content Intent**.
5. Click **Save Changes** and restart your Scion hub server (`sudo systemctl restart scion-hub`).

