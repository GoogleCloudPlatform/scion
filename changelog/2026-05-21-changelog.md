# Release Notes (2026-05-13 to 2026-05-21)

This period is marked by the stabilization of the new Telegram Broker V2, the finalization of the system-wide "Grove to Project" rebranding, and a critical performance optimization for the CLI to ensure reliability in large-scale environments.

## ⚠️ BREAKING CHANGES
* **[Core]: Architectural Rename (Grove → Project).** The migration of "Groves" to "Projects" is now complete across the API, CLI, and database.
    * **CLI:** Legacy `scion grove` commands are deprecated in favor of `scion project`.
    * **API:** Primary endpoints have transitioned to `/api/v1/projects/...`.
    * **Environment:** `SCION_PROJECT_ID` is now the primary identifier, replacing `SCION_GROVE_ID`.
* **[Database]: Mandatory Migrations.** Migrations V50 through V53 are required to handle terminology updates and unified allow-list management.

## 🚀 Features
* **[CLI]: Performance Optimization & Broker Stability.** Optimized agent-specific CLI commands (start, create, stop, delete, etc.) to skip redundant synchronization checks. This prevents the broker from being saturated with concurrent provisioning requests in large workspaces, eliminating the "no_runtime_broker" errors caused by missed heartbeats.
* **[Chat]: Telegram Broker V2.** A comprehensive rewrite of the Telegram integration, featuring:
    * **Interactive UI:** Support for inline keyboards and rich HTML status cards.
    * **Intelligent Routing:** Conversation-scoped reply routing and observer mode for agent-to-agent visibility.
    * **Secure Registration:** Native user registration flow with hub-verified device authentication.
    * **Reliability:** Per-chat rate-limiting send queues and webhook server support.
* **[Harness]: MappingDialect and Type Passthrough.** Enhanced harness flexibility with the introduction of `MappingDialect`, allowing for complex data transformations and direct type passthrough to underlying models.
* **[Admin]: Unified Invitation & Allow List UX.** Merged the administrative interfaces for user invitations and allow-list management into a single, cohesive user management experience.

## 🐛 Fixes
* **[CLI]: Broker Lockup Prevention.** Resolved a race condition where synchronous agent syncing during CLI commands caused the broker to appear offline under load.
* **[Project Migration]: Rebranding Polish.** Fixed multiple terminology regressions in the hub client, project synchronization logic, and agent-visualizer log parser.
* **[Messaging]: Attachment Path Resolution.** Improved the reliability of file delivery through the `--attach` flag, specifically resolving path translation issues between the workspace and the hub.
* **[Database]: Migration Resilience.** Hardened database migrations (specifically V53) to be resilient against missing tables, ensuring smoother updates on older installations.
* **[State Reporting]: Activity Status Clarity.** Renamed the `ActivityIdle` state to `ActivityWorking` to more accurately reflect agent status during active processing.
