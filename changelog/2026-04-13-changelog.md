# Release Notes (2026-04-13)

This update focuses on strengthening security through stricter authorization checks, enhancing Kubernetes template flexibility, and improving the messaging infrastructure for AI assistant integration.

## 🚀 Features
* **[Kubernetes]:** Enhanced template configuration with support for merging Kubernetes-specific fields, including resource specifications and node selector overrides.
* **[Hooks]:** Integrated Claude assistant replies directly into the hub message store, improving visibility into automated workflows.
* **[Web]:** Browser page titles now dynamically update to reflect the navigation context, improving tab management.
* **[Auth]:** Added GitHub as a fallback authentication provider for device flow scenarios.

## 🐛 Fixes
* **[Security]:** Enforced stricter authorization for GCP Service Account management and group membership; administrative actions now require grove-owner (`ActionManage`) permissions.
* **[Hub]:** Improved system resilience by adding polkit rules for self-restarting the scion service and implementing panic recovery for maintenance operations.
* **[Look]:** Removed fragile tmux socket discovery logic in the `look` command for more reliable agent inspection.
* **[Web]:** The Messages tab is now correctly decoupled from the cloud-logging gate, ensuring availability regardless of logging configuration.
* **[Stalled Detection]:** Refined the stalled agent detection logic to correctly ignore agents that are `idle` or `waiting_for_input`.
* **[Configuration]:** Resolved issues with harness configuration synchronization against local Hub storage and fixed missing IAM permissions for GCP SA management.
