# Release Notes (2026-04-10)

This release focuses on significant architectural improvements to agent isolation, path resolution consistency, and Kubernetes deployment hardening.

## ⚠️ BREAKING CHANGES
* **Shared Directory Path Resolution:** Aligned hub-native and agent provisioning path resolution for shared directories. Shared directories are now resolved via the `.scion` marker in `~/.scion/grove-configs/` instead of the legacy `~/.scion/groves/` directory. Existing shared directory contents may need to be migrated to the new location to be accessible in the web UI.
* **Agent Scoping & Broker API:** Agent identities and container names are now strictly scoped by their Grove ID to prevent cross-grove collisions. The `RuntimeBrokerClient.StartAgent` interface now requires a `groveID` parameter.

## 🚀 Features
* **Kubernetes Agent Management:** Introduced state reconciliation for terminal Kubernetes agents to ensure pod lifecycle is correctly tracked.
* **Non-Root Agent Support:** Initial preparations for running agent harnesses as non-root users, including pre-creation of home directories and hardened `PATH` configurations.
* **Broker Heartbeats:** Enabled heartbeat monitoring for colocated brokers to improve connection reliability and failure detection.
* **UI Enhancements:** Improved GitHub URL link styling with hover underlines and implemented server-side sorting for user lists to improve pagination performance.

## 🐛 Fixes
* **Agent Isolation:** Scoped container names with a grove prefix (`grove--agent`) to prevent Docker/Podman name collisions across different groves.
* **GitHub Integration:**
    * Fixed GitHub App token usage during template imports on app-configured groves.
    * Improved GitHub template URL normalization to correctly handle `/tree/main` paths.
* **Broker Stability:**
    * Defaulted standalone brokers to loopback in production mode for improved security.
    * Allowed brokers to start without HMAC keys to facilitate pending registrations.
* **Security:** Hardened Kubernetes agent pod security specifications.
* **Provisioning:** Prevented unnecessary worktree recreation from interfering with new agent provisioning flows.
* **Compatibility:** Resolved issues with empty `PLATFORM_ARGS` arrays in bash scripts when `set -u` is enabled.
* **Process Management:** Updated tmux session checks to utilize container IDs, ensuring compatibility with new grove-scoped naming conventions.
