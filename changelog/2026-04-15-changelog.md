# Release Notes (2026-04-15)

This update introduces significant enhancements to the Claude harness authentication, improves server maintenance workflows, and resolves several critical bugs across the hub, web interface, and deployment scripts.

## 🚀 Features
* **[Claude Harness] Enhanced Authentication Methods:** Added support for OAuth token (`oauth-token`) and native credentials file (`auth-file`) authentication. This enables users of Claude Code (Pro, Max, Team, and Enterprise) to use their subscription-based credentials directly within SCION agents.
* **[Server Maintenance] Integrated Update Mechanism:** Introduced a "Check for Updates" feature in the web dashboard, complemented by an "Update Now" action. This allows operators to trigger server rebuilds and restarts directly from the UI.
* **[Web UI] Advanced Grove Filtering:** Evolved the grove filter into a three-way scope selector, providing more granular control over resource visibility in the dashboard.

## 🐛 Fixes
* **[Server Rebuild] Reliable Restart Logic:** Replaced the Polkit-based authorization with a more robust sudoers implementation to ensure compatibility across all Ubuntu LTS versions. Added fire-and-forget restart execution and automated cleanup of stalled maintenance operations on startup.
* **[Harness] Claude Workspace Paths:** Corrected the project path resolution for Claude agents using git-clone to consistently use the `/workspace` directory.
* **[Web UI] Admin & Dashboard Polish:** Restored missing pagination on the Admin Users page and resolved a DOM flickering issue during maintenance polling. Ensured that server build time and commit metadata are consistently displayed.
* **[Runtime] Container ID Resolution:** Fixed issues where container IDs were not correctly resolved before stopping agents on Apple runtimes or during `Exec` operations.
* **[Deployment] Script Improvements:** Optimized `gce-start-hub.sh` by removing redundant SSH calls, fixing shell block escaping, and ensuring correct ownership of sudoers rules.
* **[Hub] Grove Access Control:** Fixed a bug in the shared grove filter to correctly account for transitive group memberships.
* **[Broker] Log Resolution:** Removed an erroneous `.scion` suffix that was preventing correct log path resolution in the broker.
* **[CLI] Kubernetes Sanitization:** Added sanitization for usernames in the Kubernetes `attach` command to prevent execution errors.
