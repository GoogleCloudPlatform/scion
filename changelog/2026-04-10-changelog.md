# Release Notes (2026-04-10)

This release focuses on hardening agent identity scoping, improving Kubernetes agent resilience, and resolving path resolution inconsistencies between the Hub and agents.

## 🚀 Features
* **Kubernetes Agent Resilience:** Added reconciliation for terminal Kubernetes agent states to improve automatic recovery from pod failures.
* **Broker Connectivity:** Enabled heartbeats for colocated brokers to provide better connection monitoring and stability.
* **UI Enhancements:** Improved styling for GitHub URL links in the web interface with hover effects.

## 🐛 Fixes
* **Agent Identity Scoping:** Agents are now strictly scoped by their grove (using `grove--agent` naming) to prevent name collisions and ensure commands are routed to the correct container across different workspaces.
* **Consistent Path Resolution:** Resolved a path mismatch where the Hub and agents were looking for shared directories in different locations by standardizing on marker-based resolution.
* **Provisioning Reliability:** Fixed a race condition in worktree recreation that could interfere with workspace creation for new agents during provisioning.
* **GitHub Integration:**
    * Improved normalization for GitHub template URLs to correctly handle `/tree/main` paths.
    * Fixed template importing for groves using GitHub App authentication by ensuring the App token is correctly utilized.
* **Broker Stability:**
    * Standalone brokers now default to loopback in production mode for improved security.
    * Allowed brokers to start without HMAC keys during the pending registration phase.
* **Runtime Compatibility:**
    * Updated tmux session checks to correctly use container IDs following the introduction of grove-scoped naming.
    * Fixed a bash compatibility issue where empty `PLATFORM_ARGS` would cause errors when `set -u` was enabled.
* **Management UI:** User list sorting is now applied server-side before pagination, ensuring consistent results in the management console.

## ⚙️ Chores & Security
* **Security Hardening:** Hardened Kubernetes agent pod security profiles and prepared home directories for non-root harness execution.
* **Dependencies:** Updated multiple dependencies including Vite, Astro, Go-Jose, and OpenTelemetry exporters.
