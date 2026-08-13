---
title: External Channels
description: Connect Scion to Telegram, Discord, and A2A for external messaging and notifications.
---

## Overview

Scion can relay agent messages and notifications to external platforms, extending communication beyond the CLI and Web Dashboard. Multiple external channels are supported: **Telegram** (bidirectional group chat), **Discord** (bidirectional chat and outbound notifications), **Google Chat** (comprehensive bidirectional workspace integration), **Microsoft Teams** (enterprise-grade bidirectional channel messaging), and the **A2A protocol** (exposing agents as programmatically queryable endpoints).

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
- Available bot commands: `/agents` (list agents), `/default` (set default), `/terminal <agent>` (get web terminal URL), `/settings` (configure group), `/notifications` (toggle notification types).

### Group Settings

Each linked group can be configured via `/settings`:

- **Observer mode (`a2a`):** Show agent-to-agent messages in the group, so you can watch how agents coordinate.
- **Commentary:** Show agent reply messages (responses to other agents) in the group.
- **Group notifications (`grp`):** Post agent state change notifications (completed, error, waiting for input) in the group chat.

For a guided Workstation walkthrough, see [Setting Up Telegram](/scion/getting-started/telegram/). For advanced deployment (webhook mode, HA/standalone, `settings.yaml` reference), see [extras/scion-telegram/README.md](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-telegram).

## Discord

Scion supports Discord integration in two distinct modes: the **Interactive Discord Bot** (bidirectional messaging) and **Outbound-Only Webhook Notifications** (simple status alerts).

---

### 1. Interactive Discord Bot (Recommended)

The Interactive Discord Bot (powered by the `scion-plugin-discord` plugin) provides **bidirectional messaging** — allowing you to list, start, stop, and message Scion agents, as well as create threaded conversations with new agents directly within Discord.

When communicating, the bot automatically manages Discord webhooks on a per-agent basis. This allows each agent to appear with its own distinct persona (name and avatar) rather than as a generic bot.

#### Setup & Connection

1. **Invite the Bot**: Ensure your server administrator has invited the Scion Discord bot using an OAuth2 invite URL with the necessary permissions (specifically `Manage Webhooks` for per-agent personas, and `Use Application Commands` for slash commands).
2. **Link a Channel**: In the Discord channel where you want to interact with your agents, run:
   ```text
   /scion setup
   ```
   Select your Scion project from the resulting list to bind the channel.
3. **Register Your Identity**: To authenticate your requests, you must associate your Discord account with your Scion Hub account. Run:
   ```text
   /scion register
   ```
   Click the button/link returned by the bot to log into your Hub's profile page and complete the link.

#### Available Bot Commands

All bot interactions are handled via `/scion` slash commands:

| Command | Description |
| :--- | :--- |
| `/scion setup` | Link the current channel to a Scion project. |
| `/scion unlink` | Unlink the channel from its Scion project. |
| `/scion agents` | List all agents in the project with live statuses. |
| `/scion status <agent>` | Show the detailed status of a specific agent. |
| `/scion start <agent>` | Start a stopped agent. |
| `/scion stop <agent>` | Stop a running agent. |
| `/scion msg <agent> <text>` | Send a message to a specific agent. |
| `/scion logs <agent>` | Stream/view recent logs for an agent. |
| `/scion default [agent]` | Set or clear the default agent for the channel or thread (so unaddressed text routes automatically). |
| `/scion send <path>` | Send a file from your workspace by path or search for files. Supports container-to-host path translation. |
| `/scion secret` | Subcommand group for managing project secrets (list, get, set, delete) directly from Discord (see below). |
| `/scion thread <title> [template]` | Create a Discord thread and a Scion agent in one atomic step (see below). |
| `/scion register` | Link your Discord account to your Scion Hub identity. |
| `/scion unregister` | Unlink your Discord account from Scion Hub. |
| `/scion settings` | Configure channel-specific notification settings. |
| `/scion terminal <agent>` | Resolve an agent name via the Hub API and return its interactive web terminal URL. |
| `/scion info` | Display your linked Scion Hub registration info. When run from a thread, displays both the thread and channel defaults. |
| `/scion help` | Show the help menu. Includes the plugin's build version and git commit hash (injected via build-time `ldflags`). |

#### One-Step Thread & Agent Creation (`/scion thread`)

The `/scion thread <title> [template]` command allows you to spin up a new conversation and a corresponding agent simultaneously. 

##### How It Works:
1. **Validation & Naming**: The bot validates that the current channel is linked and that you are registered. It automatically "slugifies" your thread title into a valid Scion agent name and checks that it is unique.
2. **Template Selection**: You can specify an optional template. The command provides auto-complete to help you pick from available templates, or defaults to the project default.
3. **Concurrent Orchestration**: The bot starts a coordinated background process:
   - **In Discord**: It creates a new thread (or a forum post in forum-style channels) named after your title.
   - **On the Hub**: It triggers agent creation and starts it up.
