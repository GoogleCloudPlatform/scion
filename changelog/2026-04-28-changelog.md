# Release Notes (2026-04-28)

This release focuses on improving agent reliability through automated bundle reconciliation, enhancing the `opencode` harness configuration, and standardizing Cloud Build templates.

## 🚀 Features
* **[Diagnostics]:** Improved agent troubleshooting by logging `provision.py` output directly to `agent.log`. This provides better visibility into the container-side provisioning process.

## 🐛 Fixes
* **[Harness Configuration]:** The `opencode` harness now defaults to the `container-script` provisioner for new installations. This ensures full support for modern features like MCP server translation and automated authentication resolution. Existing installations can migrate using `scion harness-config upgrade opencode --activate-script`.
* **[Agent Lifecycle]:** Implemented automatic, idempotent reconciliation of the `container-script` bundle on every agent start or resume. This ensures that agents provisioned before recent architectural migrations have all necessary hooks and scripts correctly staged.
* **[Cloud Build]:** Standardized Cloud Build templates by ensuring required SHA substitutions (`_SHORT_SHA`, `_COMMIT_SHA`) are declared across all templates and filtering out unused substitutions to prevent build errors.
* **[Harness Stability]:** Resolved several edge cases in harness configuration loading, including improved error handling for malformed configuration directories and more robust grove filtering during auto-selection.

## 📝 Documentation
* Added a design document for the **Hub Template Admin** section, outlining the roadmap for centralized management of global and grove-scoped templates.
