# Release Notes (2026-04-18)

This release focuses on improving agent isolation in shared workspaces and enhancing Kubernetes runtime support for environments with sidecar containers.

## 🚀 Features
* **Enhanced Agent Isolation in Shared Workspaces:** Implemented per-agent state isolation in hub-hosted git groves. Agents in shared-workspace mode no longer have access to the configuration files of their siblings. This includes an automatic migration of legacy state for existing agents.

## 🐛 Fixes
* **Kubernetes Sidecar Support:** Updated the Kubernetes runtime to correctly identify and interact with the agent container in pods containing sidecars (e.g., Istio, Linkerd). This resolves issues with logs, listing, and execution in multi-container environments.
* **SSE Agent Action Buttons:** Resolved an issue where newly created agents appearing via realtime SSE updates were missing action buttons (start, stop, etc.) until a manual page refresh.
* **Shared-Workspace Persistence:** Fixed a bug where agents in hub-hosted groves would lose their shared-workspace configuration upon restart, ensuring they continue to use the shared git checkout rather than spawning unnecessary worktrees.
