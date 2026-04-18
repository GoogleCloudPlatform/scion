# Release Notes (2026-04-12)

This release focuses on enabling remote access to local storage through a new HTTP proxy system and improving support for rootless Podman and Kubernetes environments.

## 🚀 Features
* **Local Storage HTTP Proxy:** Implemented a robust proxy system for local storage uploads and downloads. Remote clients can now securely transfer template files, workspaces, and harness configurations via the Hub's HTTP endpoint, bypassing the need for direct filesystem access to the Hub's local storage.
* **Kubernetes In-Cluster Detection:** Added support for automatic detection of the Kubernetes runtime namespace when Scion is running within a cluster.
* **Runtime Broker Logs:** Enabled GET requests for retrieving agent logs from the runtime broker, improving observability and debugging.

## 🐛 Fixes
* **Podman Rootless Support:** Improved user identity handling and filesystem cleanup in rootless Podman environments. The system now correctly maintains user IDs and utilizes `podman unshare` for home directory cleanup.
* **Sciontool Permissions:** Resolved an issue where home directory ownership was not correctly initialized before privilege dropping in `sciontool`.
* **System Stability:** Addressed linting errors and improved the reliability of SSE (Server-Sent Events) delivery tests to ensure consistent real-time event communication.
