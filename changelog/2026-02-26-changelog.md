# Release Notes (Feb 26, 2026)

This release introduces a robust capability-based access control system, a dedicated administrative management suite, and critical session management upgrades to support larger authentication payloads.

## 🚀 Features
* **Capability-Based Access Control:** Implemented a comprehensive capability gating system across the Hub API and Web UI. Resource responses now include `_capabilities` annotations, enabling granular UI controls and API-level enforcement for resource operations.
* **Administrative Management Suite:** Launched a new Admin section in the Web UI, providing centralized views for managing users, groups, and brokers. Includes a maintenance mode toggle for Hub and Web servers to facilitate safe infrastructure updates.
* **Advanced Environment & Secret Management:** Introduced a profile-based settings section for managing user-scoped environment variables and secrets. Secrets are now automatically promoted to the configured backend (e.g., GCP Secret Manager) with standardized metadata labels.
* **SSR Data Prefetching:** Improved initial page load performance and eliminated "flash of unauthenticated content" by prefetching critical user and configuration data into the HTML payload via `__SCION_DATA__`.
* **Hub Scheduler Design:** Completed the technical specification for a new Hub scheduler and timer system to manage long-running background tasks and lifecycle events.
* **Enhanced Real-time Monitoring:** Expanded Server-Sent Events (SSE) support to the Brokers list view, ensuring infrastructure status is reflected in real-time without manual refreshes.

## 🐛 Fixes
* **Filesystem Session Store:** Replaced cookie-based session storage with a filesystem-backed store to resolve "400 Bad Request" errors caused by cookie size limits (4096 bytes) during large JWT/OAuth exchanges.
* **Hub-Managed Grove Reliability:** Fixed critical 503 errors and path resolution issues during agent creation in hub-managed groves by correctly propagating grove slugs to runtime brokers.
* **Agent Deletion Cleanup:** Hardened the agent deletion flow to ensure that stopping and removing an agent in the Hub correctly dispatches cleanup commands to the associated runtime broker and removes local workspace files.
* **Environment Validation:** Improved agent startup safety by treating missing required environment variables as fatal errors (422), preventing agents from starting in incomplete states.
* **Terminal Responsiveness:** Resolved several layout bugs in the web terminal, ensuring it correctly resizes with the viewport and fits within the application shell.
* **Group Persistence:** Fixed synchronization issues between the Hub's primary database and the Ent-backed authorization store, ensuring grove-scoped groups and policies are preserved during recreation.
