# Release Notes (Feb 25, 2026)

This release focuses on hardening the agent provisioning pipeline, streamlining template management through automatic bootstrapping, and enhancing the web authentication experience.

## 🚀 Features
* **Template Bootstrapping:** Local agent templates are now automatically bootstrapped into the Hub database during server startup, ensuring all defined templates are consistently available across the system.
* **Custom ADK Runner Entrypoint:** Introduced a specialized runner entrypoint for Agent Development Kit (ADK) agents with native support for the `--input` flag, facilitating more robust automated execution.
* **Wildcard Subdomain Authorization:** Expanded security configuration to support wildcard subdomain matching in `authorized-domains`, allowing for more flexible deployment architectures.

## 🐛 Fixes
* **Agent Provisioning & Creation:** Resolved multiple issues in the Hub-dispatched agent creation flow, including a 403 authorization fix, rejection of duplicate agent names, and a critical fix for container image resolution.
* **Instruction Injection Logic:** Improved the reliability of agent instructions by implementing auto-detection for `agents.md` and ensuring stale instruction files (e.g., lowercase `claude.md`) are removed during provisioning.
* **Web UI & Auth Persistence:** Fixed a bug where the authenticated user wasn't correctly fetched on page load, ensuring the profile and sign-out options are always visible in the header.
* **Pathing & Scoping:** Corrected path resolution logic to prevent local-path groves from incorrectly using hub-managed paths, and refined the `scion delete --stopped` command to strictly scope to the active grove.
* **Environment Gathering:** Fixed a regression in the `env-gather` finalize-env flow to ensure the template slug is correctly preserved throughout the entire provisioning pipeline.
* **Configuration Schema:** Added `task_flag` support to the settings schema and Hub configuration, improving the tracking and validation of agent task states.
