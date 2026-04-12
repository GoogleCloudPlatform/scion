# Release Notes (2026-04-06)

This release introduces a major overhaul of the Google Chat integration, shifting to the Workspace Add-on architecture, alongside significant improvements to the Message Broker plugin system and GCP service account management.

## ⚠️ BREAKING CHANGES
* **Google Chat App:** The Chat App has been migrated to the Google Workspace Add-on architecture. This requires configuration updates: the `Audience` field has been removed, while `ExternalURL`, `ServiceAccountEmail`, and `CommandIDMap` are now required. Webhook-based deployments must be updated to use the HTTP Service-based Add-on model.

## 🚀 Features
* **Google Chat Workspace Add-on:** A complete rewrite of the Google Chat adapter, supporting synchronous responses, host app data actions, and multi-turn event handling to prevent duplicate messages. Includes automation for Hub VM installation and new documentation for remote setup.
* **Message Broker Plugin Evolution:** Enhanced the plugin system with `HostCallbacks` for plugin-initiated subscriptions and notification routing through the broker. Added a self-managed lifecycle mode for standalone plugin binaries.
* **GCP Service Account Integration:** Groves now support default GCP identities that are auto-verified upon registration and automatically applied in the agent creation form.
* **Agent Progeny Secret Access:** Implemented granular secret access controls for progeny agents, improving security for multi-agent workflows.
* **Terminal UX Improvements:** Added detection for active tmux windows upon terminal connection, ensuring the toolbar stays synchronized with the current session.

## 🐛 Fixes
* **Credential Management:** Fixed issues where the credential helper would return expired GitHub App tokens or fail to write expiry timestamps during refreshes.
* **Identity Persistence:** Resolved an issue where GCP service account associations were lost during agent stop/start cycles and updated the metadata server to use dynamic tokens.
* **System Stability:** Added missing default values (version, timestamp, type) for structured messages and resolved compilation errors related to the `no_sqlite` build tag.
* **CI/CD & Maintenance:** Improved CI detection of formatting issues and removed stale sync-templates tests.
* **Agent Scopes:** Added `ScopeAgentNotify` to the default agent token scopes.
