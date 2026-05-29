# Release Notes (2026-04-27)

This release focuses on standardizing the Model Context Protocol (MCP) integration across harnesses and introducing a major overhaul to the container image building system with support for multiple backends.

## ⚠️ BREAKING CHANGES
* **Claude Harness Configuration:** Environment variables `ANTHROPIC_MODEL` and `ANTHROPIC_SMALL_FAST_MODEL` have been moved from `settings.json` to `config.yaml`. This migration aligns Claude harness settings with the standard Scion configuration model. Users with custom `settings.json` overrides should migrate these keys to their `config.yaml`.

## 🚀 Features
* **Universal MCP Server Support:** Implemented a standardized `mcp_servers` schema and validation across the API. This enables consistent MCP server configuration for major harnesses including Opencode, Codex, and Web-Dev.
* **Pluggable Container Builder:** Refactored the `image-build` system to support pluggable backends. Users can now select between `local-docker`, `local-podman`, and `cloud-build` using the new `--builder` flag in `build-images.sh`.
* **Container-Script Harness Enhancements:** Enabled `allow_container_script_harnesses` by default and introduced a shared `scion_harness.py` provisioning helper to simplify harness setup.
* **New 'harness-config install' Command:** Added a new CLI command to streamline the installation and management of harness configurations.
* **Amp Harness Example:** Introduced the "Amp" harness as a reference implementation and design document for developers building complex tool-calling agents.
* **Discord Integration Design:** Finalized the architecture for the Discord chat adapter, paving the way for upcoming multi-platform chat support.

## 🐛 Fixes
* **Cloud Build Robustness:** Improved the Cloud Build pipeline with automatic project detection from registry URLs and pre-flight permission checks to prevent late-stage build failures.
* **Deduplication:** Fixed an issue where harness configurations were incorrectly duplicated when listing agents by grove ID.
* **Notification Noise:** Resolved a race condition that resulted in duplicate "waiting for input" notifications in the inbox.
* **Harness Cleanup:** Standardized the Web-Dev harness by removing obsolete configuration shims and improving MCP integration.

## 🛠️ Internal & Others
* Added PostgreSQL support strategy documentation.
* Refactored local build logic to make registry flags optional for development.
* Standardized license headers across several internal scripts.
