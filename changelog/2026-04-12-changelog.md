# Release Notes (2026-04-12)

This release focuses on strengthening the storage proxy system and improving runtime compatibility for both Kubernetes and rootless Podman environments.

## 🚀 Features
* **Kubernetes Integration:** The system now automatically detects the runtime namespace when running within a Kubernetes cluster, simplifying configuration for in-cluster deployments.
* **Agent Observability:** Enabled retrieval of agent logs via GET requests through the runtime broker, providing a more accessible way to monitor agent behavior.

## 🐛 Fixes
* **Local Storage Proxying:** Implemented a secure proxy for local storage uploads and downloads through the Hub HTTP endpoint. This includes authenticated transfers and streaming support, ensuring reliable file management when direct access is restricted.
* **Podman Rootless Stability:** Enhanced rootless Podman operations by ensuring consistent user ID mapping and reliable cleanup of home directories using `podman unshare`.
* **CLI Tooling:** Fixed a permission issue in `sciontool` where home directories were not correctly owned before dropping privileges during initialization.
* **Broker Runtime Resolution:** Fixed an issue where the broker would not correctly respect grove-level settings for runtime selection when no explicit profile was provided.
