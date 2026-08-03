---
title: External Channels
description: Connect Scion to Telegram, Discord, and A2A for external messaging and notifications.
---

## Overview

Scion can relay agent messages and notifications to external platforms, extending communication beyond the CLI and Web Dashboard. Three channels are available: **Telegram** (bidirectional group chat), **Discord** (bidirectional chat and outbound notifications), and **A2A protocol** (expose agents as A2A endpoints for programmatic interaction).

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
- **Body mentions:** Mentions inside the body of a message (e.g., `@agent-b`) are delivered as lightweight `TypeMention` (`mention`) notifications instead of full instructions, preventing accidental multi-agent execution loops. If a message contains only body mentions, the group's default agent is restored as the primary recipient.
- Available bot commands: `/agents` (list agents), `/default` (set default), `/settings` (configure group), `/notifications` (toggle notification types).

### Group Settings

Each linked group can be configured via `/settings`:

- **Observer mode (`a2a`):** Show agent-to-agent messages in the group, so you can watch how agents coordinate.
- **Commentary:** Show agent reply messages (responses to other agents) in the group.
- **Group notifications (`grp`):** Post agent state change notifications (completed, error, waiting for input) in the group chat.

For a guided Workstation walkthrough, see [Setting Up Telegram](/scion/getting-started/telegram/). For advanced deployment (webhook mode, HA/standalone, `settings.yaml` reference), see [extras/scion-telegram/README.md](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-telegram).

## Discord

Scion supports Discord through two distinct integration models depending on your needs:

1. **Bidirectional Chat Integration (Full Discord Bot):** A rich, interactive experience where users can chat with agents in real-time, execute slash commands, and receive agent replies directly in linked channels.
2. **Direct Hub Webhook (Outbound-only notifications):** A simple, outbound-only mechanism for broadcasting system alerts, agent messages, and user-input prompts to a specific channel.

### 1. Bidirectional Chat Integration (Full Bot)

The full Discord bot (`scion-plugin-discord`) operates as a bidirectional bridge, allowing you to run slash commands and hold conversations with agents directly from Discord.

- **Slash Command Suite:** Use `/scion setup` to link a channel to a project, `/scion register` to securely link your Discord account with your Scion Hub identity, `/scion default` to select a default agent, and `/scion agents` or `/scion status` to check agent state.
- **Rich Conversations:** Message agents via `@mention` (e.g. `@my-agent-slug hello`) or set a default agent for the channel to route plain-text messages automatically.
- **Custom Agent Avatars:** Each agent replies using its own name and a custom avatar (powered by Discord webhooks and RoboHash), making agent-to-agent interactions clear and readable.
- **Multi-Server (Multi-Guild) Support:** A single bot instance can serve multiple servers simultaneously. Admins can configure `guild_ids` for instant command registration on listed servers.
- **Outage Protection:** Automatically deactivates channel links when the bot is removed from a server, while protecting active links against temporary Discord API outages.

:::tip[Workstation quick start]
Ready to set up the bidirectional Discord bot? Follow the step-by-step
[Setting Up Discord](/scion/getting-started/discord/) walkthrough — bot creation, plugin
configuration, server invites, and your first message.
:::

For advanced standalone/HA deployment and `settings.yaml` reference, see [extras/scion-discord/README.md](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-discord).

### 2. Direct Hub Webhook (Outbound-only)

If you only need outbound alerts and don't require bidirectional chat, you can configure the Hub server to post directly to a Discord webhook.

- **Severity-based color coding:** Messages are color-coded in Discord based on their severity (info, warning, error, urgent).
- **@mentions:** Urgent messages and explicit `ask_user` requests can trigger `@user` or `@role` mentions.
- **Per-Agent Webhook Identity:** Outbound webhook messages are automatically posted under the actual sending agent's webhook identity and avatar, rather than the topic agent's.
- **Observed Message Styling:** Relayed agent-to-agent (observed) messages feature a distinct gray-sidebar embed styling and a `Sender → Recipient` title format, making them easy to identify and distinguish from direct messages.

#### Configuration

Set the webhook URL in one of two ways:
- **settings.yaml:** Set `server.discord_webhook_url` in the Hub configuration.
- **Environment variable:** Set `SCION_DISCORD_WEBHOOK_URL`.

For more details, see [Hub Setup — Discord Integration](/scion/hosted/single-node/hub-server/#discord-integration).

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

### Per-User Authentication & Isolation

When the A2A bridge is configured with per-user authentication, callers present their own individual credentials instead of a shared static API key. This activates **CallerIdentity context propagation** and granular task isolation.

#### The Two Per-User Schemes

The A2A bridge supports two per-user authentication schemes, specified via `auth.scheme` in the bridge configuration:

1. **`hubUAT` (Recommended for Desktop App Federation)**:
   - Callers present a Scion User Access Token (`Authorization: Bearer scion_pat_...`) created via `scion token create`.
   - **Validation (`UATValidator`)**: The bridge uses a `UATValidator` component to dynamically introspect each token by calling the Hub's `/api/v1/auth/me` endpoint.
   - **SHA-256 Keyed Cache**: To avoid overwhelming the Hub with authentication requests, validated tokens are cached in memory using a SHA-256 hash of the token as the key.
   - **Configurable TTL**: The cache TTL is configurable via `auth.uat_cache_ttl` (default: `60s`, maximum: `300s`). If a user revokes their UAT, access is completely cut off once the cache TTL expires (within 60 seconds by default).
2. **`hubJWT` (Recommended for CLI & Scripted Access)**:
   - Callers present a Scion-signed User JWT.
   - **Local Validation**: The bridge validates the JWT signature locally using the HS256 `hub.signing_key` secret. Since this happens locally, it requires no active API calls to the Hub, making it extremely fast.

#### Per-User Isolation Benefits

* **Task Ownership**: Users can only see, query, and cancel/interrupt tasks they created. One user cannot view or modify the active tasks of another user.
* **Audit Trails**: All downstream Hub API calls made by the bridge (such as sending messages or interrupting containers) propagate the user's actual `CallerIdentity`. The Hub's audit logs will show the real user's identity as the initiator rather than the bridge admin's service account.

:::note[Bridge Operator Configuration]
To enable per-user authentication, edit `scion-a2a-bridge.yaml`:

```yaml
auth:
  scheme: "hubUAT" # or "hubJWT"
  
  # Optional: TTL for caching validated UATs (default: 60s, max: 300s)
  uat_cache_ttl: 60s
```

* Under these schemes, the static `auth.api_key` field is not required and can be omitted.
* Ensure `hub.signing_key` is configured so that local JWT signature verification works for `hubJWT` or token decryption works securely.
* For a complete configuration example, see the [sample config](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-a2a-bridge/scion-a2a-bridge.yaml.sample).
:::
