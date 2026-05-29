# Release Notes (2026-04-10 - 2026-04-25)

This update introduces significant security hardening for Kubernetes environments, a new tool for analyzing Pull Request dependencies, and several improvements to agent execution reliability and configuration management.

## 🚀 Features
* **PR Dependency Tooling:** Added a new tool to generate dependency graphs for Pull Requests, including an `--infer` flag to automatically detect dependencies via git ancestry.
* **Security & Kubernetes Hardening:** Hardened agent security by running Kubernetes pods as non-root by default. Added support for decoding file secrets in Kubernetes and custom OTLP CA bundles.
* **Agent Execution Enhancements:** Enhanced agent execution with exit code propagation, timeout support, and shared action definitions. Added support for grove-scoped execution.
* **Configuration Management:** Support for hub IDs in versioned server configurations and improved hub endpoint resolution via the `--base-url` flag.
* **Infrastructure Monitoring:** Introduced a new `vm-pulse` script for tracking VM health.

## 🐛 Fixes
* **Agent Lifecycle:** Resolved issues where heartbeats could incorrectly revert stopped agent states and improved agent phase derivation from container status. Failed local launches are now correctly reported as errors.
* **Permissions & Environment:** Improved privilege handling in `sciontool` and Podman rootless environments. Optimized workspace ownership management for non-root containers.
* **Routing & Connectivity:** Fixed profile resolution to respect grove-level overrides and resolved various routing issues related to broker registration and standalone mode.
* **Claude Harness:** Improved the Claude harness by pre-trusting workspaces and removing unnecessary model suffixes.
* **UI/UX Improvements:** Improved GitHub URL styling, user list sorting, and resolved agent identity collisions across different groves.
