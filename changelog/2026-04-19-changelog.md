# Release Notes (2026-04-19)

This release focuses on improving the Web UI for template and agent details, enhancing template reconciliation in the Hub, and ensuring robust logging and link rendering.

## 🚀 Features
* **Web UI: Improved Template Hash Display:** Introduced a new component that truncates long SHA256 hashes with an ellipsis. This improves layout consistency on the agent and template detail pages while providing a one-click copy-to-clipboard action.

## 🐛 Fixes
* **Web UI: Real-time Agent Action Buttons:** Fixed an issue where action buttons (Start, Stop, Delete, Terminal) were missing for agents added via SSE until the page was reloaded. Action capabilities are now correctly derived and preserved during real-time updates.
* **Hub: Template Reconciliation:** Improved the reliability of template re-imports from remote URLs. The process now forces a re-upload of all files and correctly prunes deleted files from storage, preventing agents from starting with stale template data.
* **Web UI: Automatic Link Rendering:** Enhanced the git remote display to automatically render bare `github.com` repository paths as clickable links.
* **Daemon: Log Directory Creation:** Ensured that the log directory is created before the daemon attempts to open the log file, preventing potential startup failures.
