# Release Notes (2026-05-08)

This release introduces a new agent lifecycle for suspending and resuming sessions, adds a new A2A protocol bridge, and improves the reliability and rendering of messaging across the platform.

## 🚀 Features
* **[Lifecycle]: Agent Suspend and Resume:** Introduced the ability to suspend agents, preserving their session state for later resumption. Advanced harnesses (Claude, Gemini, Codex) now support `scion suspend` to "pause" an agent's work, and `scion start` has been enhanced to automatically detect and resume suspended sessions.
* **[Extras]: Scion-A2A Bridge:** Added a new `scion-a2a-bridge` service that allows Scion agents to be integrated with any application supporting the Agent-to-Agent (A2A) protocol. Features include SSE streaming for real-time responses, push notification webhooks, and automatic agent provisioning.
* **[Extras]: Message Debugging:** Added the `scion-broker-log` plugin, a debugging tool that allows developers to monitor, log, and proxy broker messages in real-time.
* **[Web]: Lifecycle UI:** Added Suspend and Resume actions to the agent management interface, with visibility gated by harness capabilities.

## 🐛 Fixes
* **[Chat]: Improved Message Rendering:** Resolved an issue in the Google Chat integration where long assistant responses were incorrectly rendered as notification cards. Responses are now properly truncated and displayed as direct message cards.
* **[Messaging]: Hub Reliability:** Fixed a race condition to ensure that messages are correctly persisted and delivered via SSE even when the runtime broker is under heavy load.
* **[Harness]: Subagent Isolation:** Refined the hook pipeline to properly isolate subagent stop events, preventing them from prematurely updating the state or turn counts of the parent agent.
* **[CLI]: Shell Completion:** Enhanced the `scion completion` command to correctly handle permission requirements when writing scripts to system directories like `/etc/bash_completion.d`.
* **[Broker]: Plugin Callbacks:** Fixed host callback forwarding in the broker log plugin to ensure consistent event delivery to downstream plugins.

## ⚠️ BREAKING CHANGES
* **[Lifecycle]:** `scion start` now defaults to resuming a suspended agent if one exists, rather than starting a fresh session.
* **[Lifecycle]:** The `scion suspend` command is now explicitly gated on harness support; attempting to suspend an agent using a harness that does not support session preservation will result in an error.
