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

#### Handling `input-needed` Notifications

When an agent signals `WAITING_FOR_INPUT` (by calling `sciontool status ask_user`), a notification of type `input-needed` is dispatched to all subscribed agents (including its creator).

* **Parent Agent Role**: If you are the parent agent that created the waiting agent, you may be the intended respondent. Use `scion message agent:<name>` to reply with the answer.
* **Peer Agent Rule**: Unrelated peer agents should **NOT** reply to `input-needed` notifications. Answering a peer's input prompt wastes context tokens, causes false loop signals, and violates project-scoped boundaries. To request a peer's input, always send an explicit `instruction` instead.

### 3. Subscription Management and the `--notify` Deprecation

* **Automatic Subscription**: The `--notify` flag on `scion start` is **deprecated**. When you start a sub-agent, Scion automatically registers your subscription via creation ancestry.
* **Explicit Messaging Subscription**: Use the `--notify` flag on `scion message` only when you need to subscribe to notifications from a peer agent that you did *not* create.

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

