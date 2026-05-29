# Release Notes (2026-05-10)

This release introduces a comprehensive user invitation system, adds agent lifecycle controls for suspending and resuming agents, and enhances the Scion CLI with better notification defaults and new debugging tools.

## 🚀 Features
* **[hub] User Invitation System:** Implemented a multi-phase invitation system including:
    * **Allow List & Access Modes:** New `user_access_mode` configuration (Open, Domain Restricted, Invite Only) with a persistent allow list.
    * **Invite Codes:** Support for generating time-limited, multi-use invite codes with shareable landing page links.
    * **Bulk Management:** Added CSV and JSON import support for allow list entries via CLI and Web UI.
    * **Observability:** Integrated structured audit logging for all invite-related operations and real-time SSE updates for the admin dashboard.
    * **Admin UI:** New management interfaces for allow lists and invites, including a dashboard statistics widget.
* **[lifecycle] Agent Suspend and Resume:** Introduced the ability to suspend and resume agents. The `scion start` command now automatically resumes suspended agents, and the Web UI includes new controls gated by harness capabilities.
* **[cmd] CLI UX Improvements:**
    * The `message` command now defaults to enabling notifications (`--notify`).
    * Added back-compatibility for the `--notify` flag on the `start` command.
    * Improved non-interactive mode auto-detection for agent CLI sessions.
* **[extras] Message Debugging:** Added `scion-broker-log`, a new broker plugin for debugging message traffic, supporting both tee/proxy and forward modes.
* **[runtime] Enhanced Container Management:**
    * Improved crash detection that automatically transitions agents to an error state.
    * Added support for the `gh` CLI wrapper within agent containers.
    * Refined shell-embedded prompt escaping and environment variable ordering.
* **[a2a] Scion-a2a Bridge:** Initial implementation of the agent-to-agent bridge for cross-hub communication.
* **[grove] GCP Inference:** Enabled automatic GCP project ID inference from service account emails during grove creation.

## 🐛 Fixes
* **[chat] UI Refinement:** Improved the chat interface by truncating long assistant replies and fixing notification icon consistency.
* **[web] Capability Gating:** Ensured the agent suspend button is only visible when the underlying harness supports the resume capability.
* **[hub] Message Reliability:** Fixed message persistence issues when the broker is active to ensure reliable SSE delivery.
* **[completions] Permissions:** Fixed a permission issue when writing shell completions to `/etc` by properly respecting sudo.
* **[broker] Callback Forwarding:** Resolved an issue where host callbacks were not being correctly forwarded to downstream plugins in `broker-log`.
