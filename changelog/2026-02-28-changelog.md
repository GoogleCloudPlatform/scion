# Release Notes (Feb 28, 2026)

This release marks a major milestone with the completion of the canonical agent state refactor and the launch of the Hub scheduler system, alongside significant enhancements to real-time observability and broker security.

## ⚠️ BREAKING CHANGES
* **Unified State Model:** The legacy `Status` and `SessionStatus` fields have been fully retired in favor of a canonical, layered agent state model. Downstream consumers of the Hub API or `sciontool` status outputs must update to the new schema.
* **Notification Triggers:** In alignment with the state refactor, notification `TriggerStatuses` have been renamed to `TriggerActivities`.

## 🚀 Features
* **Canonical Agent State Refactor:** Completed a comprehensive, multi-phase overhaul of the agent state system across the Hub, Store, Runtime Broker, CLI, and Web UI. This ensures a consistent, high-fidelity representation of agent activity throughout the entire lifecycle.
* **Hub Scheduler & Timer Infrastructure:** Launched a unified scheduling system for recurring Hub tasks and one-shot timers. This includes automated heartbeat timeout detection for "zombie" agents and a new CLI/API for managing scheduled maintenance and lifecycle events.
* **Real-time Debug Observability:** Introduced a full-height debug panel in the Web UI, providing a real-time stream of SSE events and internal state transitions for advanced troubleshooting and observability.
* **Enhanced Web UI Feedback:** Added emoji-based status badges to agent cards and list views, providing more intuitive visual indicators of agent health and activity.
* **Broker Authorization & Identity:** Strengthened security by enforcing dispatch authorization checks and resolving creator identities for all registered runtime brokers.
* **Automated Grove Cleanup:** Hardened the hub-managed grove lifecycle by implementing cascaded directory cleanup on remote brokers whenever a grove is deleted via the Hub.
* **CLI Enhancements:** Added a new `-n/--num-lines` flag to the `scion look` command, enabling tailored views of agent terminal output.

## 🐛 Fixes
* **Notification Dispatcher:** Fixed a bug where the notification dispatcher failed to start when the Hub was running in combined mode with the Web server.
* **Environment Variable Standardization:** Renamed `SCION_SERVER_AUTH_DEV_TOKEN` to `SCION_AUTH_TOKEN` and introduced `SCION_BROKER_ID` and `SCION_TEMPLATE` variables for better debugging and interoperability.
* **Local Secret Storage:** Resolved issues with local secret storage and added diagnostics for environment-gathering resolution.
