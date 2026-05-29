# Release Notes (2026-05-11 to 2026-05-19)

This period marks a significant architectural milestone with the systematic transition from "Groves" to "Projects" and the introduction of a completely overhauled Telegram Broker for advanced agent interaction.

## 🚀 Features
* **Telegram Broker V2:** Introduced a comprehensive rewrite of the Telegram integration.
    * Support for interactive inline keyboards and status cards.
    * Conversation-scoped reply routing and observer mode for agent-to-agent visibility.
    * Native user registration flow and hub-verified device authentication.
    * Per-chat rate-limiting send queues and webhook server support.
* **Grove to Project Unification:** Performed a codebase-wide rename of "Groves" to "Projects" to align with standard industry terminology. This includes dual-field backward compatibility in APIs and configuration to ensure a smooth transition.
* **User Invitation System:** Implemented a new foundation for user access management, including invite codes and unified allow-list management.
* **Skill Bank Foundation:** Launched the "Skill Bank" system, enabling late-binding agent capabilities through a centralized store and resolution mechanism.
* **Agent "Wake" Support:** Added the `--wake` flag to the `scion message` command, allowing the system to automatically resume suspended agents before delivering messages.
* **Multi-Target Messaging:** Introduced composite recipient support (`set[]`), enabling a single message to be fanned out to multiple users or agents.
* **SDK Enhancements:** Significant updates to both Python and TypeScript SDKs, adding full resource coverage (Agents, Messages, Projects, Secrets) and SSE streaming support for real-time events.
* **Agent Identity:** Added the `scion whoami` command to allow agents to easily discover their own identity and scope.
* **Shared Directory ZIP Downloads:** Users can now download shared workspace directories as ZIP archives directly from the web UI.

## 🐛 Fixes
* **Telegram Integration:** Resolved numerous issues related to message routing, attachment path resolution, and HTML status card rendering.
* **Hub Security & Stability:** Hardened the Hub against path traversal attacks, ensured atomic token file writes, and resolved SQLite connection bottlenecks in the web API.
* **Rename Transition:** Fixed multiple regressions introduced during the Project rename, specifically around JSON shadowing and database migration paths.
* **CLI Usability:** Improved pre-flight checks for `.scion/agents/` gitignore rules and refined the `help` command routing.
* **Message Delivery:** Improved reliability of message delivery reporting, including better error propagation for composite recipients.
