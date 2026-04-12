# Release Notes (2026-04-09)

This update focuses on enhancing agent stability, improving the harness configuration experience, and expanding authentication capabilities with Kubernetes in-cluster support and multi-provider Hub login.

## 🚀 Features
* **Harness Configuration:** Harness configurations are now dynamically loaded from disk, and a new "Other" option has been added to the UI for custom setups.
* **Kubernetes:** Added support for in-cluster Kubernetes authentication, allowing for easier deployment within Kubernetes environments.
* **Environment Toolset:** Integrated `EnvironmentToolset` and updated `google-adk` to version 1.28.0 or higher for improved environment management.
* **Hub & Authentication:** Hub login now supports explicit provider selection, and agent token expiry information is now visible in the hub status.

## 🐛 Fixes
* **ADK Agent Reliability:** Improved ADK agent stability with the addition of crash reporting, startup logging, and direct log routing to container logs via `/proc/1/fd/2`.
* **Agent Creation UI:** Resolved issues where groves were missing from the agent creation form and scheduler admin due to pagination limits.
* **Agent Lifecycle:** Fixed a bug where workspace worktrees were not correctly recreated when restarting existing agents.
* **System Stability:** Improved overall reliability by synchronizing the `HubConnection` control channel lifecycle and ensuring daemon exit errors are surfaced during component startup.
* **UI Tweaks:** Removed the redundant grove subtitle from the user message card header for a cleaner interface.
