# Release Notes (2026-05-06)

This release focused on improving agent observability and refining the Google Chat integration. Key architectural changes were introduced to better manage subagent lifecycles and ensure structured communication between the harness and external platforms.

## 🚀 Features
* **Subagent Lifecycle Isolation:** Introduced a distinct `subagent-end` event in the hook pipeline. This decouples internal subagent turn completions from the main agent's state, preventing subagent activity from incorrectly driving status updates, messaging, or resource tracking.
* **Observability Architecture Analysis:** Published a comprehensive design analysis of the harness observability systems (hooks, broker, and ACP). This work identifies critical paths for content filtering and proposes a new classification architecture to prevent internal reasoning/thinking content from leaking into user-facing channels.

## 🐛 Fixes
* **Google Chat Integration Refinement:** Resolved multiple issues in the Google Chat relay:
    * Implemented intelligent message routing for assistant replies, ensuring they are rendered as direct cards rather than notification cards.
    * Added 500-character truncation for long assistant replies to maintain chat readability.
    * Introduced a structured `Status` field for activity messages, replacing unreliable keyword-based detection with precise status reporting (e.g., COMPLETED, ERROR).
* **CLI & Installation:**
    * Updated binary installation instructions to reflect the correct build path and `sudo` requirements.
    * Fixed shell completion setup commands to correctly handle write permissions for system directories.
