# Release Notes (2026-05-30)

This release marks a major milestone with the introduction of the Telegram message broker plugin and the completion of the second phase of the "Grove to Project" architectural rename.

## 🚀 Features

* **Telegram Message Broker Plugin:** A comprehensive new integration allowing users to interact with Scion agents directly via Telegram.
    * **Interactive Commands:** Manage agents and view status using `/agents`, `/status`, and `/default`.
    * **Identity Linking:** Securely link Telegram accounts to Scion identities using the `/register` flow.
    * **Rich Notifications:** Receive real-time agent state updates (started, completed, error, input needed) with formatted HTML status cards.
    * **Intelligent Routing:** Support for @mentions and native Telegram replies in group chats to direct messages to specific agents.
    * **File Support:** Send and receive file attachments (photos and documents) directly through the chat interface.
    * **Reliability:** Built-in per-chat rate limiting and automatic retry logic for transient Telegram API errors.
* **Multi-Broker Fan-Out:** Introduced the `FanOutBroker`, enabling the Hub to dispatch messages to multiple backends simultaneously. This allows for concurrent delivery to plugins (like Telegram) while maintaining internal processing and logging.
* **Project Terminology Migration:** Completed "Phase 2" of the internal refactor renaming "Grove" to "Project" across identifiers, comments, and internal logic. This aligns the codebase with updated branding and architectural goals.

## 🐛 Fixes

* **Hub Security:** Telegram bot tokens and other sensitive broker credentials are now redacted from error logs to prevent accidental exposure.
* **Message Stability:** Resolved a "double-delivery" issue in `PublishUserMessage` where messages were occasionally duplicated when using the fan-out broker.
* **Resource Protection:** Fixed a "message storm" bug where canceled HTTP contexts could trigger excessive error logging in broker subscription callbacks.
* **Security Hardening:** 
    * Implemented IP-based rate limiting on the Telegram link verification endpoint.
    * Added path traversal protection for file attachment resolution.
    * Switched to constant-time comparison for webhook secret verification.
* **UI/UX Polishing:** Updated Telegram agent state emojis to match the web UI (💤 idle, ⚙️ executing, ✅ completed, etc.).
