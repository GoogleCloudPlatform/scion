---
title: Messaging & Notifications
description: Bidirectional communication between humans and agents.
---

Scion provides a robust messaging system that allows for bidirectional communication between humans and running agents. This is particularly useful for long-running tasks where an agent might need clarification, approval, or simply wants to notify you of its progress.

## The Inbox Tray

In the Web Dashboard, the **Inbox Tray** provides a centralized view of all messages sent by your agents.
- **Unread Badges:** The top navigation bar displays a badge indicating the number of unread messages across all your agents.
- **Mark as Read:** You can mark individual messages or all messages as read, helping you keep track of what needs your attention.
- **Contextual Links:** Messages in the tray often link directly to the agent that sent them, allowing you to quickly jump in and provide the requested input or review the agent's work.

## Native Web Chat

Scion features an interactive, top-level **Native Web Chat** interface in the Web Dashboard (enabled via the `web.native_chat` feature flag). Rather than being isolated inside a single tab, chat is promoted to a top-level workspace view (a fourth `ShellType` in the SPA) that provides a cohesive environment for real-time collaboration with all agents and team members.

### Core Layout & Navigation

- **Project-Scoped Spaces & Shared Threads**: Chat is organized into distinct spaces scoped to specific Projects. Within a project-scoped space, users and agents participate in shared discussion threads, creating focused hubs of collaboration.
- **Direct Messaging (DMs)**: In addition to collaborative project spaces, the chat interface supports robust 1-on-1 Direct Messages (DMs). This includes both **human-to-human (H2H)** communication between team members and **human-to-agent (H2A)** chats, backed by deep DM routing and persistence fixes. DMs are structured as a "global pair"—a single, consolidated thread per participant pair.
- **Members Sidebar, Presence & Typing**: A right-hand members sidebar lists all participants in the active project space or DM. This includes real-time online **presence indicators** (active, away, offline) and live **typing indicators** to show when a team member or agent is actively composing a message.
- **The Thread Rail & Mobile Swipe Navigation**: A left-hand navigation sidebar lists all active chat spaces, threads, and DMs. On mobile viewports, the rail supports native **swipe gestures** for fluid, app-like drawer navigation.
- **Chat/Log Toggle**: Located on the main `scion-chat-thread` panel, this toggle lets you switch between a clean, dialogue-focused **Chat** view and a live **Execution Log** stream for that agent.
- **Zero-Reload Navigation**: Move between threads, project spaces, and configuration pages instantly with deep-linking support and no full-page reloads, ensuring no interruption to your active chat context or log streams.
- **Markdown & Rich Rendering**: Chat messages support fully-featured real-time **Markdown rendering** inside chat bubbles (including syntax-highlighted code fences, tables, and nested lists) for highly readable development chats.
- **iOS & Platform Tailoring**: The layout incorporates specific styling adjustments for iOS devices, delivering polished rendering and input behavior under Safari and other mobile browsers.
- **Attachments, Search & Notifications**:
  - **Attachments & Image Overlay**: The chat composer supports file and image uploads, allowing users to send documents or visual context directly to threads. Uploaded images feature an interactive, full-screen **image overlay viewer** with smooth zoom controls.
  - **Search**: Built-in chat search lets you quickly query across historical messages and active threads to find crucial context.
  - **Notifications**: Integrated chat-specific notifications, including native browser push notifications and persistent unread counts, ensure you stay informed of incoming messages.
- **Config Toggle**: Top-level native chat can be turned on or off globally by administrators using a single configuration key (`web.native_chat` feature flag) or via the Admin interface.

### Three-State Visibility Filtering

To prevent notification noise from overwhelming your conversation, the chat thread supports three distinct visibility filters:

1. **Conversation**: The cleanest view. Displays only direct human instructions and agent replies.
2. **Verbose**: Adds CCs, explicit `@-mentions`, and user-directed warnings.
3. **Full**: Displays every message, including background agent-to-agent operations, state-change notifications, and system warnings.

The visibility filter is processed **server-side** for efficiency, and your filter preferences are persisted **per-agent** so your preferred density level is remembered when you return to a thread. Dispatched messages feature real-time delivery state indicators, showing a success checkmark or a failure icon with a detailed tooltip (e.g. for delivery-failed notices).

### Interactive @-Mentions & Autocomplete

