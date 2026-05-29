# Release Notes (2026-05-04)

This update focuses on strengthening grove isolation, improving Chat App usability by splitting administrative and messaging commands, and introducing a new CLI identity system.

## ⚠️ BREAKING CHANGES
* **Chat App Command Split:** The `/scion` slash command has been split into two separate commands to improve clarity and reduce command collisions.
    * `/scion`: Now dedicated strictly to agent messaging.
    * `/scionAdmin`: A new command that handles all administrative tasks, including space linking (`link`), registration (`register`), and agent management (`list`, `start`, `stop`, `create`, `delete`).
    * **Action Required:** Users and administrators must update their Google Chat App configurations to include the new `/scionAdmin` command (Command ID 2) and update their workflows to use it for all management operations.

## 🚀 Features
* **CLI Mode System:** Implemented a new CLI mode system that distinguishes between `human`, `assistant`, and `agent` contexts, allowing for better-tailored command behavior and security.
* **Agent Identity:** Added the `scion whoami` command, allowing users and agents to verify their current identity and active CLI mode.
* **Metadata Server Diagnostics:** Introduced self-healing health checks and a new diagnostic command to the metadata server to improve system reliability.
* **Hub Rebuild Improvements:** Hub rebuild operations now support explicit branch selection, providing more flexibility for maintenance and testing.

## 🐛 Fixes
* **Grove Isolation & Security:**
    * Hardened agent lookup, message delivery, and subscriptions to be strictly scoped by grove, eliminating the risk of cross-grove routing.
    * Improved grove cloning by falling back to the creator's GitHub token when necessary.
* **Chat App Stability:**
    * Improved slash command routing to correctly handle numeric Command ID mappings from the Google Chat console.
    * Fixed command name parsing from message text when ID maps are missing.
* **Infrastructure & Web:**
    * The `starter-hub` setup script now correctly waits for `cloud-init` to finish before attempting SSH connections.
    * Added a dedicated error page for cases where web assets are not properly embedded in the binary.
    * Resolved TypeScript type errors in the file browser signal parameters.
    * Standardized internal `Message` and `MessageRaw` API calls to include `groveID` for consistent isolation across all call sites.
