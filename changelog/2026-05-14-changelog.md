# Release Notes (2026-05-14)

This release represents a massive milestone in the evolution of the platform, featuring a foundational terminology shift from "Groves" to "Projects," the introduction of official Python and TypeScript SDKs, a comprehensive Telegram V2 broker, and the first phase of the Skill Bank system.

## ⚠️ BREAKING CHANGES
* **[Core]: Grove to Project Renaming.** The term "Grove" has been officially retired in favor of "Project." This affects API endpoints (e.g., `/groves/` is now `/projects/`), database schemas, and CLI commands. While backward compatibility has been implemented for key API parameters (like `groveId`), users should migrate to `projectId` and update any scripts or integrations relying on the old terminology.
* **[State]: Activity State Rename.** `ActivityIdle` has been renamed to `ActivityWorking` to more accurately reflect that an agent is active and ready for tasks rather than sitting dormant. This change is reflected in the API and agent activity badges.

## 🚀 Features
* **[SDK]: Official Python & TypeScript SDKs.** We are launching the first version of our official SDKs to streamline agent development and integration.
    * **Full Resource Coverage:** CRUD support for Agents, Messages, Projects, and Secrets.
    * **SSE Streaming:** Native support for Server-Sent Events to stream agent events and logs in real-time.
    * **Async/Sync Support:** The Python SDK provides both synchronous and asynchronous clients.
* **[Broker]: Telegram V2 Plugin.** A completely re-engineered Telegram message broker plugin that provides a production-grade interface for interacting with agents via Telegram.
    * **Rich Interactions:** Support for inline keyboards, bot commands, and @-mention routing.
    * **Device Auth:** A new hub-verified device registration flow for linking Telegram users to platform accounts.
    * **Reliability:** Integrated message ID deduplication and intelligent handling of rate limits (429 errors).
* **[Skills]: Skill Bank (Phase 1).** Implementation of the new Skill Bank system for managing and resolving specialized agent capabilities.
    * **Skill Store:** A new SQLite-backed storage layer for skills.
    * **CLI Management:** New commands for managing the skill library.
    * **Integration:** Skills are now integrated into the agent provisioning workflow.
* **[Messaging]: Agent Wake-on-Message.** The new `--wake` flag for the `scion message` command allows users to automatically start a stopped agent when sending it a message. The Hub now handles the intermediate "starting" phase and waits for the agent to be ready before delivering the payload.
* **[Messaging]: Composite Recipients.** Introduced the `set[]` composite recipient type, allowing a single message to be fanned out to multiple target agents or groups simultaneously.
* **[Core]: FanOut Broker.** A new native multi-broker fan-out capability in the Hub, enabling messages to be routed across multiple communication channels (e.g., Slack and Telegram) at once.
* **[Web UI]: Enhanced Visibility.** Added relative timestamps to agent activity badges and improved shared directory management with "Download as ZIP" support.

## 🐛 Fixes
* **[Migrations]: Database Resilience.** Hardened migrations V50, V51, and V53 to be more resilient to missing tables and to handle term-mapping more reliably during the Grove-to-Project transition.
* **[Installation]: Path & Privilege Management.** The installation script now defaults to `/usr/local/bin`, provides warnings if the destination is not in the user's `PATH`, and handles directory creation more robustly across different Linux distributions.
* **[CLI]: Command Routing.** Fixed an issue where the `help` command would sometimes be incorrectly routed when passed with other arguments.
* **[Storage]: Gitignore Validation.** Moved the `.scion/agents/` gitignore check to pre-flight validation to prevent configuration errors later in the agent lifecycle.
