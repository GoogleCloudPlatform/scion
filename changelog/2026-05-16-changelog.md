# Release Notes (2026-05-16)

This period is marked by the introduction of the Telegram broker, significant SDK expansions for Python and TypeScript, and the completion of the "Grove" to "Project" rebranding initiative.

## ⚠️ BREAKING CHANGES
* **[Core]: Grove to Project Rebranding.** The term "Grove" has been officially replaced with "Project" across the system. This includes database schema updates, API endpoint changes (e.g., `/groves/` to `/projects/`), and message attribute renames (e.g., `grove_id` to `project_id`). While backward compatibility layers have been added for many areas, users should update their scripts and integrations.
* **[Agent State]: State Constant Renamed.** `ActivityIdle` has been renamed to `ActivityWorking` to more accurately reflect an agent's active processing state. Any external systems monitoring this specific string will need to be updated.

## 🚀 Features
* **[Messaging]: Telegram Broker V2.** A comprehensive new integration for Telegram, providing a robust interface for interacting with Scion agents via a bot:
    * **Interactive UI:** Support for inline keyboards, bot commands, and @-mention routing.
    * **Security:** Integrated hub-verified device registration flow for linking Telegram users to Scion identities.
    * **Reliability:** Built-in SQLite persistence for message tracking, automatic retry/backoff for transient errors, and message ID deduplication.
* **[SDKs]: Python and TypeScript Foundations.** Major expansion of Scion's developer tools:
    * **Resource Modules:** Both SDKs now include dedicated modules for managing Agents, Messages, Projects, and Secrets.
    * **Streaming Support:** Implemented Server-Sent Events (SSE) support in both Python and TypeScript for real-time log and event streaming.
    * **Documentation:** Added comprehensive READMEs and code examples for both languages.
* **[Messaging]: Wake Capability.** Added a new `--wake` flag to the `scion message` command. This feature handles agent wake-up transitions, ensuring messages are delivered only after the agent has transitioned to a ready state.
* **[Core]: Multi-Broker Fan-Out.** Introduced `FanOutBroker` in the Hub, enabling the delivery of a single message across multiple broker types simultaneously.
* **[CLI]: Composite Recipient Syntax.** The `scion message` command now supports a `set[]` syntax for addressing multiple recipients in a single command.
* **[Skills]: Skill Bank Expansion.** Implemented Phase 1 of the Skill Bank, including new CLI commands for skill management and integration of skill resolution into the agent provisioning pipeline.

## 🐛 Fixes
* **[Infrastructure]: Migration Resilience.** Hardened database migrations (notably V53 and V50/51) to be resilient against missing tables and improve reliability during updates.
* **[Installation]: Standardized Paths.** The CLI installer now defaults to `/usr/local/bin` and includes a validation check to warn users if the destination is not in their system `PATH`.
* **[Core]: Terminology Alignment.** Completed a massive sweep to align all internal logs, tests, and documentation with the new "Project" terminology.
* **[Messaging]: Hub Subscription Logic.** Resolved a feedback loop issue in the FanOutBroker and prevented double-delivery of user messages.
