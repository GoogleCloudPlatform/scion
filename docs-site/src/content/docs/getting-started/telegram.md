---
title: Setting Up Telegram
description: An end-to-end walkthrough for Workstation-mode users — create a bot with BotFather, configure the Telegram plugin from the Hub admin UI, link your account and a group, and start chatting with your agents from Telegram.
---

**What you will learn**: How to go from zero to chatting with your Scion agents over
Telegram — creating a bot, configuring the Telegram plugin from your local Hub's web UI,
registering your identity, linking a group to a project, and sending your first message.

This guide is for [Workstation mode](/scion/choosing-a-mode/): you installed Scion with
Homebrew and run the combo server locally with `scion server start`. It assumes you have
already finished [installation](/scion/getting-started/install/) and the
[Onboarding Wizard](/scion/getting-started/onboarding/), and that you can open the web
dashboard at `http://127.0.0.1:8080`.

:::note[Already installed the plugin?]
The Homebrew install includes `scion-plugin-telegram` automatically — no separate build or
download is needed. If you installed from source instead, see the
[plugin README](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-telegram)
for build instructions, then rejoin this guide at [Step 2](#step-2-configure-the-plugin-from-the-hub-admin-ui).
:::

At a glance, you will:

1. [Create a bot with BotFather](#step-1-create-a-bot-with-botfather) and turn **off** its group privacy.
2. [Configure the plugin](#step-2-configure-the-plugin-from-the-hub-admin-ui) by pasting the bot token into the Hub admin UI.
3. [Register your identity](#step-3-register-your-telegram-identity) so Scion knows who you are.
4. [Link a group to a project](#step-4-link-a-group-to-a-project) and pick a default agent.
5. [Message an agent](#step-5-message-an-agent) and see its replies in Telegram.

---

## Step 1: Create a bot with BotFather

Telegram bots are created by talking to Telegram's own bot, **@BotFather**.

1. Open Telegram and search for [`@BotFather`](https://t.me/BotFather) (look for the blue
   verified checkmark), then open the chat and press **Start**.
2. Send the command:

   ```text
   /newbot
   ```
3. BotFather asks for a **name** (a display name, e.g. `My Scion Agents`) and then a
   **username** (must be unique and end in `bot`, e.g. `my_scion_agents_bot`).
4. BotFather replies with your **bot token** — a string that looks like:

   ```text
   123456789:ABC-DEF1234ghIkl-zyx57W2v1u123ew11
   ```

:::caution[Treat the token like a password]
Anyone with this token can control your bot. Keep it secret and never commit it to a repo.
You will paste it into the Hub admin UI in the next step; Scion stores it in its secrets
backend, not in a plaintext config file.
:::

### Turn OFF Group Privacy (required)

By default, a Telegram bot in a group chat only receives messages that start with `/` — it
cannot see ordinary messages. For agent routing (@-mentions and default-agent messages) to
work, you **must** disable privacy mode:

1. In the BotFather chat, send `/mybots`.
2. Select your bot from the list.
3. Tap **Bot Settings** → **Group Privacy**.
4. Tap **Turn off**.

**What you should see:** BotFather confirms with *"Privacy mode is disabled for …"*.

:::caution[Re-add the bot to existing groups]
Privacy mode is read when the bot joins a group. If you already added the bot to a group
**before** disabling privacy, **remove and re-add** the bot so the change takes effect.
:::

You can verify privacy mode any time with this API call (replace `<TOKEN>`):

```bash
curl -s "https://api.telegram.org/bot<TOKEN>/getMe" | grep can_read_all_group_messages
```

`can_read_all_group_messages` should be `true`.

---

## Step 2: Configure the plugin from the Hub admin UI

With Workstation mode running (`scion server start`), you configure the Telegram plugin
entirely from the browser — no config files to edit.

1. Open the web dashboard at `http://127.0.0.1:8080`.
2. In the left sidebar, open the **Admin** section and click **Integrations** (the
   plug 🔌 icon). This takes you to `/admin/integrations`.
3. Find **telegram** in the list:
   - If it appears under **Available Integrations**, click **Install** first, then open it.
   - Otherwise, click the **telegram** row to open its detail page
     (`/admin/integrations/telegram`).

**What you should see:** a detail page with a yellow banner reading
*"Telegram requires setup to operate — Enter your Bot Token below to get started."*

4. In the **Secrets** section, find **Bot Token** (marked **Required**) and paste the token
   from Step 1. There is an optional **Webhook Secret** field — leave it blank for a typical
   workstation setup (see the note on inbound mode below).
5. In the **Configuration** section, leave **Inbound Mode** set to its default, `poll`.

   :::tip[Poll vs. webhook on a workstation]
   `poll` (long-polling) is the right choice for Workstation mode: your local Hub reaches
   out to Telegram, so **no public HTTPS URL is required**. `webhook` mode needs Telegram
   to reach your machine from the internet and is meant for hosted deployments. Stick with
   `poll` and you can ignore the Webhook URL / Webhook Listen fields.
   :::
6. Click **Save Configuration**.

   **What you should see:** a green *"Configuration saved successfully"* message.
7. Click **Restart** so the plugin picks up the new token.

**What you should see:** after a moment, the **Status** section shows **Connected** and a
**healthy** badge. The integration list at `/admin/integrations` also shows telegram as
**Connected**.

:::note[The message broker turns itself on]
You do **not** need to manually enable a message broker or edit `settings.yaml`. Scion
auto-enables the message broker whenever a broker plugin is configured, and adds
`telegram` to the broker types for you. (Advanced users can still set
`server.message_broker` and `plugins.broker.telegram` in `settings.yaml` directly — see the
[plugin README](https://github.com/GoogleCloudPlatform/scion/tree/main/extras/scion-telegram)
— but it is not required here.)
:::

---

## Step 3: Register your Telegram identity

Scion needs to know which Scion user a Telegram account belongs to. You do this once, from a
**direct message** with your bot.

1. In Telegram, open a **direct chat** with your bot (search for its `@username`) and press
   **Start**.
2. Send:

   ```text
   /register
   ```
3. The bot replies with a **Link Telegram account** button. Tap it (on Telegram Desktop) or
   tap-and-hold → *Open in …* your browser. The link opens your Hub's Telegram profile page
   with a one-time code attached (`/profile/telegram?code=…`).

   :::tip[Open it where you're logged into the Hub]
   The link must open in a browser that can reach your Hub **and** where you are signed in.
   On a workstation that is the same machine at `http://127.0.0.1:8080`, so using **Telegram
   Desktop** on that machine is the smoothest path. See
   [Troubleshooting](#registration-link-wont-open) if you started `/register` from your phone.
   :::
4. The page verifies the code automatically and shows *"Telegram account linked
   successfully!"*.

**What you should see:** back in Telegram, the bot sends a confirmation:
*"Linked! You are you@example.com"*.

The code expires after **15 minutes**. If it lapses, just send `/register` again.

:::note[Manual code entry]
If the automatic link doesn't work, open **Profile → Telegram** in the dashboard and type
the 6-character code into the form there. You can check your status anytime by sending
`/status` to the bot in a DM.
:::

---

## Step 4: Link a group to a project

Agents live in Scion **projects**. To talk to them, you link a Telegram **group** to one of
your projects. (Group chats — not DMs — are where agent conversations happen.)

1. Create a Telegram group (or use an existing one) and **add your bot** to it as a member.
2. In the group, send:

   ```text
   /setup
   ```
3. The bot shows a keyboard of your projects. Tap the project you want this group to talk to.
4. The bot then prompts you to choose a **default agent** for the group. Pick one (you can
   change this later).

**What you should see:** the bot confirms the group is linked to your chosen project.

Useful group commands once linked:

| Command | What it does |
| :--- | :--- |
| `/agents` | List the project's agents with live status (💤 idle, ⚙️ executing, 💭 thinking, ✅ completed, …). |
| `/default` | Set, change, or clear the default agent for the group. |
| `/settings` | Toggle group options (see below). |
| `/unlink` | Unlink the group from its project (only the person who linked it can unlink). |
| `/help` | Show the available commands. |

### Setting the default agent

`/default` controls where **unaddressed** messages (plain text with no @-mention) are
routed:

- In a normal group, `/default` sets one default agent for the whole group.
- In a **forum-style** group (Telegram groups with named *topics*), running `/default`
  **inside a topic** sets a default agent for that topic only. The routing order is
  **topic default → group default → no default**. Selecting *"No default agent"* in a topic
  reverts it to the group-wide default.

If no default is set, only explicit @-mentions and replies reach agents.

---

## Step 5: Message an agent

Make sure the linked project has at least one running agent (start one from the dashboard or
with `scion start`, and confirm with `/agents`). Then, in the linked group:

| You type | Where it goes |
| :--- | :--- |
| `hello, can you help?` | The group's (or topic's) **default agent**, if one is set. |
| `@agentslug do the thing` | The named agent. |
| `@mybot @agentslug do the thing` | The named agent (addressing the bot explicitly). |
| `@mybot status?` | The group's default agent. |
| `@all please sync up` | **Every** agent in the linked project (broadcast). |
| *(reply to a bot message)* | Continues the conversation with the **same** agent. |

The bot strips its own mention and the `@agentslug` prefix before forwarding your text to the
agent.

### Seeing agent responses

- **Agent replies** appear in the group, prefixed with 🤖 and the agent's slug.
- **State-change notifications** (completed, error, waiting for input) go to your **DM** if
  you've subscribed to them — manage these by sending `/notifications` to the bot in a DM.
- To also post state changes in the group, enable **Notify in group** via `/settings`.
- **Urgent** messages are prefixed `[URGENT]`; **broadcasts** are prefixed `[Broadcast]`.
- Messages longer than Telegram's 4096-character limit are truncated with `[truncated]`.

**What you should see:** send `@agentslug hello` and, within a few seconds, a reply in the
group prefixed with `🤖 agentslug`. 🎉 You're now driving your agents from Telegram.

:::tip[Watch agents collaborate]
Enable **Observer mode** (`a2a`) via `/settings` to see agent-to-agent messages in the group
(`👀 🤖 agentA → 🤖 agentB 👀`), and **Commentary** to see agents' replies to each other.
:::

---

## Troubleshooting

### The bot ignores regular messages but responds to `/commands`

This is the most common problem: **group privacy is still ON**. The bot can only see
`/commands` until you disable it. Revisit [Turn OFF Group Privacy](#turn-off-group-privacy-required),
then **remove and re-add** the bot to the group. Verify with:

```bash
curl -s "https://api.telegram.org/bot<TOKEN>/getMe" | grep can_read_all_group_messages
```

`can_read_all_group_messages` must be `true`.

### Messages still aren't reaching agents

Work through these in order:

1. **Group not linked** — run `/setup` in the group.
2. **You're not registered** — run `/register` in a DM so the plugin can identify you as the
   sender. Check with `/status`.
3. **No default agent and no @-mention** — either @-mention an agent or set a default with
   `/default`.
4. **No running agents** — `/agents` must list at least one agent. Start one first.

### The integration shows "Disconnected" in the admin UI

- Re-open **Admin → Integrations → telegram** and confirm **Bot Token** shows **Configured**.
- Click **Restart**, then recheck the **Status** section.
- Double-check the token you pasted has no leading/trailing spaces. Paste it again and
  **Save Configuration** if unsure.

### Registration link won't open

The `/register` button points at your Hub (e.g. `http://127.0.0.1:8080/profile/telegram`).
`127.0.0.1` only resolves on the workstation machine itself, so a link tapped on your **phone**
won't reach it. Options:

- Run `/register` from **Telegram Desktop on the same machine** as your workstation server and
  tap the button there.
- Or open **Profile → Telegram** in your local browser and enter the 6-character code manually.

### "Code expired" or "code not found"

Linking codes are valid for 15 minutes and can be verified a limited number of times. Send
`/register` again to get a fresh code.

### `/profile/telegram` shows a blank page

The Hub was built without the embedded web UI (this happens with a bare `go install`). Install
with Homebrew for a ready-to-run build, or rebuild from a clone with `make all`. See the
[installation guide](/scion/getting-started/install/#install-with-homebrew-recommended).

---

## Next steps

- **Run more agents** — the [Tutorial](/scion/getting-started/tutorial/) covers starting,
  monitoring, and cleaning up agents.
- **Understand messaging** — see [Messaging & Notifications](/scion/hosted/user/messaging/)
  for the Inbox Tray, `ask_user`, and real-time delivery.
- **Other channels** — [External Channels](/scion/hosted/user/external-channels/) covers
  Telegram alongside Discord and the A2A bridge.
