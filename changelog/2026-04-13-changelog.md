# Release Notes (2026-04-13)

This release significantly enhances the messaging ecosystem by bridging Claude assistant replies into the Hub message store and resolving critical UI and infrastructure issues, alongside improvements to Kubernetes configuration and authentication reliability.

## 🚀 Features
* **[Messaging] Integrated Assistant Reply Bridge:** Bridged Claude assistant replies directly into the Hub message store. This ensures that the "received" half of conversation history is correctly populated in the agent detail view.
* **[Messaging] Bidirectional Message History:** Updated the agent messages endpoint to support bidirectional conversation history, ensuring users can see both their sent messages and agent replies in a single cohesive view.
* **[Web UI] Dynamic Page Titles:** Implemented context-aware browser tab titles that reflect current navigation context, improving accessibility and multi-tab management.
* **[Auth] GitHub Device Flow Fallback:** Introduced GitHub as a fallback provider for device flow authentication, enhancing login reliability for remote environments.
* **[Kubernetes] Unified Template Configuration:** Merged Kubernetes resource and node selector overrides into a consistent configuration schema for improved template management.

## 🐛 Fixes
* **[Messaging] Resolved 405 Error on Messages Tab:** Fixed a routing conflict that caused the agent messages tab to return a "405 Method Not Allowed" error instead of retrieving message history.
* **[Web UI] Inline Maintenance Feedback:** Improved user feedback by displaying specific error messages inline when maintenance operations fail within the web interface.
* **[Web UI] Messages Tab Accessibility:** Decoupled the Messages tab from the `cloudLogging` feature gate, ensuring it remains accessible regardless of logging configuration.
* **[Infrastructure] Maintenance Robustness:** Added enhanced debug logging and panic recovery to Hub maintenance routines to improve system stability during background tasks.
* **[Infrastructure] Stalled Agent Detection:** Refined stalled agent detection logic to correctly exclude agents in legitimate `idle` or `waiting_for_input` states, preventing false positives.
* **[Infrastructure] Harness Config Sync:** Resolved an issue preventing successful harness configuration synchronization when targeting local Hub storage.
* **[Infrastructure] IAM Permissions:** Restored missing IAM permissions required for automated Hub Service Account management and provisioning.