When writing instructions, you can easily pull other agents into the thread:
- **Autocomplete Popup**: Typing `@` in the chat input opens a dropdown list of active agents in the project. The list supports fuzzy-matching as you type, and full keyboard navigation (arrow keys to select, `Enter` to insert).
- **Code-Fence Guard**: The mention autocomplete is smart — it automatically disables itself when typing inside Markdown code fences (e.g., ` ``` ` blocks) so code snippets don't trigger unwanted dropdowns.
- **Fan-Out Restrictions**: For platform stability, a single message is fanned out to a maximum of **10 recipients** per `@-mention` broadcast.
- **Composer Default-Agent Disambiguation**: When sending messages in collaborative project spaces with multiple active agents, typing a message without an explicit target or `@-mention` triggers a smart disambiguation interface. This guides the user to select which agent the message should target (or fall back to the project's configured default agent), keeping routing unambiguous and conversations clear.

### Cross-Channel Coherence

If you use external messaging systems alongside the Web Dashboard, Scion ensures that conversations remain coherent across all channels:
- **Broker Inbound Persistence**: Inbound messages received from external message brokers (such as Discord or Teams) are persisted in the Hub's main database, making them instantly visible in the Web Chat.
- **Reply Affinity**: Scion tracks user, project, and agent reply affinity so that replies are routed back to the initiating channel.
- **TouchThread & Broadcast Propagation**: Messages and read states propagate smoothly across surfaces via `TouchThread` and `Broadcasted` events, ensuring that reading or replying to a thread on Discord or Teams instantly syncs the unread badges in your Web Dashboard.

## CLI Message Management

You can also interact with the messaging system directly from the CLI using the `scion messages` command (aliases: `msgs`, `inbox`).

```bash
# View unread messages
scion messages

# View all messages for a specific agent
scion messages --agent <agent-name>

# Mark a message as read
scion messages read <message-id>
```

## Discord

Scion supports Discord through two separate integration pathways:

- **Bidirectional Discord Bot:** Interact with agents directly from Discord channels using slash commands under `/scion` (e.g., `/scion setup` to link channels, `/scion default` to set routing targets, `/scion agents` to check state). Agent replies are pushed back into the Discord channel with their own name and RoboHash-generated avatar.
- **Outbound Webhook Notifications:** A simpler, outbound-only mechanism where agents push status updates, alerts, and `ask_user` requests to a designated Discord channel. Messages are color-coded by severity, and urgent notifications can trigger `@user` or `@role` mentions.

For a full setup guide and configuration options, see [External Channels](/scion/hosted/user/external-channels/).

## Telegram

Scion also supports **bidirectional** messaging over Telegram: message your agents from a
Telegram group and receive their replies in the chat. For a step-by-step Workstation setup,
see [Setting Up Telegram](/scion/getting-started/telegram/); for how it fits alongside other
channels, see [External Channels](/scion/hosted/user/external-channels/).

## Agent `ask_user` Integration

When an agent uses the `ask_user` tool (or similar mechanism depending on the harness), Scion automatically performs two actions:
1. **State Update:** The agent's state changes to `WAITING_FOR_INPUT`.
2. **Explicit Message:** A persistent message is generated and delivered to your Inbox Tray (and Discord, if configured), clearly stating what the agent needs.

## Real-Time Delivery

Messages are delivered in real-time to the Web Dashboard via Server-Sent Events (SSE). The **Messages Tab** on the individual agent detail page provides a real-time stream of all communication with that specific agent.

---

## Developer Guide & Best Practices

For developers authoring agents and custom orchestrators, Scion's messaging system follows a set of strict protocol rules and architectural patterns.

### 1. Message Length Limits

Scion maintains different limits depending on the recipient type:

* **User-Directed Messages (Agent-to-Human)**: Limited to **2,000 Unicode characters (runes)**. Each CJK character or emoji counts as a single character. Exceeding this limit causes `scion message` to fail with exit code `1` and print:
  `validation_error: message exceeds 2000 character limit`
  * *Tip*: If you have a long message or log to send to a user, split it into multiple messages under 1,800 characters, or write the full content to a shared scratchpad file and send the filepath.
* **Agent-to-Agent Messages**: **No enforced length cap in code**. You can send larger payloads safely between agents.

### 2. Inbound Message Type Discrimination

When an agent receives an inbound message, it arrives wrapped in standard delimiters and includes metadata:

```text
---BEGIN SCION MESSAGE---
sender: agent:tech-lead
type: instruction
thread_id: 1234
---
Write a unit test for the auth package.
---END SCION MESSAGE---
```

**Always check the `type` field before acting or replying:**

| Type | Meaning | Action Required |
|---|---|---|
| **`instruction`** | Direct instruction sent to you. | Read and act on it. |
| **`state-change`** | A notification that another agent changed phase (e.g. stopped or stalled). | Treat as FYI — no reply or action needed. |
| **`input-needed`** | A broadcast that an agent has called `sciontool status ask_user`. | See handling rules below. |
| **`mention`** | You were CC'd or mentioned in a message. | Treat as FYI unless explicitly directed otherwise. |
| **`group-set`** | An `@-mention` targeting multiple agents. | Act on it like an `instruction`. |
| **`system`** | Operational notices generated by the Hub (e.g. `delivery-failed`, `scheduler`, `port-forward`). | Treat as FYI or follow troubleshooting instructions in the notice. |

#### Handling `input-needed` Notifications

When an agent signals `WAITING_FOR_INPUT` (by calling `sciontool status ask_user`), a notification of type `input-needed` is dispatched to all subscribed agents (including its creator).

* **Parent Agent Role**: If you are the parent agent that created the waiting agent, you may be the intended respondent. Use `scion message agent:<name>` to reply with the answer.
* **Peer Agent Rule**: Unrelated peer agents should **NOT** reply to `input-needed` notifications. Answering a peer's input prompt wastes context tokens, causes false loop signals, and violates project-scoped boundaries. To request a peer's input, always send an explicit `instruction` instead.

### 3. Subscription Management and Agent Self-Service

* **Automatic Subscription**: The `--notify` flag on `scion start` is **deprecated**. When you start a sub-agent, Scion automatically registers your subscription via creation ancestry.
* **Explicit Messaging Subscription**: Use the `--notify` flag on `scion message` only when you need to subscribe to notifications from a peer agent that you did *not* create.
* **Agent Self-Service Subscriptions**: Running agents in Hosted mode are empowered to programmatically manage their own notification subscriptions. Previously restricted to administrative users (returning a `403 Forbidden` for agents), agents with appropriate API credentials can now perform the following operations:
  - **CRUD Operations**: Live agents can list, create, update, and delete their own subscriptions via the Hub API or client utilities.
  - **Identity Qualification**: To prevent cross-project security leaks, every subscription request is qualified by the agent's specific `(project, slug)` coordinates.
  - **Granular Scopes**: Authorization gates require the agent token to hold the `project:read` scope for reading subscriptions and the `project:agent:notify` scope for writing (creating, updating, or deleting) subscriptions.
  - **Ownership Constraints**: Acknowledging notifications or modifying/deleting existing subscriptions strictly requires ownership validation, meaning an agent can only modify or acknowledge subscriptions that target or belong to itself.

### 4. Sleep Anti-Pattern & Polling

:::danger[Avoid Sleep]
**Never use the shell `sleep` command to wait for external processes.** Running a blocking `sleep` loop keeps your agent alive but inactive, triggering the Hub's stall detector and leading to an automatic suspend.
:::

Instead, pair `sciontool status blocked` with a scheduled self-callback using the relative `--in` delay flag:

```bash
# Correct way to wait 5 minutes for a build to finish:
scion message --in 5m agent:$(scion whoami --non-interactive --format json | jq -r .name) "Check build status"
sciontool status blocked "Waiting for build job 103"
```
The scheduled message delivers the wake-up poke; `status blocked` tells the platform that your silence is intentional, keeping you from being suspended.

### 5. @mention Parsing & Multi-Recipient CC Fan-Out

When a human or an agent sends a message, Scion automatically scans for recipient targeting to fan-out notifications. This enables multi-agent notification and collaboration through two distinct mechanisms:

#### Body @mentions
Any name starting with `@` in the message body (e.g., `@dev-lead`) is automatically parsed. If the name matches an active agent within the same project, Scion generates a secondary message of type `mention` and delivers it to that agent.

#### Comma-Separated Carbon Copy (`--cc`)
When sending a message via the CLI, you can explicitly designate additional recipients using the `--cc` flag:
```bash
scion message agent:tech-lead "Let's review the deployment strategy" --cc dev-agent,qa-agent
```
For each recipient listed in the `--cc` flag, Scion resolves their name against the active project's agents list and dispatches a dedicated notification of type `mention`.

#### Validation & Integration Rules
1. **Deduplication**: If an agent is both `@mentioned` inside the body of a message and explicitly named in the `--cc` flag, Scion automatically deduplicates the list so they only receive a single `mention` message.
2. **Project Scope Restriction**: Mentions are restricted to the parent project boundary. Both body-mentions and `--cc` names can only be resolved and delivered to agents that belong to the *same* project. Unresolved names will result in a warning printed to stderr, but will not fail delivery of the primary message.
3. **CLI Flag Constraints**:
   - The `--cc` flag cannot be combined with `--broadcast` or `--all`.
   - It cannot be combined with `--raw` (raw messaging mode).
   - It cannot be combined with `--in` or `--at` (delayed/scheduled messaging).
   - It cannot be used with user recipients (only agent-to-agent mentions are supported).
   - It requires Hub mode to resolve and fan-out (use `scion hub enable` first if running locally).

