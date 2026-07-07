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

## Discord Notifications

For teams or individuals who prefer external notifications, Scion supports native Discord webhooks.

- **Severity-Based Color Coding:** Messages are color-coded in Discord based on their severity (e.g., info, warning, error, urgent).
- **Mentions:** Urgent messages or explicit `ask_user` requests can trigger `@user` or `@role` mentions in Discord, ensuring that critical requests don't go unnoticed.

To configure Discord notifications, see the [Hub Administration Guide](/scion/hub-admin/hub-server/#discord-integration).

## Agent `ask_user` Integration

When an agent uses the `ask_user` tool (or similar mechanism depending on the harness), Scion automatically performs two actions:
1. **State Update:** The agent's state changes to `WAITING_FOR_INPUT`.
2. **Explicit Message:** A persistent message is generated and delivered to your Inbox Tray (and Discord, if configured), clearly stating what the agent needs.

## Real-Time Delivery

Messages are delivered in real-time to the Web Dashboard via Server-Sent Events (SSE). The **Messages Tab** on the individual agent detail page provides a real-time stream of all communication with that specific agent.
