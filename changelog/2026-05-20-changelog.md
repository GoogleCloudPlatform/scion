# Release Notes (2026-05-16 - 2026-05-20)

This period is highlighted by the introduction of the Telegram Bot integration, enabling rich, multi-agent interaction directly from Telegram. Additionally, this release features a new data-driven hook mapping system and significant improvements to the Hub's message fan-out architecture.

## 🚀 Features
* **[Messaging]: Telegram Bot Integration.** Full support for Telegram as a native message broker.
    * **Interactive Commands:** Comprehensive bot commands for agent management, including `/agents`, `/setup`, `/status`, `/notifications`, and `/settings`.
    * **Rich Media & Attachments:** Seamlessly send and receive file attachments (documents and photos) between Telegram and agent workspaces.
    * **HTML Status Cards:** Real-time agent activity and state changes are delivered via beautiful, HTML-formatted cards.
    * **Multi-Agent Interaction:** Support for @mentions in group chats to target multiple agents simultaneously, and context-aware reply-to routing for natural conversations.
* **[Infrastructure]: Multi-Broker Fan-Out.** Introduced `FanOutBroker`, enabling concurrent message delivery across multiple broker plugins (e.g., In-Process, Telegram, and Logging).
* **[Hooks]: Data-Driven Mapping Dialect.** New `MappingDialect` allows developers to define hook event translations via YAML configuration, facilitating rapid integration of new harnesses without writing Go code.
* **[Web UI]: Telegram Profile Linking.** Added a dedicated Telegram registration and linking interface in the user profile, featuring a secure one-click linking flow.

## 🐛 Fixes
* **[Broker]: Automated Credential Management.** The Hub now automatically generates and injects HMAC credentials into managed broker plugins on startup, resolving persistent 401 errors and rotating secrets safely.
* **[Messaging]: Rate Limiting & Reliability.**
    * Implemented a per-chat rate-limited send queue for the Telegram plugin to gracefully handle API limits and prevent 429 errors.
    * Resolved a "double-delivery" issue where user messages were being persisted and delivered twice via SSE.
    * Improved message deduplication logic to ensure reliable delivery in multi-recipient broadcasts.
* **[Filesystem]: Attachment Path Resolution.** Hardened the resolution of attachment paths, ensuring `/workspace` and relative paths are correctly mapped to project storage while preventing path traversal vulnerabilities.
* **[Security]: Webhook Protection.** Switched to constant-time comparison for webhook secret tokens to protect against timing side-channel attacks.
* **[UI/UX]: Agent State Consistency.** Synchronized agent activity emojis (e.g., ⚙️ for executing, 💤 for idle) across the Telegram bot and the Web UI for a consistent visual experience.
* **[Core]: Context-Aware Logging.** Detached broker subscription callbacks from publisher contexts to prevent log floods and message storms during connection cancellation.
