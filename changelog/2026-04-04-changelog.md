# Release Notes (2026-04-04)

This update focuses on improving GCP identity handling for agents, enhancing the template configuration experience with more flexible URL support, and refining the user interface for grove configuration.

## 🚀 Features
* **Flexible Template URLs:** Support for scheme-less GitHub URLs in grove settings (e.g., `github.com/org/repo`) has been added. The system now automatically prepends `https://` and appends `/.scion/templates/` when a deeper path is not specified, simplifying remote template setup.
* **UI Configuration Improvements:** The branch selection field is now intelligently hidden when configuring agents for non-git groves or shared-workspace git groves, reducing UI clutter and potential configuration errors.

## 🐛 Fixes
* **GCP Identity Propagation:** Fixed a critical bug where GCP service account identities were not correctly forwarded to the broker during the "Create & Edit" agent startup path. Identity details are now correctly injected from the agent's applied configuration.
* **Grove Search Accuracy:** Improved the `ListGroves` API to use exact matching for git remote filters. This prevents collision issues where repositories with similar prefix names would incorrectly appear in filtered results.

## 📝 Documentation
* **GCP Authentication Setup:** Added comprehensive documentation for configuring GCP authentication, including the setup of the `scion-telemetry-gcp-credentials` secret.
