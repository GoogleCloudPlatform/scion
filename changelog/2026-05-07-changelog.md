# Release Notes (2026-05-07)

This release introduces a new Hub user invitation system, enhances agent reliability with automatic crash detection, and provides new debugging tools for broker message traffic.

## 🚀 Features
* **Hub User Invitation System:** Implemented a multi-phase user invitation system for the Hub. This includes an allow-list foundation, invite code generation, and enhanced polish with observability features for administrators.
* **Agent Crash Detection:** Agents now automatically detect container crashes and transition to an error state. This ensures that the system accurately reflects the operational status of agents and provides immediate feedback for troubleshooting.
* **Broker Message Debugging (scion-broker-log):** Introduced the `scion-broker-log` plugin, a new debugging tool for inspecting message traffic through the broker. It includes a `--forward` flag to support tee and proxy modes for advanced debugging scenarios.
* **UI Capability Gating:** Enhanced the web interface to hide or gate the "Suspend" button based on the specific capabilities of the agent's harness. If an agent does not support the resume capability, the suspend option is suppressed.

## 🐛 Fixes
* **Runtime Argument Ordering:** Standardized the position of the `SCION_START_CMD` environment variable in container arguments to ensure consistent and reliable agent execution across different environments.
* **Prompt Escaping:** Switched to POSIX single-quote escaping for prompts embedded within shell commands, improving compatibility and robustness for complex instructions.
* **Hub Token Reliability:** Implemented atomic file writes for Hub tokens, eliminating transient 401 Unauthorized errors caused by race conditions during token refresh.
* **Test Cleanup:** Resolved an issue where automated tests could leave orphan trigger files on disk, ensuring a cleaner development environment.

## 🛠️ Internal & Others
* Added comprehensive README and design documentation for the Hub user invitation system and the `scion-broker-log` plugin.
