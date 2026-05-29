# Release Notes (2026-05-10 - 2026-05-18)

This period is dominated by a foundational architectural shift, renaming the primary organizational unit from **"Groves"** to **"Projects"** across the entire ecosystem. Alongside this migration, we've expanded communication capabilities with Telegram support and enhanced CLI messaging tools.

## ⚠️ BREAKING CHANGES
* **[Core]: Architectural Rename (Grove → Project).** A comprehensive migration has renamed "Groves" to "Projects" throughout the codebase, API, database, and CLI. 
    * **CLI:** Commands like `scion grove` are now `scion project` (though `grove` remains as a hidden alias for backward compatibility).
    * **API:** Primary endpoints have moved from `/api/v1/groves` to `/api/v1/projects`. Legacy paths are supported with deprecation warnings.
    * **Environment Variables:** New variables like `SCION_PROJECT_ID` have been introduced. Legacy `SCION_GROVE_ID` is still exported for compatibility during this transition.
    * **Database:** Migrations (V48-V53) are required to update schema references.
* **[Database]: Mandatory Migrations.** Migrations V50 and V53 are required for Hub operation. These migrations handle the project rename and unify allow-list management.

## 🚀 Features
* **[Chat]: Telegram Integration.** Added official support for Telegram chat, expanding the platform's multi-channel communication capabilities.
* **[Messaging]: Enhanced Targeting and Wake Control.**
    * Introduced `set[]` composite recipients, allowing messages to be sent to multiple targets simultaneously.
    * Added the `--wake` flag to the `scion message` command, providing more control over agent engagement.
* **[Web UI]: ZIP Downloads for Shared Directories.** Users can now download the entire contents of a shared directory as a single ZIP archive.
* **[Harness]: MappingDialect and Type Passthrough.** Enhanced harness flexibility by introducing `MappingDialect`, allowing for more complex data transformations and direct type passthrough.
* **[UX]: Relative Activity Timestamps.** The agent activity badge now displays relative timestamps (e.g., "3 minutes ago"), providing better real-time context for agent operations.

## 🐛 Fixes
* **[Core]: State Reporting Clarity.** Renamed the internal `ActivityIdle` state to `ActivityWorking` to more accurately reflect agent status during active task processing.
* **[Security]: Path Traversal Protection.** Restored and hardened path traversal protections in workspace resolution logic.
* **[Infrastructure]: Migration Resilience.** Improved the robustness of database migrations, ensuring they are idempotent and resilient to missing tables (e.g., `allow_list`).
* **[Installation]: Improved PATH Validation.** The installation script now defaults to `/usr/local/bin` and provides explicit warnings if the installation directory is not in the user's `PATH`.
* **[CLI]: Command Matching & Scope Alignment.**
    * Fixed a bug where the `help` command was incorrectly matched in multi-argument scenarios.
    * Aligned secret scoping and message listing query parameters with the new "project" terminology.
