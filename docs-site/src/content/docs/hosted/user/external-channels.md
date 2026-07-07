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
- The bot runs as a Hub plugin (`scion-plugin-telegram`), which must be built and configured in the Hub's `settings.yaml`.
- **Group linking:** Use the `/setup` bot command in a Telegram group to link it to a Scion project.
- **Identity linking:** Use `/register` to associate your Telegram account with your Scion Hub identity.

### Routing & Commands

- **@-mention routing:** Mention a specific agent (e.g., `@mybot agent-name message`) to route a message to that agent.
- **Default agent:** Set a default agent with `/default` so untagged messages route automatically.
- Available bot commands: `/agents` (list agents), `/default` (set default), `/settings` (configure group), `/notifications` (toggle notification types).

### Group Settings

Each linked group can be configured via `/settings`:

- **Observer mode (`a2a`):** Show agent-to-agent messages in the group, so you can watch how agents coordinate.
- **Commentary:** Show agent reply messages (responses to other agents) in the group.
- **Group notifications (`grp`):** Post agent state change notifications (completed, error, waiting for input) in the group chat.

For full setup instructions, bot configuration, and troubleshooting, see [extras/scion-telegram/README.md](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-telegram).

## Discord

Discord integration provides **outbound-only** webhook notifications — agents can push messages to a Discord channel, but cannot receive inbound messages from Discord.

- **Severity-based color coding:** Messages are color-coded by severity (info, warning, error, urgent).
- **@mentions:** Urgent messages and explicit `ask_user` requests can trigger `@user` or `@role` mentions.

### Configuration

Set the webhook URL in one of two ways:

- **settings.yaml:** Set `server.discord_webhook_url` in the Hub configuration.
- **Environment variable:** Set `SCION_DISCORD_WEBHOOK_URL`.

For more details, see [Hub Setup — Discord Integration](/scion/hub-admin/hub-server/#discord-integration).

## A2A Protocol Bridge

The A2A (Agent-to-Agent protocol) bridge exposes Scion agents as **standard A2A endpoints**, allowing external A2A clients to discover and interact with them programmatically.

- **Discovery:** External clients can query available agents and their capabilities via the A2A protocol.
- **Interaction modes:** Supports blocking (synchronous), SSE streaming, and push notification delivery.
- **Standalone service:** Runs as a separate bridge process alongside the Hub (see `extras/scion-a2a-bridge`).

This is useful for integrating Scion agents into larger multi-agent systems or exposing them to third-party A2A-compatible clients.

For setup and configuration, see [extras/scion-a2a-bridge/README.md](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-a2a-bridge).