4. **Delegated Identity (`X-Scion-On-Behalf-Of`)**: To ensure proper ownership, the bot propagates your identity to the Hub using a secure, HMAC-signed delegated identity header (`X-Scion-On-Behalf-Of` with `user:email` format). This ensures the newly created agent is attributed to your Scion user rather than being left ownerless.
5. **In-Thread Progress & Interaction**: While the agent provisions, the bot posts real-time progress updates inside the newly created thread (e.g., `"Creating agent <slug>..."`). Once successful, the bot sets the thread-wide default agent to the new agent, letting you immediately start chatting!

- **Multi-Server (Multi-Guild) Support:** A single bot instance can serve multiple servers simultaneously. Admins can configure `guild_ids` for instant command registration on listed servers.
- **Outage Protection:** Automatically deactivates channel links when the bot is removed from a server, while protecting active links against temporary Discord API outages.

#### Managing Project Secrets via Discord (`/scion secret`)

You can view and modify project-scoped secrets directly from a linked Discord channel or thread using the `/scion secret` subcommand group. To protect sensitive credentials, secret values are never typed or printed in public chat rooms; they are input securely via Discord pop-up modals, and all responses are ephemeral (visible only to you).

##### Requirements
*   **Channel Link**: The Discord channel or thread must be linked to a project (via `/scion setup`).
*   **Account Association**: Your Discord account must be registered with your Scion Hub identity (via `/scion register`). Write operations use secure `X-Scion-On-Behalf-Of` delegation.

##### Subcommands
*   **`/scion secret list`**: Lists the keys, types, and injection targets of all secrets in the linked project. Secret values themselves are never shown.
*   **`/scion secret set <key>`**: Initiates setting a secret. After specifying the key name, Discord displays an interactive pop-up modal. Enter your sensitive secret value securely inside this modal and submit. The value is securely sent to the Hub over an HMAC-signed API call and stored in your project scope.
    *   *Key Restriction*: Secret keys must not contain spaces, tabs, newlines, carriage returns, equals signs (=), or colons (:).
*   **`/scion secret get <key>`**: Displays the metadata (key, type, target) of a specific secret to verify its existence. The secret value itself is never shown.
*   **`/scion secret delete <key>`**: Permanently deletes a secret from the linked project.

#### File Transfers & Attachments

The Discord integration includes robust support for bidirectional file exchanges:

* **Container Path Translation (`/scion send <path>`)**: Agents operate natively within their container environments where paths start with `/workspace/...`. The `/scion send` command features automatic **container-to-host path translation**. When you specify a container path (e.g. `/workspace/output.json`), the Discord plugin automatically resolves and translates this path to the correct directory on the host machine for that agent's project workspace, ensuring files are located and uploaded as Discord attachments correctly.
* **Inbound Attachment Downloads**: When you upload an attachment to a Discord channel or thread linked to a Scion agent, the Discord broker automatically downloads the file.
  - **Default Path**: By default, attachments are downloaded to `/home/scion/.scion/projects/<project-slug>/downloads/` on the host, which is mounted inside the agent container at `/workspace/downloads/`.
  - **Custom Downloads Path override**: In isolated workspace modes or specialized backends where `/workspace/downloads` is not the standard target directory, you can configure the **`downloads_path`** parameter in the plugin configuration. The parameter overrides the download destination and supports the `{project_slug}` placeholder, which is expanded dynamically at runtime (e.g., `downloads_path: /custom-mounts/{project_slug}/downloads/`). When set, the agent container can access files directly at the custom path.

:::tip[Workstation quick start]
Ready to set up the bidirectional Discord bot? Follow the step-by-step
[Setting Up Discord](/scion/getting-started/discord/) walkthrough — bot creation, plugin
configuration, server invites, and your first message.
:::

For advanced standalone/HA deployment and `settings.yaml` reference, see [extras/scion-discord/README.md](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-discord).

---

### 2. Outbound-Only Webhook Notifications

If you do not need bidirectional chat or command interaction, you can configure standard outbound-only webhook notifications. In this mode, Scion simply posts status alerts (completed, error, waiting for input) directly to a Discord channel.

- **Severity-based color coding:** Messages are color-coded in Discord based on their severity (info, warning, error, urgent).
- **@mentions:** Urgent messages and explicit `ask_user` requests can trigger `@user` or `@role` mentions.
- **Per-Agent Webhook Identity:** Outbound webhook messages are automatically posted under the actual sending agent's webhook identity and avatar, rather than the topic agent's.
- **Observed Message Styling:** Relayed agent-to-agent (observed) messages feature a distinct gray-sidebar embed styling and a `Sender → Recipient` title format, making them easy to identify and distinguish from direct messages.

#### Configuration

Set the webhook URL in one of two ways:
- **settings.yaml:** Set `server.discord_webhook_url` in the Hub configuration.
- **Environment variable:** Set `SCION_DISCORD_WEBHOOK_URL`.

