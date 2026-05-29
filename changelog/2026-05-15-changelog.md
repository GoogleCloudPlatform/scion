# Release Notes (2026-05-10 - 2026-05-15)

This period is marked by the comprehensive "Grove to Project" rebranding—a massive, system-wide overhaul of core terminology, database schemas, and API protocols. Alongside this migration, the release introduces significant improvements to multi-target messaging, administrative UX unification, and storage management.

## ⚠️ BREAKING CHANGES
* **[Core]: System-wide Rebranding (Grove → Project).** "Groves" have been renamed to "Projects" across the entire Scion ecosystem.
    * **Database Migration:** Database tables and schemas have been updated to reflect Project naming.
    * **API Paths:** Standard API endpoints now use `/api/v1/projects/...`. While legacy `/api/v1/groves/...` paths are supported with deprecation warnings, users should migrate immediately.
    * **Environment Variables:** `SCION_GROVE_ID` is deprecated in favor of `SCION_PROJECT_ID`. Dual-export support is currently active.
    * **CLI Flags:** Most commands now use `--project` or `--project-id`. The `scion grove` command has been renamed to `scion project` (with a hidden alias for backward compatibility).
* **[Agent]: Scoped Stop() Command.** The `Stop()` operation is now strictly scoped to the active project to prevent accidental cross-project agent termination.

## 🚀 Features
* **[Management]: Unified Administrative UX.** Consolidated "Allow List" and "Invite Management" into a single, cohesive management interface, streamlining user onboarding and access control.
* **[Messaging]: Multi-Target & Wake Support.**
    * Added `set[]` composite recipient support, allowing messages to be broadcast to multiple targets simultaneously.
    * Introduced the `--wake` flag to the `scion message` command to ensure dormant agents are active before message delivery.
* **[Storage]: ZIP Archive Downloads.** Shared directories can now be downloaded directly from the web interface as consolidated ZIP archives.
* **[Web UI]: Activity & State Visibility.**
    * Added relative timestamps to agent activity badges for better temporal context.
    * Renamed the `ActivityIdle` state to `ActivityWorking` to more accurately reflect agent status during background processing.
* **[Skills]: Team-Builder Tuning.** Hand-tuned the team-builder skill for improved collaborative performance and role assignment.

## 🐛 Fixes
* **[Infrastructure]: Migration Stability & Idempotency.** Significantly improved the resilience of V50 and V53 migrations. Migrations are now idempotent, preventing hub startup crashes during complex schema updates.
* **[Filesystem]: Content-Aware Path Resolution.** Fixed workspace path resolution logic to include a fallback mechanism that automatically redirects from new `projects/` paths to legacy `groves/` directories where necessary.
* **[Security]: Path Traversal Protection.** Restored and hardened path traversal protections during workspace resolution following the project rename.
* **[CLI]: Installation & Pre-flight Refinements.**
    * Default installation path moved to `/usr/local/bin` with automatic PATH validation warnings.
    * Relocated `.scion/agents/` gitignore checks to pre-flight validation to catch configuration issues earlier.
    * Fixed `help` command matching logic to avoid accidental triggers when used as a sub-argument.
* **[Templates]: Scope Normalization.** Normalized scope resolution in the CLI to ensure templates are correctly mapped to projects regardless of how they are invoked.
