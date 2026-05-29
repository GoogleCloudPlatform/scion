# Release Notes (2026-04-23)

This update focuses on improving template configuration management, enhancing compatibility for Apple Silicon users, and refining authentication workflows for both Vertex AI and local development.

## 🚀 Features
* **[Template Management]:** Enhanced how templates manage harness configurations by introducing `DefaultHarnessConfig`. This preserves specific configuration names (e.g., "claude-web", "adk") during import and editing, ensuring agents use the intended configuration instead of a generic default. This also includes automatic backfilling for existing templates and better prioritization of user-specified configs.
* **[Apple Silicon Support]:** Improved runtime compatibility for macOS by automatically stripping unsupported container flags (such as `--cap-add`, `--device`, and `--mount`) when using the Apple `container` CLI.

## 🐛 Fixes
* **[Authentication]:** 
 * Vertex AI auth detection now correctly identifies the project from `GOOGLE_CLOUD_PROJECT` in the resolved environment, allowing users with GCP credentials to start agents without explicit auth configuration.
 * The CLI now correctly prioritizes local dev tokens over stale remote agent tokens when connecting to localhost endpoints.
* **[Web UI]:** Fixed an issue where dark-mode badge colors were not rendering correctly when the page was served via the Go backend. Theme CSS is now injected dynamically to ensure consistency across all serving methods.
