# scion-plugin-discord

Discord message broker plugin for the Scion hub. Runs as a [go-plugin](https://github.com/hashicorp/go-plugin) broker spoke in the hub's FanOutBroker, providing bidirectional messaging between Discord channels and Scion agents.

**Outbound:** Hub publishes `StructuredMessage`s → plugin formats and sends them to linked Discord channels via the Bot API, using per-agent webhooks for distinct agent identities (custom name + avatar).
**Inbound:** Discord messages (via Gateway) → plugin converts to `StructuredMessage`s → delivered to agents via the hub's inbound endpoint.

## Prerequisites

- Scion hub running with FanOutBroker support (`server.message_broker.types`)
- A Discord account with permission to create applications at [discord.com/developers](https://discord.com/developers/applications)
- Go 1.25+ (for building from source)

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
2. Select the bot permissions listed below (or use the permissions integer `277562386496`)
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

Add the Discord plugin to the hub's `settings.yaml`:

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
