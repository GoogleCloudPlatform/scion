# Release Notes (Feb 24, 2026)

This release introduces a robust policy-based authorization system, a comprehensive agent notification framework, and significant enhancements to hub-managed groves and schema validation.

## ⚠️ BREAKING CHANGES
* **Policy-Based Authorization:** Strictly enforced authorization for agent operations. Agent creation now requires grove membership, while interaction (PTY, messaging) and deletion are restricted to the agent's owner (creator) or system administrators.

## 🚀 Features
* **Agent Notifications System:** Launched a multi-phase notification framework enabling real-time subscriptions to agent status events. This includes a new notification dispatcher, Hub API endpoints, and a `--notify` flag in the CLI for status tracking.
* **Harness-Agnostic Templates:** Introduced support for role-based, harness-agnostic agent templates. New fields for `agent_instructions`, `system_prompt`, and `default_harness_config` allow templates to be defined by their role rather than specific LLM implementations.
* **GKE Security Enhancements:** Added a dedicated `gke` runtime configuration option to enable GKE-specific features like Workload Identity, streamlining secure deployments on Google Kubernetes Engine.
* **Hub-Managed Workspace Management:** Advanced hub-managed grove capabilities (Phase 3) with new support for direct workspace file management via the Hub API, reducing reliance on external Git repositories.
* **ADK Agent Integration:** Added a specialized example and Docker template for Agent Development Kit (ADK) agents, facilitating the development of custom autonomous agents within the Scion ecosystem.
* **Infrastructure & Models:** Upgraded the default agent model to `gemini-3-flash-preview` and introduced Cloud Build configurations for automated image delivery.

## 🐛 Fixes
* **Schema & Config Synchronization:** Conducted a comprehensive audit and sync between Go configuration structs and JSON schemas. This fixes field naming inconsistencies (e.g., camelCase for `runtimeClassName`) and improves cross-platform validation.
* **Environment Variable Passthrough:** Corrected environment handling to treat empty variable values as implicit host environment passthroughs.
* **Per-Agent Hub Overrides:** Enabled agents to specify custom Hub endpoints directly in their configuration, providing flexibility for agents to report to different Hubs than their parent grove.
* **Soft-Delete Configuration:** Added explicit server-side settings for soft-delete retention periods and workspace file preservation.
