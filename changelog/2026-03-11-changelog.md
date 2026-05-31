# Release Notes (Mar 11, 2026)

This release focuses on improving agent lifecycle flexibility and enhancing the web-based terminal experience. It introduces support for targeting specific git branches during agent creation and provides better visibility into template versions, alongside critical fixes for runtime stability and authentication.

## 🚀 Features
* **Custom Branch Targeting:** Added a branch name field to the agent creation flow and enabled cloning of agent branches from origin. This allows users to direct agents to specific branches immediately upon creation, improving workflow flexibility (consolidated from commits 182c323, 2d50def, 11c36a8).
* **Web Terminal & Tmux Interactivity:** Introduced a tmux mouse toggle (via `C-b m`) and a toolbar button in the web terminal. This release also resolves persistent copy-paste issues in the web interface and adds comprehensive documentation for terminal options (consolidated from commits 9a41138, 9371859, 616250a).
* **Enhanced Template Traceability:** Updated the CLI and Web UI to display template IDs and hashes, providing clear visibility into the exact configuration version associated with each agent.

## 🐛 Fixes
* **Runtime & Broker Stability:**
    * **Podman Reliability:** Resolved an issue where Podman containers would fail to restart correctly from the Hub or Broker.
    * **Double-Daemonization:** Prevented the broker from double-daemonizing during start or restart operations.
* **Agent Attachment Reliability:** Added a readiness check for tmux sessions before attachment, ensuring more reliable connections when attaching to running agents.
* **Authentication & Secret Injection:** Corrected a bug where environment-type secrets were not properly injected into the execution environment during authentication resolution.
* **Grove & Workspace Management:**
    * **Multi-Hub Compatibility:** Fixed a regression where git-based groves were incorrectly rejected in multi-hub environments.
    * **Cleanup & Resolution:** Improved hub-managed grove path resolution during agent deletion and enhanced detection of orphaned grove configurations.
* **Configuration & Compatibility:**
    * **Legacy Key Support:** Updated `config get` to support legacy v1 settings keys like `image_registry`.
    * **Fallback Logic:** Improved `env-gather` and harness configuration to correctly fall back to global settings when local context is missing.
* **Documentation & Polish:** Performed final pre-launch polish on philosophical documentation and refined the agent creation UX by defaulting runtime profiles to "Use broker default."
