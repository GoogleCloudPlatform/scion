# Release Notes (2026-04-14)

This release focuses on critical infrastructure stability, Kubernetes environment fixes, and API behavior refinements for agent management.

## 🚀 Features
* **Improved Terminal Redrawing:** Added automatic `tmux` window-size adjustment upon attachment, ensuring the session correctly adapts to the current terminal dimensions.
* **Performance Optimization:** Enhanced `ForceRuntime` resolution performance by utilizing the `auxiliaryRuntimes` cache.

## 🐛 Fixes
* **Server Maintenance & Rebuilds:** Resolved `ETXTBSY` errors when rebuilding running servers. The process now uses a staging path and `sudo` installation to ensure reliability during server updates.
* **Kubernetes (GKE) Stability:** Resolved issues in Kubernetes environments where attach pod names were incorrectly resolved and unexpected `su` password prompts interrupted attachment.
* **Agent Management Refinements:**
    * Re-creating an agent now correctly deletes the stopped instance instead of attempting an in-place restart.
    * Corrected the Hub API response code to `200 OK` (from `201 Created`) when a request results in restarting an existing agent.
* **Workspace & Sync Reliability:** 
    * Added validation to prevent workspace operations on Git-backed groves that have no providers.
    * Fixed synchronization issues in linked groves by removing redundant broker ID filters.
* **Environment Protection:** Hardened worktree protection logic to prevent nested worktree creation in CI and containerized environments.
* **Broker Reliability:** Improved log path and container ID resolution in the broker's log handlers.
* **Service Stability:** Fixed a race condition in the service monitor by ensuring context cancellation is respected after backoff periods.
