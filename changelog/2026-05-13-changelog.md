# Release Notes (2026-05-13)

This release focuses on streamlining the administrative user experience and completing the major terminology transition from "Grove" to "Project" across the system.

## 🚀 Features
* **[Admin UI]: Unified Member Management.** Merged the separate "Allow List" and "Invites" tabs into a single, cohesive **Members** view. 
    * Manage all users and access controls from a single interface.
    * Inline invite status tracking (active, expired, revoked, exhausted).
    * Integrated invite generation and revocation directly within the member list.
* **[CLI]: Agent Wake-on-Message.** Introduced the `--wake` flag for the `scion message` command, allowing users to explicitly wake suspended agents when sending a message from the CLI.

## 🐛 Fixes
* **[Core]: Grove to Project Finalization.** Completed the renaming of "Grove" terminology to "Project" across `hubclient`, `projectsync`, and `agent-viz`.
    * Implemented backward compatibility for `groveId` and legacy `groves` keys in JSON payloads to ensure uninterrupted service for older clients and integrations.
    * Updated internal API paths and TypeScript types for consistency across the stack.
    * Resolved CI failures resulting from the terminology and path changes.
* **[Observability]: Clarified Activity States.** Renamed the `ActivityIdle` state to `ActivityWorking`. This provides more accurate feedback in the agent detail view, ensuring that an agent performing background work is no longer incorrectly labeled as "Idle".
* **[Installation]: Enhanced Setup UX.** The installation script now defaults to `/usr/local/bin` and includes a validation check to warn users if the installation directory is not in their system `PATH`.
* **[Database]: Migration Resilience.** Hardened database migration V53 to ensure it remains resilient if the `allow_list` table is not yet present in the schema during execution.
