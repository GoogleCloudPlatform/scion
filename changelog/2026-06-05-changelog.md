# Release Notes (2026-06-05)

This release introduces significant architectural improvements focused on scalability and infrastructure flexibility. Key highlights include the addition of a Postgres storage backend, multi-node broker dispatch, and NFS-coordinated workspace sharing.

## ⚠️ BREAKING CHANGES
* **Workspace Pathing:** The workspace file browser now resolves to `groves/` instead of `projects/`. This aligns with internal resource naming conventions but may affect external scripts or bookmarks.
* **Database Migration:** A new in-process migration from legacy raw-SQL `hub.db` to the Ent-backed schema is now active. While automatic, operators are encouraged to backup their database before upgrading.

## 🚀 Features
* **Postgres Storage Backend:** Full support for Postgres using the `pgx` driver, featuring Ent schema parity and real-time event distribution via `LISTEN/NOTIFY`.
* **Multi-Node Broker Dispatch:** Introduced a robust dispatch system for multi-replica environments, including affinity-based routing and durable intent tracking.
* **NFS-Coordinated Workspaces:** Enabled workspace sharing across nodes using NFS, supporting both Docker and GKE/Cloud Run runtime environments.
* **Google IAP Auth Proxy:** Added support for Google Identity-Aware Proxy (IAP) as an authentication proxy.
* **Resource Hardening:** Implemented resource cloning and deletion with hardened authorization checks.

## 🐛 Fixes
* **Multi-Node Stability:** Resolved session management issues and improved Cloud Run deployment stability.
* **UI/UX Refinements:** Fixed task overflow in the agent list and unified action buttons for a consistent experience.
* **Broker Reliability:** Prevented stale disconnect events from incorrectly marking reconnected brokers as offline.
* **Lifecycle Reliability:** Guarded hub agent phase transitions against spurious session lifecycle events.
* **Token Protection:** Prevented `sciontool` tests from accidentally clobbering live agent tokens.
