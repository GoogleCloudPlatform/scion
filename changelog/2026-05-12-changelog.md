# Release Notes (2026-05-12)

This update introduces comprehensive SDKs for Python and TypeScript, significant progress on the Skill Bank system, and a new agent waking mechanism. It also includes various stability fixes and refinements to the resource scoping model.

## 🚀 Features

* **Python & TypeScript SDKs:** Launched foundational SDKs for both Python and TypeScript. These SDKs provide both synchronous and asynchronous clients for managing Agents, Projects, Messages, and Secrets resources. They also include built-in support for Server-Sent Events (SSE) for real-time agent events and cloud logs.
* **Skill Bank Phase 1:** Implemented the core infrastructure for the Skill Bank system. This includes a SQLite-backed storage engine, CLI commands for skill management, and integration with the Hub API and container-script harnesses for automated skill resolution.
* **Agent Wake Flag (`--wake`):** Added a new `--wake` flag to the message command. This feature automatically triggers agent wake logic when sending messages and includes a new `waitForAgentReady` polling mechanism to ensure agents are ready to receive communication.
* **Multi-target Messaging:** Introduced support for `set[]` composite recipients, allowing messages to be sent to multiple targets simultaneously.
* **Shared Directory ZIP Downloads:** Users can now download shared directories as ZIP archives, improving file portability and sharing.
* **UI Enhancements:** Added relative timestamps to the activity badge in the agent detail view for better visibility into recent agent actions.

## 🐛 Fixes

* **Resource Scope Alignment:** Standardized the use of the "project" scope across the CLI and backend, resolving inconsistencies where "grove" was still being used for template resolution and storage paths.
* **V50 Migration Refinement:** Updated the V50 migration script to ensure templates and harness configurations are correctly migrated to the new scoping model.
* **Cross-platform Stability:** Fixed an issue with gitignore path resolution on non-Linux platforms by using `filepath.ToSlash`.
* **CLI Routing:** Refined the help command detection to prevent accidental triggers when "help" is used as a sub-argument rather than the primary command.
* **Token Optimization:** Optimized agent context usage by stripping the unnecessary `version` field from the message delivery payload.
* **Validation Improvements:** Moved `.scion/agents/` gitignore validation to pre-flight checks to provide earlier feedback on potential configuration issues.
