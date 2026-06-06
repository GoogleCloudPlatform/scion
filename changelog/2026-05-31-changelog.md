# Release Notes (2026-05-31)

This release introduces significant architectural improvements to template portability and a major terminology shift from "grove" to "project" across the entire system.

## ⚠️ BREAKING CHANGES
- **Template Portability:** Harness-specific fields (`image`, `model` when using concrete provider names, and `auth_selectedType`) are now deprecated in `scion-agent.yaml`. While these remain functional for backward compatibility, they will trigger deprecation warnings. Users should migrate these settings to harness configurations and use the new model size aliases.

## 🚀 Features
- **Harness-Agnostic Templates:** Templates are now decoupled from specific LLM backends for improved portability.
    - **Model Size Aliases:** Introduced `small`, `medium`, and `large` aliases. Harness configurations now map these to concrete provider models (e.g., `gemini-pro`, `claude-opus`), allowing the same template to work across different harnesses.
    - **Deprecation Warnings:** The system now warns when templates contain hardcoded harness-specific environment or model data.
- **Terminology Shift (Grove → Project):** The term "grove" has been replaced with **"project"** in all user-facing CLI help text, logs, and documentation.
    - The filesystem watcher now supports the `--project` flag (with `--grove` maintained as a deprecated alias).
    - Internal settings and default configurations have been updated to reflect the "project" naming convention.
- **Vocabulary Alignment:**
    - Server mode "production" has been renamed to **"hosted"**.
    - "Hub-native" naming has converged to **"hub-managed"**.
    - The messaging "set" concept is now referred to as a **"message group"**.
- **Skills Management:** Skills are now strictly template-only, simplifying their integration and lifecycle within the agent composition model.

## 🐛 Fixes
- **Agent Identity & Collision:**
    - Resolved cross-project slug collisions in broker exec/stop operations (impacting `scion look`).
    - Fixed agent slug collisions across projects during broker heartbeats.
- **Messaging Improvements:**
    - Fixed Telegram mention parsing for agents with hyphenated names.
    - Improved error handling when providing bare email recipients to the message command.
    - Added validation for empty event names and corrected missing field mappings in `MappingDialect`.
- **System Stability:**
    - Fixed CI failures resulting from the major terminology rename.
    - Removed defunct harness plugin types from the plugin system.
    - Cleaned up stale deterministic-UUID language and corrected internal code documentation.
