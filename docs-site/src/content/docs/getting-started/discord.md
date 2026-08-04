---
title: Setting Up Discord
description: An end-to-end walkthrough for Workstation-mode users — create a Discord application and bot, configure the Discord plugin from the Hub admin UI with multi-server (multi-guild) registration, invite the bot, link your account, and start chatting bidirectional with your agents.
---

**What you will learn**: How to go from zero to chatting with your Scion agents over Discord — creating a Discord bot, configuring the plugin from your local Hub's web UI (including multi-server guild ID configuration), inviting the bot with correct permissions, registering your identity, linking a channel to a project, and holding your first multi-agent conversation.

This guide is for [Workstation mode](/scion/choosing-a-mode/): you installed Scion with Homebrew and run the combo server locally with `scion server start`. It assumes you have already finished [installation](/scion/getting-started/install/) and the [Onboarding Wizard](/scion/getting-started/onboarding/), and that you can open the web dashboard at `http://127.0.0.1:8080`.

:::note[Already installed the plugin?]
The Homebrew install includes `scion-plugin-discord` automatically — no separate build or download is needed. If you installed from source instead, see the [plugin README](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-discord) for build instructions, then rejoin this guide at [Step 2](#step-2-configure-the-plugin-from-the-hub-admin-ui).
:::

At a glance, you will:

1. [Create a Discord bot and application](#step-1-create-a-discord-bot-and-application) in the Discord Developer Portal and enable required intents.
2. [Configure the plugin](#step-2-configure-the-plugin-from-the-hub-admin-ui) via the Hub admin integrations page with your token, application ID, and allowed server (guild) IDs.
3. [Invite the bot](#step-3-invite-the-bot-to-your-server) to your server(s) using the auto-generated invite button.
4. [Link a Discord channel](#step-4-link-a-discord-channel-to-a-project) to your Scion project.
5. [Register your identity](#step-5-register-your-discord-identity) so Scion knows who you are.
6. [Message an agent](#step-6-message-an-agent) and see replies showing up under distinct agent names and avatars.

---

## Step 1: Create a Discord bot and application

To run a Scion Discord integration, you must register a bot application with Discord.

1. Open the [Discord Developer Portal](https://discord.com/developers/applications) and sign in.
2. Click **New Application** in the top-right. Name your application (e.g. `Scion Orchestration`) and click **Create**.
3. Under the **General Information** tab, copy the **Application ID** (Client ID) — you will need this in Step 2.
4. Go to the **Bot** tab on the left sidebar:
   - Click **Add Bot** if prompted.
   - Under the Username field, click **Reset Token**, confirm, and **Copy** the new token string. Keep this token highly secure.
5. In the same **Bot** tab, scroll down to the **Privileged Gateway Intents** section. You **must** enable:
   - **Server Members Intent** (required for username caching and verification).
   - **Message Content Intent** (required for reading agent messages and slash commands).
6. Click **Save Changes** at the bottom of the page.

:::caution[Treat the token like a password]
Anyone with this token can control your bot. Keep it secret and never commit it to a repo. You will paste it into the Hub admin UI in the next step; Scion stores it in its secrets backend, not in plaintext config files.
:::

---

## Step 2: Configure the plugin from the Hub admin UI

With Workstation mode running (`scion server start`), configure the Discord plugin from your web browser.

1. Open the web dashboard at `http://127.0.0.1:8080`.
2. In the left sidebar, open the **Admin** section and click **Integrations** (the plug 🔌 icon). This takes you to `/admin/integrations`.
3. Find **discord** in the list:
   - If it appears under **Available Integrations**, click **Install** first.
   - Otherwise, click the **discord** row to open its detail page (`/admin/integrations/discord`).
4. Enter your credentials under **Integration Settings**:
   - **Bot Token**: Paste the token copied from Step 1.
   - **Application ID**: Paste the Application ID copied from Step 1.
   - **Allowed Guild IDs**: (Optional) Enter a comma-separated list of Discord server IDs (also known as Guild IDs) to enable instant slash command registration.

#### Finding a Discord Guild ID
To find your server's ID:
1. Open Discord, go to **User Settings** → **Advanced**, and toggle on **Developer Mode**.
2. Right-click your server's icon in the left-hand server list and click **Copy Server ID**.

#### Command Registration Modes
The way you configure **Allowed Guild IDs** changes how the bot registers its `/scion` slash commands:

| Config Field | Registration Mode | Behavior |
|---|---|---|
| **Empty** (Global) | Global Registration | Commands are available on ALL servers the bot joins. **Note:** Discord takes up to 1 hour to propagate global commands. |
| **`id1,id2`** (Guild-scoped) | Per-Guild Registration | Commands are concurrently registered and become available **instantly** on the listed servers. Recommended for testing and quick setups. |

:::note[Backward Compatibility]
If you are migrating from an older version of Scion, the single-server config key `guild_id` is automatically treated as a single-item fallback list within `guild_ids`.
:::

5. Click **Save Configuration** at the bottom of the page.

---

## Step 3: Invite the bot to your server

Once the Application ID is saved, Scion's admin UI automatically generates a **Bot Setup** invite button.

1. In the Discord integration detail page, look for the **Bot Setup** card that appears once an Application ID is saved.
2. Click the invite button. Under the hood, this opens the official Discord authorization page with the required scopes and a permission bitmask:
   `https://discord.com/api/oauth2/authorize?client_id=<APP_ID>&permissions=329101954112&scope=bot%20applications.commands`
3. Select your Discord server from the drop-down menu and click **Authorize**.

#### Required Bot Permissions
The authorization URL requests the following permissions:
- **Send Messages** & **Read Message History** (required to chat with agents).
- **View Channels** (required to access linked channels).
- **Embed Links** (required for rendering rich agent responses and status embeds).
- **Manage Webhooks** (highly recommended for **Custom Agent Avatars**).

:::tip[How Custom Agent Avatars work]
To make multi-agent discussions highly readable, Scion lazily creates a webhook named **Scion Agent Relay** inside linked channels. When an agent replies, Scion sends the message via this webhook, overriding the username to match the **agent's slug** and the avatar to a unique, procedurally-generated [RoboHash](https://robohash.org/) icon. If **Manage Webhooks** is not granted, all agent replies will fall back to the generic name and icon of your Discord bot.
:::

---

## Step 4: Link a Discord channel to a project

Now that your bot has joined your server, associate a channel with a Scion project workspace.

1. Open your Discord client, navigate to your server, and select a channel (e.g. `#scion-sandbox`).
2. Type `/scion setup` in the message input and press Enter.
   - *Note: Only users with the **Manage Channels** permission in Discord can execute `/scion setup` and `/scion unlink`.*
3. A drop-down menu will appear showing the projects configured on your Hub. Select the project you want to link.
4. The bot will reply with a confirmation message: *"Channel `#scion-sandbox` successfully linked to project `<project-slug>`."*

---

## Step 5: Register your Discord identity

To ensure security and audit trails, you must link your Discord user account to your Hub user profile.

1. Type `/scion register` in any Discord channel and press Enter.
2. The bot will respond with an ephemeral message (only visible to you) containing a **Link Account** button and a 6-character code.
3. Click **Link Account** to open the Hub profile page (`http://127.0.0.1:8080/profile/discord`) in your browser.
4. Review the connection, and confirm the 6-character registration code on the screen.
5. The web page will update with a success message, and the Discord bot will update its reply to confirm your account is fully linked.

:::note[Per-User registration]
User registration is associated with your global Scion account, not individual servers. Once registered, your identity is recognized across all Discord servers that utilize the same Scion bot token.
:::

---

## Step 6: Message an agent

With the channel linked and your identity registered, you can now start messaging agents.

### Slash Commands
Run `/scion help` to see all available commands under the `/scion` root:

| Command | Purpose |
|---|---|
| `/scion setup` | Link channel to a project. |
| `/scion default` | Set or clear the channel's default agent. |
| `/scion agents` | List agents and their real-time state. |
| `/scion status <slug>`| Display detailed state of a specific agent. |
| `/scion terminal <agent>` | Resolve an agent name via the Hub API and return its interactive web terminal URL. |
| `/scion register` | Securely link your account. |
| `/scion info` | Show active channel links and user registration status. When invoked from a thread, displays both the thread and channel defaults. |

### Routing Messages to Agents
In Discord, messages are routed using `@mention` triggers. If a default agent is set, you can also send plain-text messages directly.

| Message Example | Destination Agent |
|---|---|
| `@my-agent check this code` | Routes directly to the agent with slug `my-agent`. |
| `can you review this PR?` | Routes to the channel's **default agent** (if configured via `/scion default`). |
| `@all run safety audit` | Broadcasts the message to **all agents** running within the linked project. |
| *Reply to an agent's message* | Replying to a webhook message or bot response automatically continues the discussion thread with that specific agent. |

---

## Multi-Server Operational Details

### Trust Model
**One Bot = One Trust Domain.** All Discord servers utilizing the same bot token can access and interact with the same Scion projects and registered users. If you need complete project isolation or separate permissions boundaries across servers, deploy multiple separate Discord Applications and run them on separate Scion Hub instances.

### Guild Removal and Outage Protection
If your bot is kicked or removed from a Discord server, the plugin runs a **Guild-removal cleanup**:
- It captures the kick event and automatically deactivates all channel links for that server.
- The `guild_name` is tracked in the database (synced from Discord's session cache) for auditing and reporting in `/scion info`.
- To prevent accidental cleanup, an **outage guard** is built in. If Discord undergoes a temporary server outage, the channel links remain active and resume routing automatically once Discord services recover.

### Live Help Button
Need to consult this documentation while configuring? Click the **Help** button inside the Discord integrations admin panel to open this site in a new browser tab.
