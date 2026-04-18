# Release Notes (2026-04-14)

This release focuses on improving system stability, refining the agent lifecycle, and addressing numerous test failures in CI and containerized environments. Key highlights include more robust handling of agent restarts and recreations, as well as several fixes for Kubernetes and deployment-related issues.

## 🚀 Features
(None)

## 🐛 Fixes

### Agent Lifecycle & Management
- **Agent Recreation:** Improved agent recreation logic to delete stopped agents and create fresh instances instead of attempting to restart them in-place. This ensures a clean state for recreated agents.
- **Restart API Status:** The API now returns `200 OK` instead of `201 Created` when successfully restarting an existing agent that was already in a `Created` or `Provisioning` state.
- **Terminal Reliability:** Fixed an issue where `tmux` sessions would not redraw correctly upon attachment by ensuring the window size is updated to the latest client dimensions.
- **Worktree Protection:** Enhanced container worktree guards to prevent nested worktree creation and correctly handle UID mapping in containerized environments.

### Core Stability & Test Improvements
- **CI/Test Stability:** Resolved multiple test failures across several groups, specifically addressing issues related to worktree environments, network dependencies in shared workspace tests, and missing harness configurations.
- **Workspace Validation:** Added strict validation to reject workspace operations on git-anchored groves that do not have any registered providers.

### Connectivity & Deployment
- **Endpoint Resolution:** Fixed logic for resolving `SCION_HUB_ENDPOINT` on combined servers and during sync preflight to ensure correct Hub connectivity.
- **Server Rebuild Process:** Improved the `rebuild-server` process to use a staging path and `sudo install`, avoiding `ETXTBSY` errors when updating a running server binary.
- **Kubernetes & GKE:** Addressed deployment and attachment issues on GKE, including polkitd rule setup and sudo password prompts during pod attachment.
- **Log Resolution:** Fixed path resolution for agent logs in the broker's `getLogs` handler.
- **Subcommand Flexibility:** Relaxed configuration checks (image registry and grove status) for non-API server subcommands.
