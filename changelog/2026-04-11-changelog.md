# Release Notes (2026-04-11)

This release focuses on Kubernetes security hardening, unified agent action dispatching, and a new dependency analysis toolset.

## ⚠️ BREAKING CHANGES
* **Kubernetes Runtime:** Kubernetes agent pods now run as non-root by default (UID 1000). While this aligns with security best practices, custom images that require root privileges or specific user configurations may need adjustment.

## 🚀 Features
* **PR Dependency Graphing:** Added a new `pr-deps.sh` utility and a `--infer` flag for the CLI to automatically detect and map dependencies between pull requests using git ancestry analysis.
* **Unified Agent Actions:** Refactored agent action dispatching to route all hub agent executions through a centralized dispatcher, ensuring consistent handling across local and hosted environments.
* **Kubernetes Enhancements:** 
    * Added support for decoding file-based secrets in the Kubernetes runtime.
    * Hardened agent pods by default, including automatic injection of `HOME`, `USER`, and `LOGNAME` environment variables to ensure tools resolve identity correctly without `/etc/passwd` lookups.
* **Improved Execution:** Enhanced `agent exec` with support for timeouts and reliable exit code propagation from the container to the CLI.
* **Secure Observability:** Introduced support for custom OTLP CA bundles, enabling secure telemetry exports in environments with private certificate authorities.

## 🐛 Fixes
* **Agent State Management:** Fixed a race condition where heartbeats could inadvertently revert an agent's state from "stopped" to "running."
* **Profile Resolution:** Resolved an issue where grove-level `active_profile` overrides were ignored for existing agents.
* **Runtime Consistency:** 
    * Standardized agent phase derivation from container status across all runtime `List` methods.
    * Fixed Podman rootless detection to correctly return the `scion` user instead of `root` for exec operations.
* **OAuth & Config:** Improved OAuth provider fallback logic and added a configuration loader fix to remap legacy V1 grove ID fields.
* **Claude Harness:** Fixed workspace trust issues and removed the redundant `@default` model suffix in the Claude harness.
