# Release Notes (2026-05-09)

This release introduces a major overhaul of the Hub's invitation and security systems, enhances agent crash visibility, and simplifies GitHub and GCP integrations for Grove management.

## ⚠️ BREAKING CHANGES
* **[CLI]: Notification Defaults Changed.** The `--notify` flag now defaults to **on** for the `start` command to provide better feedback on agent launch. Conversely, it is now **opt-in** for the `message` command. Scripts relying on the previous defaults may require adjustment.

## 🚀 Features
* **[Invitation System]: Production-Grade Security & Observability.** A significant update to the user invitation workflow:
    * **Authorization Enforcement:** Added strict checks across OAuth callbacks, invite redemption, and domain-restricted modes to prevent unauthorized access.
    * **Audit Logging:** Implemented comprehensive audit logging for all allow-list and invitation operations.
    * **Real-time Updates:** Integrated Server-Sent Events (SSE) to provide live UI updates for administrative changes.
* **[Core]: Agent Crash Detection.** Agents now automatically detect container crashes and transition to an explicit error state, improving observability into runtime failures.
* **[GitHub Integration]: Streamlined Grove Setup.**
    * Added a dedicated GitHub token field to the Grove creation form.
    * Integrated the `gh` CLI wrapper within agent containers, with automatic management of `GITHUB_TOKEN` environment variable conflicts.
* **[GCP Integration]: Auto-Project Inference.** Groves now automatically infer the GCP project ID from the provided service account email, reducing manual configuration steps.
* **[CLI]: Non-Interactive Agent Mode.** Agents running in CLI mode now automatically enable non-interactive mode for smoother execution in automated environments.
* **[Maintenance]: Hub Maintenance Operations.** Added a new `rebuild-container-binaries` operation to the hub for easier environment lifecycle management.

## 🐛 Fixes
* **[Invitation System]: Security & Logic Hardening.**
    * Fixed a TOCTOU race condition in invite redemption using atomic database increments.
    * Implemented enumeration defense by standardizing error responses for invalid invite codes.
    * Resolved an issue where unlimited invites (`maxUses=0`) were incorrectly treated as single-use.
* **[Runtime]: Shell Escaping & Environment Ordering.**
    * Improved handling of shell-embedded prompts using POSIX single-quote escaping.
    * Corrected the ordering of the `SCION_START_CMD` environment variable in agent execution arguments.
* **[Web UI]: Feature Gating & Routing.**
    * The suspend button is now correctly gated based on the harness's resume capability across all views.
    * Fixed API endpoint mismatches on the invite landing page.
* **[Infrastructure]: Database Migrations.** Renumbered migrations to resolve rebase conflicts and ensured email columns use `COLLATE NOCASE` for reliable case-insensitive matching.
