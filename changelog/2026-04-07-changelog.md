# Release Notes (2026-04-07)

This update focuses on deepening the integration between the Scion Hub and the Google Chat application, improving signing key resilience via GCP Secret Manager, and enabling direct agent-to-user messaging.

## ⚠️ BREAKING CHANGES
* **Secret Manager Enforcement:** When GCP Secret Manager is configured for signing keys, the Hub will now fail to start if it cannot successfully synchronize or verify the key against the backend. This ensures cryptographic consistency across services and prevents the silent generation of mismatched ephemeral keys.
* **Hub Initialization:** The programmatic `hub.New()` function signature has changed to return an error, requiring updates to any custom integrations or test harnesses.

## 🚀 Features
* **Direct User Messaging:** The `scion message` CLI and internal agent services now support `user:<name>` recipients, allowing agents to send messages directly to a user's chat inbox.
* **Streamlined Chat Onboarding:** The Google Chat application now automatically registers users by matching their Google-asserted email address to their Hub account identity, bypassing the manual OAuth device flow when identities align.
* **Chat Info Command:** Added a new `/scion info` slash command to the chat application for a quick overview of the connected environment and Hub status.
* **Startup Connectivity Checks:** The chat application now performs a comprehensive connectivity check to the Hub at startup to provide immediate feedback on configuration or network issues.

## 🐛 Fixes
* **Agent-to-User Routing:** Resolved a critical bug where direct agent messages were published to SSE but failed to dispatch through broker plugins to the chat application.
* **Signing Key Resilience:** 
    * Implemented automatic recovery of signing keys from GCP Secret Manager in cases where the local SQLite metadata is missing (e.g., after a database reset).
    * Resolved key mismatches during legacy migrations by improving auto-discovery logic to prefer modern internal key formats.
* **Chat UI/UX Stability:** 
    * Converted all slash commands to return synchronous responses, eliminating "Not Responding" error toasts in the Google Chat interface.
    * Improved user mentions in message cards and resolved a crash loop caused by `go-plugin` magic cookie verification.
    * Fixed handling of scientific notation in timezone offsets and floating-point annotation indices returned by the Google Chat API.
* **Plugin Reliability:**
    * Implemented auto-reconnection logic for self-managed broker plugins to recover from transient RPC failures.
    * Fixed `install.sh` logic to correctly place plugin configurations under the server scope and remove stale root-level plugins.
* **Identity Resolution:** Improved matching of chat identities to Hub accounts in message sender and recipient fields for better auditability and UI display.
