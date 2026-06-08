# Release Notes (2026-06-06)

This release focuses on significantly improving authentication resilience, specifically addressing token expiry deadlocks and providing new tools for agent recovery. It also introduces project renaming and strengthens agent identity with unique slugs.

## ⚠️ BREAKING CHANGES
* **[Agents]:** Agent slugs are now enforced to be unique within a single project. Operations that would create or rename an agent to a duplicate slug will now fail with a validation error.

## 🚀 Features
* **Authentication Resilience and Recovery:**
    * **Diagnostic Tools:** Introduced `sciontool doctor`, a new diagnostic command to verify agent health, connectivity, and authentication status from within the container.
    * **Auth Reset Mechanism:** Added a "Reset Auth" mechanism to repair-inject fresh authentication tokens into running agent containers without requiring a restart. This is accessible via the `scion reset-auth` CLI command and a new button in the Agent detail UI.
* **Project Management:**
    * **Project Rename:** Added support for renaming projects through both the CLI and Hub API.
* **Agent Progeny Support:**
    * Agents are now empowered to create sub-agents. This was enabled by refactoring internal principal tracking to support both users and agents, resolving a schema constraint that previously blocked agent-initiated operations.

## 🐛 Fixes
* **Authentication & Session Stability:**
    * Resolved a critical deadlock where auth tokens could fail to refresh after a hub signing-key rotation.
    * Fixed multi-node session issues including OAuth `state_mismatch` errors and inconsistent signing key usage across nodes.
    * Improved hub stability during upgrades with targeted triage remediation for potential authentication breakage.
* **System Integrity:**
    * Switched to deterministic UUIDs for plugin broker IDs to ensure consistency and stability during system migrations.
