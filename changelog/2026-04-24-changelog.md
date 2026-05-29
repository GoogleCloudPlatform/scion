# Release Notes (2026-04-24)

This update focuses on hardening template harness configurations and refining authentication flows for both cloud and local development environments.

## 🚀 Features
*(No major features were introduced in this period.)*

## 🐛 Fixes
* **Template Harness Configuration:** Implemented a significant overhaul of how default harness configurations are handled. This includes backfilling defaults for existing templates, ensuring manual edits in the hub are preserved, and correctly prioritizing user-provided configurations over template defaults during agent creation.
* **Authentication & Environment:** Improved Vertex AI authentication by automatically detecting projects from the environment and refined local development workflows by preferring developer credentials over stale agent tokens when connecting to localhost endpoints.
* **Platform & UI Stability:** Resolved compatibility issues with unsupported container flags on Apple runtimes and fixed a dark-mode CSS injection issue that caused inconsistent badge rendering when the application is served via the Go backend.
