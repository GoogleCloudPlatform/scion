# Release Notes (2026-04-17)

This release introduces native Discord notification support, real-time message streaming for individual agents, and a significant overhaul of grove-level access controls to ensure owners and admins have consistent privileges.

## 🚀 Features
* **Native Discord Notification Channel:** The Hub now supports Discord webhooks as a native notification channel. Features include severity-based color coding (blue for state changes, yellow for input needed, red for urgent), support for user and role mentions on urgent messages, and automatic safe truncation for long message descriptions.
* **Per-Agent Real-time Message Stream:** Added a store-backed SSE stream for the per-agent Messages tab. This enables real-time chat updates on all deployments, including those not configured with Cloud Logging, resolving a long-standing gap where the "Stream" toggle was non-functional on many hubs.

## 🐛 Fixes
* **Grove Owner & Admin Permissions Bypass:** Corrected a permission gap where grove owners and admins (who were not the original creator) lacked full access to manage grove settings and other members' agents. Owners and admins now correctly inherit full bypass privileges across the grove and its scoped resources.
* **Dashboard Count Updates:** Fixed an issue where dashboard counts would not reliably update upon initial load, ensuring accurate visibility of system state.
* **Provisioning Automation:** Updated starter-hub provisioning scripts to automatically enable the Google Cloud Secret Manager API, reducing manual setup steps for new deployments.
* **System Stability & Performance:** Resolved several race conditions in the web frontend, eliminated redundant API fetches, and tightened TypeScript definitions across the hub and web components.