For more details, see [Hub Setup — Discord Integration](/scion/hosted/single-node/hub-server/#discord-integration).

## Google Chat

Scion provides a powerful **Google Chat** integration (powered by the `scion-chat-app` plugin) that lets users message agents, manage agent lifecycles, and receive real-time notifications directly from within their Google Workspace. It runs as both a Google Workspace Add-on (HTTP Service) and a message broker plugin.

### Key Capabilities

- **Bidirectional Messaging**: Chat with agents directly within Space conversations.
- **Agent and Space Administration**: Run slash commands (`/scion` to message agents, `/scionAdmin` for space/agent administration, including `terminal`, `thread`, `send`, and `secret`).
- **Thread-Level Default Agent Routing**: Set specific default agents on a per-thread basis within Google Chat spaces.
- **Card-Based Interactive Flows**: For firewall-restricted or high-security deployments where interactive dialogs are unavailable, the plugin uses rich Card-based flows for space deletion and notification subscription setups.
- **Inbound Attachment Handling**: Send files up to 25 MB directly to agent workspaces using the Google Chat API's `media.upload` pipeline, backed by strict path-traversal sanitization.
- **Reliable Message Delivery**: Deduplicates outgoing and incoming messages using a dedicated per-space send queue to protect against duplicate posts and network jitter.
- **Cloud Pub/Sub Ingress Mode**: Supports Cloud Pub/Sub ingress, allowing the plugin to run securely in firewalled, private network environments without exposing a public HTTPS webhook endpoint.
- **Observe Mode Filtering**: Optionally monitor and filter public space conversations using outbound mention resolution with settings toggles.

### Commands

Google Chat utilizes the following slash commands:

- **`/scion`**: Message, list, or control agents.
- **`/scionAdmin`**: Perform administrative tasks like starting, stopping, linking spaces to Scion projects, configuring notification subscriptions, and managing secrets.

For advanced deployment instructions, API scopes, and configuration details, see [extras/scion-chat-app/README.md](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-chat-app).

## Microsoft Teams

The Microsoft Teams integration (powered by the `scion-plugin-teams` plugin) brings Scion's bidirectional messaging and agent control directly into Microsoft Teams channels and conversations via the Azure Bot Framework.

### Key Capabilities

- **Bidirectional Messaging**: Users interact with agents in Channels or Group Chats. The inbound message path correctly maps inbound message Types (`"instruction"`), Channels (`"teams"`), ThreadIDs (fallback to normalized conversation IDs), and Recipient attributes (`agent:<slug>`).
- **Interactive Setup Cards**: Confirm setups using modern adaptive cards switching from standard submit actions to Azure Bot Framework `Action.Execute` (invoke activities), with mutex-safe state management.
- **Card-Level Agent Attribution**: While Teams utilizes a single bot identity (as defined in the downloadable App Manifest), outbound cards are styled as **Adaptive Cards** and explicitly display the sending agent's name (bolded, accent-colored) and project slug in the header for clear attribution.
- **Resilient Suffix Normalization**: Automatically normalizes conversation IDs using `stripThreadSuffix()` across all channel links, ensuring setup confirmation persistence succeeds across Teams restarts and multi-instance deployments.
- **Authentication**: Bidirectional communication is secured via Azure Active Directory (Azure AD) OAuth2 and JWT validation.
- **Flexible Storage**: Supports both local SQLite and robust PostgreSQL backends, migrating account link-codes from memory maps to DB for high-availability setups.
- **Sideloading and Deployment**: Admins configure the plugin via the Scion Admin UI, download the automatically compiled App Manifest package (version `1.1.0` for smooth Admin Center updates, `.zip` format), and sideload or publish it to the Teams Admin Center.
- **Channel Prefix Resiliency**: Handles the hidden `28:` bot prefix Teams adds to bot entity IDs in channel contexts, ensuring slash and bot commands work flawlessly in all group contexts.

### Bot Commands

Interact with the Teams bot using these `@-mention` commands:

- **`@BotName setup`**: Binds a Teams channel or group conversation to a specific Scion project.
- **`@BotName register`**: Pairs your Teams account with your Scion Hub identity.
- **`@BotName unregister`**: Unlinks your Teams account from the Hub.
- **`@BotName agents`**: Lists all running agents within the bound project.
- **`@BotName default [agent]`**: Sets or clears the channel-specific default routing agent, matching the Discord integration behavior and allowing untagged messages to route automatically.

For step-by-step setup guides, App Manifest templates, and Azure AD registration details, see the [Microsoft Teams Plugin Guide](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-teams/README.md).

## A2A Protocol Bridge

The **A2A (Agent-to-Agent) Protocol Bridge** exposes Scion agents as standard A2A endpoints, enabling external A2A-compatible clients to discover and interact with them programmatically.

* **Universal Discovery**: External clients can query available agents and discover their capabilities using standard A2A Agent Cards.
* **Flexible Interaction Modes**: Supports blocking (synchronous request/response), SSE streaming (real-time token updates), and push notification deliveries (async webhooks).
* **Desktop App Integration**: Integrates directly with desktop wrappers like Claude Desktop or Codex Desktop using per-user User Access Token (UAT) authentication.

:::tip[Dedicated A2A Documentation]
For complete architectural details, step-by-step installation guides (including Hub Admin UI configuration), YAML reference, desktop federation guides, and troubleshooting steps, please see the dedicated [A2A Protocol Bridge](/scion/hosted/user/a2a-bridge/) documentation.
:::
