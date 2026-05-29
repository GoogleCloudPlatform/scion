# Release Notes (2026-05-17)

This period is marked by a major architectural transition, renaming the core "Grove" concept to "Project" across the entire ecosystem. Alongside this shift, we've improved messaging capabilities, unified administrative interfaces, and enhanced the reliability of our migration and installation processes.

## ⚠️ BREAKING CHANGES
* **Grove to Project Rename:** The concept of "Groves" has been renamed to "Projects" across the CLI, API, Database, and Web UI.
    * **CLI:** Primary commands now use `projects` (e.g., `scion projects list`), though `groves` remains supported as a hidden alias for backward compatibility.
    * **Environment Variables:** `SCION_PROJECT_ID` and `SCION_PROJECT_PATH` are now preferred over their `GROVE` counterparts.
    * **API:** New `/api/v1/projects` endpoints have been introduced. Legacy `/api/v1/groves` endpoints now include deprecation headers and will be removed in a future release.
    * **Database:** Internal schemas have been updated, but data migrations are idempotent and include filesystem fallbacks to ensure existing "groves" directories continue to function.

## 🚀 Features
* **[hub] Unified Management UX:** Combined the allow list and invite management interfaces into a single, cohesive experience for administrators.
* **[cmd] Multi-Target Messaging:** Introduced the `set[]` composite recipient in the `message` command, enabling simultaneous message delivery to multiple agents or users.
* **[cmd] Agent Wake Control:** Added a new `--wake` flag to the `message` command, allowing users to explicitly wake dormant agents when sending a message.
* **[web] Shared Directory ZIP Downloads:** Added the ability to download the entire contents of a shared directory as a single ZIP archive.
* **[web] Enhanced Activity Monitoring:** Agent detail pages now feature relative timestamps on activity badges, providing clearer insight into when an agent was last active.
* **[skills] Team Builder:** Added a hand-tuned "team-builder" skill to assist in coordinating multi-agent workflows.

## 🐛 Fixes
* **[migration] Resilient Database Updates:** Improved the V50 and V53 migrations to be idempotent and more resilient to missing tables, preventing hub startup failures in partially migrated environments.
* **[agent] Accurate State Reporting:** Renamed the `ActivityIdle` state to `ActivityWorking` to more accurately reflect agent status during active task processing.
* **[security] Path Traversal Protection:** Restored and strengthened path traversal protections to account for the new project-based filesystem structure and fallbacks.
* **[installation] Improved Path Handling:** The installer now defaults to `/usr/local/bin` and provides a warning if the installation path is not present in the user's `PATH`.
* **[cmd] Help Command Precision:** Refined the help command logic to ensure it only triggers when "help" is the sole argument, preventing accidental activation during complex CLI operations.
* **[build] Docker Keyring Support:** Included the `keyring` package in the Docker base image to facilitate more secure credential handling within containers.
