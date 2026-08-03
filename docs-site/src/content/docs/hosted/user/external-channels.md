---
title: External Channels
description: Connect Scion to Telegram, Discord, and A2A for external messaging and notifications.
---

## Overview

Scion can relay agent messages and notifications to external platforms, extending communication beyond the CLI and Web Dashboard. Three channels are available: **Telegram** (bidirectional group chat), **Discord** (outbound webhook notifications), and **A2A protocol** (expose agents as A2A endpoints for programmatic interaction).

## Telegram

The Telegram integration provides **bidirectional messaging** — users can message agents from Telegram groups and receive replies directly in the chat.

### How It Works

- A Telegram bot (created via [@BotFather](https://core.telegram.org/bots#botfather)) acts as the bridge between Telegram groups and the Scion Hub.
- The bot runs as a Hub plugin (`scion-plugin-telegram`). Homebrew installs it automatically; you configure it from the Hub admin UI (or, for a from-source install, in the Hub's `settings.yaml`).
- **Group linking:** Use the `/setup` bot command in a Telegram group to link it to a Scion project.
- **Identity linking:** Use `/register` to associate your Telegram account with your Scion Hub identity.

:::tip[Workstation quick start]
New to Telegram in Workstation mode? Follow the step-by-step
[Setting Up Telegram](/scion/getting-started/telegram/) tutorial — bot creation, plugin
configuration from the web UI, registration, and your first message.
:::

### Routing & Commands

- **@-mention routing:** Mention a specific agent (e.g., `@mybot agent-name message`) to route a message to that agent.
- **Default agent:** Set a default agent with `/default` so untagged messages route automatically.
- Available bot commands: `/agents` (list agents), `/default` (set default), `/settings` (configure group), `/notifications` (toggle notification types).

### Group Settings

Each linked group can be configured via `/settings`:

- **Observer mode (`a2a`):** Show agent-to-agent messages in the group, so you can watch how agents coordinate.
- **Commentary:** Show agent reply messages (responses to other agents) in the group.
- **Group notifications (`grp`):** Post agent state change notifications (completed, error, waiting for input) in the group chat.

For a guided Workstation walkthrough, see [Setting Up Telegram](/scion/getting-started/telegram/). For advanced deployment (webhook mode, HA/standalone, `settings.yaml` reference), see [extras/scion-telegram/README.md](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-telegram).

## Discord

Scion supports Discord through two distinct integration modes depending on your needs:

1. **Bidirectional Chat Bot (Plugin Mode)**: A full two-way chat interface where you can message agents, run commands, register your identity, and interact with agents directly from Discord.
2. **Outbound Webhook Notifications (Simple Mode)**: A simpler, one-way notification-only pipeline that pushes status updates, alerts, and `ask_user` requests from the Hub to a Discord channel.

---

### 1. Bidirectional Chat Integration (Discord Plugin)

The bidirectional Discord integration connects Discord channels to your Scion projects, allowing your team to chat with agents, receive state updates, and run administrative tasks from inside Discord.

#### How It Works

- The Discord bot runs via the `scion-plugin-discord` plugin.
- **Channel linking**: Link any Discord channel to a Scion project with the `/scion setup` command.
- **Identity mapping**: Associate your Discord account with your Scion Hub identity with `/scion register`.
- **Per-Agent Personas**: Replies from agents appear in the channel with each agent's specific name and a custom avatar (powered by lazy webhook creation). This requires the **Manage Webhooks** bot permission in Discord.

#### Bot Setup

To set up the bidirectional Discord plugin:

1. **Create an Application**: Go to the [Discord Developer Portal](https://discord.com/developers/applications) and create a **New Application**.
2. **Retrieve Credentials**: Copy the **Application ID** and **Public Key** from the *General Information* tab. Go to the *Bot* tab, click **Reset Token**, and copy the Bot Token.
3. **Enable Privileged Intents**: Under the *Bot* tab, enable **Message Content Intent** (required to read message text) and **Server Members Intent** (required to resolve user details).
4. **Invite Bot**: Go to *OAuth2* → *URL Generator*, check the `bot` and `applications.commands` scopes, select the required permissions (including **Manage Webhooks**, **Send Messages**, **Create Public Threads**, **Embed Links**, and **Read Message History**), and open the generated invite link to authorize the bot on your server.
5. **Configure `settings.yaml`**: Enable the `discord` message broker type and supply the plugin's configuration under the `server.plugins.broker.discord` block:

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
          db_path: /var/lib/scion/discord.db # Path to SQLite database
```

6. **Start the Hub**: Restart your Scion Hub server. The plugin will be discovered and run as a managed subprocess.

#### Commands & Interaction

All interactions with the Discord bot use `/scion` slash commands:

| Command | Description |
| :--- | :--- |
| `/scion setup` | Link this channel to a Scion project (requires *Manage Channels* permission). |
| `/scion unlink` | Unlink this channel from its Scion project. |
| `/scion register` | Generate a registration link to link your Discord account to your Hub profile. |
| `/scion default [agent]` | Set, change, or clear the default agent for this channel. Features an autocompleting list of available agents with case-insensitive slug validation. |
| `/scion agents` | List agents in the linked project along with their real-time state. |
| `/scion status <agent>` | Get a detailed status card for a specific agent. |
| `/scion settings` | Configure notification preferences for the channel. |

#### Message Routing

Messages are delivered to agents in two ways:
- **Direct @-mentions**: Mention an agent by their slug (e.g. `@agent-slug hello!`) to route a message to them.
- **Unaddressed messages**: If a **default agent** has been set via `/scion default`, any regular text message in the channel (without a specific @-mention) is routed to that agent automatically.

#### Channel Notification Settings

You can use `/scion settings` to toggle what notifications Scion posts to the channel (e.g. agent state changes such as completed, stopped, or error). To keep channels clean, state notifications are set to **off by default** on new channel links.

---

### 2. Outbound Webhook Notifications (Simple Mode)

If you only need outbound alert notifications and don't require bidirectional chat, you can configure a simple Discord incoming webhook.

- **Severity-based color coding**: Messages sent to Discord are styled with color-coded borders depending on their severity (info, warning, error, urgent).
- **@mentions**: Urgent messages or explicit `ask_user` requests can trigger `@user` or `@role` mentions to grab your team's attention.

#### Configuration

Create an incoming webhook in your Discord channel's settings (Integrations → Webhooks), copy its URL, and configure it on your Hub:

- **settings.yaml**: Set `server.discord_webhook_url` in the Hub configuration:
  ```yaml
  server:
    discord_webhook_url: "https://discord.com/api/webhooks/YOUR_WEBHOOK_ID/YOUR_WEBHOOK_TOKEN"
  ```
- **Environment variable**: Set `SCION_DISCORD_WEBHOOK_URL`.

For more administration details, see [Hub Setup — Discord Integration](/scion/hosted/single-node/hub-server/#discord-integration).

## A2A Protocol Bridge

The A2A (Agent-to-Agent protocol) bridge exposes Scion agents as **standard A2A endpoints**, allowing external A2A clients to discover and interact with them programmatically.

- **Discovery:** External clients can query available agents and their capabilities via the A2A protocol.
- **Interaction modes:** Supports blocking (synchronous), SSE streaming, and push notification delivery.
- **Standalone service:** Runs as a separate bridge process alongside the Hub (see `extras/scion-a2a-bridge`).

This is useful for integrating Scion agents into larger multi-agent systems or exposing them to third-party A2A-compatible clients.

For setup and configuration, see [extras/scion-a2a-bridge/README.md](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-a2a-bridge).

### Desktop App Federation (Claude Desktop, Codex Desktop)

Desktop A2A clients (such as Claude Desktop and Codex Desktop) can interact with Scion agents using per-user authentication. Each user presents their own Scion User Access Token (UAT), and the bridge propagates their identity to the Hub for audit logging and access control.

#### Prerequisites

- The bridge operator has deployed the A2A bridge with `auth.scheme: hubUAT`.
- Your Scion Hub account has access to the target project.

#### Step 1: Create a UAT

Create a Scion UAT scoped to your project with the required permissions:

```bash
scion token create --name "claude-desktop" --project <project-slug> \
  --scope agent:message,agent:read --expires 365d
```

This returns a `scion_pat_...` token. Copy it securely — it will not be shown again.

#### Step 2: Configure Your Desktop App

In your desktop A2A client's provider settings:

- **Endpoint:** `https://<bridge-host>/projects/<project-slug>/agents/<agent-slug>`
- **Auth type:** Bearer token
- **Token:** Paste your `scion_pat_...` token

To discover available agents, query the bridge's agent card:

```bash
curl https://<bridge-host>/.well-known/agent-card.json
```

Or for a specific agent:

```bash
curl https://<bridge-host>/projects/<project-slug>/agents/<agent-slug>/.well-known/agent-card.json
```

#### Step 3: Test the Connection

Verify end-to-end connectivity with a `message/send` call:

```bash
curl -X POST https://<bridge-host>/projects/<project-slug>/agents/<agent-slug>/jsonrpc \
  -H "Authorization: Bearer scion_pat_..." \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "id": "1",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "parts": [{"type": "text", "text": "Hello from desktop!"}]
      }
    }
  }'
```

#### Required Scopes

| Scope | Purpose |
|-------|---------|
| `agent:message` | Send messages to agents |
| `agent:read` | List agents and read task status |

#### Per-User Isolation

When the bridge uses `hubUAT` or `hubJWT` auth, each user's tasks are isolated:

- You can only see and cancel tasks you created.
- The Hub's audit logs reflect your identity, not the bridge admin's.
- If your UAT is revoked, access stops within 60 seconds (the bridge's cache TTL).

:::note[Bridge operator note]
To enable per-user auth, set `auth.scheme: hubUAT` in `scion-a2a-bridge.yaml`.
The `auth.api_key` field is not needed for this scheme. See the
[sample config](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-a2a-bridge/scion-a2a-bridge.yaml.sample)
for details.
:::
