# Release Notes (2026-04-15)

This release focuses on enhancing the Claude harness with new authentication options, improving web-based server maintenance workflows, and resolving several critical runtime and UI stability issues.

## 🚀 Features
* **In-App Server Rebuilds:** Introduced a "Check for Updates" feature in the web UI, allowing administrators to trigger an immediate server rebuild and restart directly from the configuration page.
* **Claude Harness Authentication:** Added support for long-lived OAuth tokens (`oauth-token`) and rotating credentials-file (`auth-file`) authentication methods. This enables better integration with Claude Pro/Max subscription features and full-scope sessions.
* **Enhanced Grove Navigation:** Evolved the grove filter into a three-way scope selector (Personal, Shared, All) for more intuitive workspace management in the web interface.

## 🐛 Fixes
* **Container ID Resolution:** Resolved issues where container IDs were not correctly identified before `stop` or `exec` operations in Apple and Docker runtimes.
* **Access Control:** Improved shared grove filtering to correctly include transitive group memberships, ensuring consistent visibility for team-based resources.
* **Web Admin & UI:** Restored functional pagination on the admin users page and eliminated DOM flickers during maintenance operation polling.
* **Deployment Stability:** Replaced `polkit` with a more robust `sudoers` configuration for server restarts and streamlined SSH operations during deployment.
* **Harness & Broker:** Fixed project pathing for git-cloned agents in the Claude harness and resolved log path resolution issues in the runtime broker.
* **Maintenance Reliability:** The server now automatically aborts stalled maintenance operations on startup to prevent inconsistent states.
