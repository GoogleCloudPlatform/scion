# Release Notes (2026-04-26)

This update introduces a major architectural shift with the initial transition to script-based harnesses, alongside significant enhancements to the Web UI, expanded notification capabilities with native Discord support, and numerous stability improvements across the Hub and Runtime.

## ⚠️ BREAKING CHANGES
* **Script-based Harness Refactor:** Initial transition to a "container-script" harness model where provisioning logic is moved to embedded scripts. While currently opt-in for existing harnesses like Codex and OpenCode, this represents a fundamental change in how harnesses are structured and provisioned.
* **Universal MCP Servers:** Introduced a new universal configuration schema for MCP servers within templates, which may require updates to custom template definitions in the future.

## 🚀 Features
* **Harness & Provisioning:** 
    * **Container-Script Harness:** Added support for Python-based provisioning scripts within harnesses, enabling more flexible and decoupled configuration management.
    * **OAuth & Credentials for Claude:** The Claude harness now supports OAuth tokens and credentials-file based authentication.
    * **Harness CLI:** Added `scion harness-config install` command for easier management of harness configurations.
* **Notifications & Communication:**
    * **Discord Integration:** Added a native Discord notification channel.
    * **Real-time Messages:** Introduced an SSE (Server-Sent Events) stream for per-agent Messages tabs in the Hub, providing live updates.
    * **Discord Adapter Design:** Finalized the design for a dedicated Discord chat adapter.
* **Web UI Enhancements:**
    * **Advanced Filtering:** Added an all/mine/shared scope filter to the agent list view for better organization.
    * **Workspace Improvements:** Added a toggle for dot-file visibility in the workspace file browser and ensured `.scion` directories are accessible.
    * **Update Management:** Added "check-for-updates" and "update-now" functionality for server rebuilds directly from the UI.
    * **Grove Details:** Added a broker column to the agent table in the grove detail view.
* **Infrastructure:**
    * **Image Updates:** Bumped Git to 2.54.0 and added `kubectl` to the core-base image.
    * **Amp Harness:** Added an Amp harness example and corresponding design documentation.

## 🐛 Fixes
* **Hub Stability:**
    * Resolved issues with template reconciliation and harness config defaults during import and restart.
    * Improved access control by granting non-creator grove owners and admins full access.
    * Fixed pagination on the admin users page and restored dashboard count updates on load.
* **Runtime & Broker:**
    * **Shared Workspaces:** Improved state isolation and preservation for agents running in shared-workspace groves.
    * **Compatibility:** Stripped unsupported flags for Apple container CLI and resolved container ID resolution issues in `Exec` calls.
    * **Authentication:** Fixed auth auto-detection precedence, preferring API keys over Vertex AI in environment gathering.
* **General UI/UX:**
    * Improved status badge contrast and fixed theme CSS injection issues for better dark mode support.
    * Resolved DOM flicker during maintenance operations and fixed clickable link rendering for GitHub paths.
    * Ensured action buttons correctly render for agents added via real-time SSE.
