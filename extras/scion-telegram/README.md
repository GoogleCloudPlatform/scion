# scion-plugin-telegram

Telegram message broker plugin for the Scion hub. Runs as a [go-plugin](https://github.com/hashicorp/go-plugin) broker spoke in the hub's FanOutBroker, providing bidirectional messaging between Telegram group chats and Scion agents.

**Outbound:** Hub publishes `StructuredMessage`s → plugin formats and sends them to linked Telegram groups via the Bot API.
**Inbound:** Telegram messages (via webhook or long-polling) → plugin converts to `StructuredMessage`s → delivered to agents via the hub's inbound endpoint.

## Prerequisites

- Scion hub running with FanOutBroker support (`server.message_broker.types`)
- A Telegram account to create a bot via [@BotFather](https://t.me/BotFather)
- Public HTTPS URL for the hub (required for webhook mode)
- Go 1.25+ (for building from source)

## Setup Guide

### 1. Create the Bot

1. Open [@BotFather](https://t.me/BotFather) in Telegram
2. Send `/newbot`, choose a name and username
3. Copy the bot token (you'll need it for `settings.yaml`)

> **CRITICAL: Disable privacy mode.**
> By default, Telegram bots in group chats only receive `/commands` — not regular messages. You **must** disable privacy mode for @-mention routing to work.
>
> In BotFather: `/mybots` → select your bot → **Bot Settings** → **Group Privacy** → **Turn OFF**
>
> After disabling, **remove and re-add the bot** to any existing groups for the change to take effect.

### 2. Build and Install

The plugin binary must be built separately from the hub. The hub discovers it by name (`scion-plugin-telegram`) on `$PATH` or via an explicit `path` in `settings.yaml`.

```bash
cd extras/scion-telegram
go build -o scion-plugin-telegram ./cmd/scion-plugin-telegram
sudo install scion-plugin-telegram /usr/local/bin/
```

> **Note:** If you are also rebuilding the hub binary, run `make web` before `go build ./cmd/scion` — the web UI is embedded in the hub binary.

### 3. Configure settings.yaml

Add the Telegram plugin to the hub's `settings.yaml`:

```yaml
server:
  message_broker:
    enabled: true
    # List all broker spokes for fan-out. Include any existing spokes
    # (e.g., broker-log) alongside telegram.
    types:
      - telegram

plugins:
  broker:
    telegram:
      # Managed plugin: hub discovers and launches the binary.
      # Set 'path' only if scion-plugin-telegram is not on $PATH.
      # path: /usr/local/bin/scion-plugin-telegram
      config:
        bot_token: "123456:ABC-DEF..."

        # Inbound mode: "poll" (default) or "webhook" (recommended).
        inbound_mode: webhook

        # Webhook settings (required when inbound_mode is "webhook").
        webhook_url: "https://hub.example.com/telegram/webhook"
        webhook_listen: ":9094"
        webhook_secret: "<generate with: openssl rand -hex 16>"

        # SQLite database for group links, user mappings, and state.
        # Default: telegram_v2.db (relative to hub working directory).
        db_path: /var/lib/scion/telegram_v2.db

        # Optional tuning.
        # send_queue_size: 100     # max queued messages per chat
        # send_min_delay: 50ms     # minimum delay between sends (rate limiting)
        # agent_cache_ttl: 5m      # how long to cache agent lists from hub
```

Add the V2 environment variable to your hub's env file (e.g., `hub.env`):

```bash
SCION_TELEGRAM_V2=1
```

### 4. Configure Caddy (Webhook Mode)

If your hub runs behind Caddy (e.g., via `scripts/starter-hub/`), add a route for the webhook **before** the catch-all hub proxy:

```
hub.example.com {
    # Telegram webhook — must come BEFORE the catch-all
    handle /telegram/webhook* {
        reverse_proxy localhost:9094
    }

    # Hub API and Web UI
    handle {
        reverse_proxy localhost:8080
    }

    tls /path/to/fullchain.pem /path/to/privkey.pem
}
```

### 5. Generate and Register Webhook Secret

```bash
# Generate a secret
openssl rand -hex 16
```

Add the generated value to `settings.yaml` as `webhook_secret`.

After the hub starts, the plugin registers the webhook automatically. If you change the secret or the webhook URL, you may need to manually re-register:

```bash
# Delete the existing webhook
curl -s -X POST "https://api.telegram.org/bot<TOKEN>/deleteWebhook"

# Re-register with the correct secret
curl -s -X POST "https://api.telegram.org/bot<TOKEN>/setWebhook" \
  -d "url=https://hub.example.com/telegram/webhook&secret_token=<SECRET>"
```

### 6. Start the Hub

```bash
# If using systemd
sudo systemctl restart scion-hub

# Or manually
SCION_TELEGRAM_V2=1 ./scion server
```

The hub will discover and launch `scion-plugin-telegram` as a managed subprocess. Look for `Telegram v2 broker configured` in the logs to confirm startup.

### 7. Link a Telegram Group

1. **Add the bot** to a Telegram group
2. **Type `/setup`** in the group → select a project → select a default agent
3. **Register your identity:** send `/register` in a DM to the bot → enter the 6-character code at your hub's profile page (`/profile/telegram`)

## User Guide

### Bot Commands (Group Chats)

| Command | Description |
|---------|-------------|
| `/setup` | Link this group to a Scion project |
| `/agents` | List agents in the linked project with real-time state |
| `/default` | Set or clear the default agent for unaddressed messages |
| `/settings` | Configure group settings (see below) |
| `/unlink` | Unlink this group from its project |
| `/help` | Show available commands |

### Bot Commands (Direct Messages)

| Command | Description |
|---------|-------------|
| `/register` | Link your Telegram account to your Scion hub identity |
| `/unregister` | Remove your Telegram account link |
| `/status` | Show linked groups and registration status |
| `/notifications` | Manage per-agent notification subscriptions |
| `/help` | Show available commands |

### Sending Messages to Agents

Messages are routed based on @-mentions. If a default agent is set and the message is plain text (no `@mention` or `/command` prefix), it is automatically routed to the default agent — no @-mention required.

| Pattern | Routing |
|---------|---------|
| `hello, can you help?` | Routes to the default agent (if set) |
| `@botname @agentslug message` | Routes to the named agent |
| `@botname message` | Routes to the group's default agent |
| `@agentslug message` | Routes to the agent if the slug is recognized |
| `@all message` | Broadcasts to ALL agents in the linked project |
| *(reply to a bot message)* | Continues the conversation with the same agent |

The bot strips @-mentions from the message text before forwarding to the agent. Use `/default` to set, change, or clear the default agent. Selecting "No default agent" disables automatic routing — only explicit @-mentions and replies will reach agents.

### Receiving Messages from Agents

- **Agent replies** appear in the linked group, prefixed with 🤖 and the agent slug
- **State change notifications** (completed, error, waiting for input) are sent to your DM if you have notification subscriptions
- **Group notifications** for state changes can be enabled via `/settings`
- **Urgent messages** are prefixed with `[URGENT]`
- **Broadcast messages** are prefixed with `[Broadcast]`
- Messages exceeding Telegram's 4096-character limit are truncated with `[truncated]`

### File Attachments

Agents can send files to users via the Scion CLI:

```bash
scion message user:email 'message' --attach /workspace/file.pdf
```

The file must be accessible from the hub's filesystem (`/workspace` maps to hub project storage).

### Group Settings

Use `/settings` in a linked group to toggle:

| Setting | Description |
|---------|-------------|
| **Observer mode** (`a2a`) | Show agent-to-agent messages in the group. Format: `👀 🤖 agentA → 🤖 agentB 👀` |
| **Commentary** (`commentary`) | Show assistant-reply messages (agent responses to other agents) |
| **Notify in group** (`grp`) | Post agent state change notifications in the group chat (in addition to DMs) |

### Notification Subscriptions

Use `/notifications` in a DM with the bot to subscribe to per-agent state change alerts. Subscriptions are per-user and per-agent — you choose which agents you want to monitor.

## Troubleshooting

### Messages not delivered to agents

**Check 1 (MOST COMMON): Bot privacy mode is enabled**

```bash
curl -s "https://api.telegram.org/bot<TOKEN>/getMe" | grep can_read_all_group_messages
```

If `false`: privacy mode is ON. The bot can only see `/commands`, not regular @-mention messages.

**Fix:** BotFather → `/mybots` → select bot → **Bot Settings** → **Group Privacy** → **Turn OFF**. Then **remove and re-add the bot** to the group.

**Check 2: Group is not linked**

Run `/setup` in the group to link it to a project.

**Check 3: User not registered**

Run `/register` in a DM with the bot to link your Telegram identity. You must be registered for the plugin to identify the message sender.

**Check 4: Webhook secret mismatch**

After install or secret changes, manually re-register the webhook:

```bash
curl -s -X POST "https://api.telegram.org/bot<TOKEN>/deleteWebhook"
curl -s -X POST "https://api.telegram.org/bot<TOKEN>/setWebhook" \
  -d "url=https://hub.example.com/telegram/webhook&secret_token=<SECRET>"
```

**Check 5: No running agents in project**

`/agents` must show at least one running agent. Start an agent in the linked project first.

### Bot works for /commands but not regular messages

Privacy mode is ON — see Check 1 above.

### 409 Conflict errors in hub logs

Two polling sessions are conflicting. This happens when multiple hub instances (or a stale process) are both long-polling the Telegram API.

**Fix:** Switch to webhook mode (recommended) or ensure only one hub instance is running with `inbound_mode: poll`.

### `/profile/telegram` returns 404

The hub binary was built without the web UI. Rebuild with:

```bash
make web && go build ./cmd/scion
```

### Registration code expired or not found

Codes expire after 15 minutes. Run `/register` again in DM to get a fresh code, then enter it at `/profile/telegram` on the hub.

## Configuration Reference

### Plugin Config Keys

These keys go in `plugins.broker.telegram.config` in `settings.yaml`:

| Key | Required | Default | Description |
|-----|----------|---------|-------------|
| `bot_token` | **Yes** | — | Telegram Bot API token from BotFather |
| `inbound_mode` | No | `poll` | `poll` (long-polling) or `webhook` |
| `webhook_url` | Webhook mode | — | Public URL for Telegram to send updates to |
| `webhook_listen` | No | `:9094` | Local address for the webhook HTTP server |
| `webhook_secret` | No | — | Secret token for webhook request validation |
| `db_path` | No | `telegram_v2.db` | Path to SQLite database for persistent state |
| `send_queue_size` | No | `100` | Max queued outbound messages per chat |
| `send_min_delay` | No | `50ms` | Minimum delay between sends (rate-limit protection) |
| `agent_cache_ttl` | No | `5m` | TTL for cached agent lists from the hub |
| `api_base_url` | No | — | Override Telegram API base URL (for testing) |

### Hub Environment Variables

| Variable | Value | Description |
|----------|-------|-------------|
| `SCION_TELEGRAM_V2` | `1` | **Required.** Enables the v2 broker with dynamic group linking |
| `SCION_MAINTENANCE_REPO_BRANCH` | e.g., `scion/chat-tee` | Optional. Use a development branch for hub provisioning |

### Example settings.yaml (Complete)

```yaml
server:
  message_broker:
    enabled: true
    types:
      - broker-log
      - telegram

plugins:
  broker:
    broker-log:
      self_managed: true
      address: "localhost:9091"
    telegram:
      config:
        bot_token: "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"
        inbound_mode: webhook
        webhook_url: "https://hub.example.com/telegram/webhook"
        webhook_listen: ":9094"
        webhook_secret: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
        db_path: /var/lib/scion/telegram_v2.db
```

## Standalone / HA Mode (Mode 3)

Standalone mode runs the Telegram broker as an independent service with Postgres-backed state. Multiple replicas can process webhook updates concurrently; only the lock holder registers the webhook with Telegram.

### Requirements

- Postgres database (shared with the Scion hub or dedicated)
- Public HTTPS endpoint for Telegram webhook delivery (Cloud Run, k8s ingress, etc.)
- **Webhook mode only** — long-poll is not supported in standalone mode

### Standalone Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | **Yes** | — | Postgres connection URL |
| `TELEGRAM_BOT_TOKEN` | **Yes** | — | Telegram bot token |
| `TELEGRAM_WEBHOOK_URL` | **Yes** | — | Public HTTPS URL for webhook delivery |
| `TELEGRAM_WEBHOOK_SECRET` | No | — | Secret token for webhook validation |
| `TELEGRAM_WEBHOOK_LISTEN` | No | `:9094` | Listen address for webhook HTTP server |
| `TELEGRAM_HUB_URL` | No | — | Hub API URL for inbound message delivery |
| `TELEGRAM_HMAC_KEY` | No | — | HMAC key for hub authentication |
| `TELEGRAM_BROKER_ID` | No | — | Broker identifier for HMAC signing |
| `GRPC_PORT` | No | `50051` | gRPC server listen port |

### Running Standalone

```bash
DATABASE_URL="postgres://..." TELEGRAM_BOT_TOKEN="..." TELEGRAM_WEBHOOK_URL="https://..." \
  ./scion-plugin-telegram --standalone
```

### Docker

```bash
docker build -t scion-telegram -f extras/scion-telegram/Dockerfile .

docker run -e DATABASE_URL="postgres://..." \
           -e TELEGRAM_BOT_TOKEN="..." \
           -e TELEGRAM_WEBHOOK_URL="https://..." \
           -p 9094:9094 -p 50051:50051 scion-telegram
```

### Webhook Registration Lock

In multi-replica deployments, a Postgres advisory lock serializes webhook registration. Only the lock holder calls Telegram's `setWebhook` API. All instances process incoming webhook updates concurrently.

### SQLite to Postgres Migration

```bash
./scion-plugin-telegram migrate --from /var/lib/scion/telegram_v2.db --to "postgres://..."
```

The migration is **read-only** on the source and **idempotent** on the target.

## Architecture

```
Telegram Bot API
     │
     ▼
 ┌──────────────────┐   webhook / poll    ┌──────────────────────┐
 │  Telegram Groups  │ ◄───────────────── │  scion-plugin-       │
 │  & DMs            │ ──────────────────►│  telegram             │
 └──────────────────┘   Bot API sends     │                      │
                                          │  ┌─ CommandHandler   │
                                          │  ├─ CallbackHandler  │
                                          │  ├─ RegistrationHndlr│
                                          │  └─ SendQueue        │
                                          │        │             │
                                          │  SQLite / Postgres   │
                                          └──────────┬───────────┘
                                                     │ go-plugin RPC
                                                     ▼
                                          ┌──────────────────────┐
                                          │     Scion Hub        │
                                          │   (FanOutBroker)     │
                                          │                      │
                                          │  ┌─ broker-log       │
                                          │  ├─ telegram  ◄──────│
                                          │  └─ chat-app         │
                                          └──────────────────────┘
```

- **FanOutBroker spoke:** The plugin runs as one of potentially several broker spokes (alongside `broker-log`, `chat-app`, etc.). The hub publishes messages to all configured spokes concurrently.
- **Webhook mode is strongly recommended** over polling — it eliminates 409 conflict errors when multiple instances run and provides real-time delivery.
- **Registration** uses a hub-issued 6-character code. The user generates a code via `/register` in Telegram DM, then enters it on the hub's `/profile/telegram` page. This works with any hub auth mode.
- **SQLite / Postgres state** persists group links, user mappings, conversation contexts, notification preferences, and pending ask-user callbacks across restarts.
- **Send queue** uses per-chat worker goroutines with configurable rate limiting to avoid Telegram 429 errors.
