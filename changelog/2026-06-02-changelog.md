# Release Notes (2026-06-02)

This release focuses on improving the robustness of the message broker with better channel filtering and thread support, alongside critical fixes for test isolation.

## 🚀 Features
* **Message Broker Channel Filtering & Threading:** Introduced strict channel filtering in broker plugins to prevent cross-channel message delivery (e.g., Telegram replies leaking into Google Chat). Added end-to-end support for thread ID propagation, ensuring agent replies land in the correct conversation threads or forum topics across supported platforms.
* **Google Chat Thread Context:** The Google Chat integration now automatically captures and propagates thread context for both inbound messages and `ask_user` dialog responses.

## 🐛 Fixes
* **Test Suite Hub Isolation:** Fixed a significant issue where integration tests could leak live Hub credentials from the environment. This prevented tests from accidentally resetting the state of real agents. The fix includes new test helpers for safe environment variable management.
* **Chat App Routing:** Resolved routing issues where `ask_user` responses and outbound messages were occasionally misdirected due to missing or incorrect channel identifiers.
* **Telegram Thread ID Forwarding:** Fixed a bug in the Telegram plugin where thread IDs were captured on inbound messages but not included in outbound replies.
