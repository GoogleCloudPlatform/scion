# Release Notes (2026-05-11)

This release is headlined by a comprehensive, multi-phase migration of the "Grove" terminology to "Project" across the entire Scion ecosystem. The migration includes extensive backward compatibility layers to ensure a smooth transition for existing integrations.

## 🚀 Features

* **Grove to Project Migration (Phases 1-6):** A system-wide refactor renaming "Grove" to "Project" throughout the codebase, API, and CLI.
    * **Backward Compatibility:** Implemented dual-field support for JSON payloads, WebSocket protocols, and REST API paths.
    * **Environment Parity:** Added dual environment variable export (e.g., `SCION_PROJECT_ID` alongside legacy `SCION_GROVE_ID`) and container labels.
    * **CLI Updates:** Renamed CLI commands while maintaining support for legacy aliases and paths.
    * **API Deprecation:** Added deprecation headers to `/api/v1/groves` endpoints to signal future removal.
* **Config Enhancements:** Support for `project_id` and `hub.projectId` within versioned settings.
* **Validation Tooling:** Introduced `hack/validate-rename.sh` to ensure terminology consistency across the codebase.

## 🐛 Fixes

* **Path Traversal Protection:** Restored critical path traversal protections and updated tests to handle the new project-based filesystem layout.
* **Workspace Path Resolution:** Implemented content-aware fallback logic to resolve workspace paths from both legacy `/groves` and new `/projects` directories.
* **Database Stability:** Resolved an index column order inconsistency for agent tables in the database.
* **A2A Bridge:** Updated the bridge to subscribe to both `scion.project` and legacy `scion.grove` topics simultaneously.
* **Regression Fixes:** Addressed various regressions in JSON unmarshaling, GCP service account minting, and hub-broker protocol mismatches identified during the migration.

## 🛠️ Internal & Build

* **Docker Base Image:** Added `keyring` to the Docker base image to support secure credential management.
* **Code Quality:** Resolved numerous compilation and `go vet` errors resulting from the large-scale rebase and refactor.
* **Documentation:** Updated internal strategy documents and source code comments to reflect the new Project-centric architecture.
