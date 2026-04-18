# Release Notes (2026-04-11)

This release focuses on enhancing the Kubernetes runtime security, improving PR dependency management, and resolving several critical consistency issues in agent state and configuration resolution.

## 🚀 Features
* **PR Dependency Tooling:** Introduced `pr-deps.sh`, a tool for generating PR dependency graphs. It includes a new `--infer` flag that uses git ancestry to automatically detect dependencies between pull requests.
* **Kubernetes Hardening:** Agent pods in Kubernetes now run as non-root by default. Additionally, added support for decoding file-based secrets within the Kubernetes runtime.
* **Stable Hub Identifiers:** Versioned server configurations (`settings.yaml`) now support a stable `hub_id` field. This ensures consistent secret namespacing across hub-scoped deployments.
* **Enhanced Agent Execution:** Agent execution now correctly propagates exit codes to the caller and supports configurable wire timeouts for more robust remote operations.
* **Agent Action Refactoring:** Consolidated and shared agent action definitions across the hub and dispatcher, including improved handling for grove-scoped execution actions.
* **OTLP Custom CA Support:** Added support for providing custom CA bundles for OpenTelemetry (OTLP) exporters.
* **Workspace Optimization:** Reduced overhead by skipping workspace bootstrapping during global hub starts and bypassing `chown` operations when containers already run as non-root.
* **Grove Persistence:** Improved grove management by preserving explicit clone URLs and enabling recovery of hosted grove IDs from workspace markers.

## 🐛 Fixes
* **Agent State Consistency:** Fixed a race condition where heartbeats could revert a stopped agent's state. Also ensured the agent phase is accurately derived from container status in runtime list methods.
* **Endpoint & Profile Resolution:** 
    * The `--base-url` flag is now correctly respected for hub endpoint resolution.
    * Profile resolution now correctly prioritizes grove-level `active_profile` overrides.
    * Resolved an infinite loop in `scion ls` when using V1 settings by fixing field remapping for `grove_id`.
* **Harness Fixes:** The Claude harness now correctly pre-trusts the workspace and removes unnecessary `@default` model suffixes.
* **List Accuracy:** The global agents list endpoint now returns all agents, bringing it into parity with the grove-scoped list behavior.
* **Dispatching & Conflicts:** Improved dispatcher reliability by routing hub agent execution through the dispatcher and adding retry logic for agent update conflicts.
* **Podman Rootless:** Fixed an issue where rootless Podman execution incorrectly identified as `root` instead of the `scion` user.
* **OAuth Reliability:** Improved fallback logic for OAuth providers to ensure smoother authentication flows.
* **Volume Mounting:** Fixed a bug in the dispatcher where volumes were not correctly applied due to missing `InlineConfig` during agent retrieval.
