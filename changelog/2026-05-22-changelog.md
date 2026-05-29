# Release Notes (2026-05-16 - 2026-05-22)

This week's release is headlined by the introduction of the Telegram Broker Plugin, bringing native mobile and desktop chat integration to Scion agents. Alongside this, the release includes foundational improvements to the message dispatch pipeline and significant CLI optimizations for large-scale workspaces.

## 🚀 Features
* **[Messaging]: Telegram Broker Integration.** A comprehensive new plugin that enables full interaction with Scion agents via Telegram.
    * **Bot Interaction:** Send instructions, receive replies, and manage agents using standard Telegram bot commands and @-mentions.
    * **Rich Notifications:** Agent state changes (started, stalled, completed, etc.) are delivered as rich HTML-formatted status cards with emoji indicators.
    * **File Support:** Bi-directional support for file attachments; agents can send files from their workspace, and users can upload photos/documents directly to agents.
    * **Group Chat Support:** Native support for group conversations, including multi-agent mention fan-out and conversation-aware reply routing.
    * **User Linking:** A secure, self-service registration flow allows Telegram users to link their chat identity to their Scion account via the Web UI.
* **[Infrastructure]: Multi-Broker Fan-Out.** Introduced `FanOutBroker`, allowing the Hub to dispatch messages to multiple internal and external brokers (e.g., Telegram and the internal event bus) concurrently.
* **[Messaging]: Multi-Agent Awareness.** Added a `recipients` field to fanned-out messages. When multiple agents are tagged in a single conversation, each agent is now aware of the other participants, enabling better collaborative context.
* **[Harnesses]: Data-Driven Mapping Dialect.** Introduced `MappingDialect`, which allows for hook event translation via YAML configuration files. This enables the integration of new external harnesses without requiring core Go code changes.
* **[Messaging]: Observer Mode.** Enhanced visibility for agent-to-agent communications, allowing observer plugins (like the Telegram broker or loggers) to monitor inter-agent traffic without interfering with delivery.

## 🐛 Fixes
* **[CLI]: Optimized Agent Commands.** Resolved a critical "broker lockup" issue where agent-specific CLI commands (e.g., `start`, `stop`, `message`, `look`) would trigger an expensive full workspace synchronization. These commands now bypass redundant syncs, drastically improving responsiveness in workspaces with many agents.
* **[Harnesses]: Harness Type Passthrough.** Fixed a bug where non-canonical harness names were being dropped during dispatch; custom harness types are now correctly passed through from the Hub database.
* **[Messaging]: Reliability & Stability.**
    * **Context Management:** Detached subscription callbacks from publisher contexts to prevent cascading failures and log storms when a request is canceled.
    * **Rate Limiting:** Implemented per-chat rate limiting and outbound message queuing in the Telegram broker to handle Telegram API limits gracefully.
    * **Deduplication:** Fixed an issue where the `FanOutBroker` could cause duplicate message delivery in certain configurations.
    * **HTML Safety:** Improved HTML truncation and escaping in notification cards to prevent malformed tags in Telegram clients.
