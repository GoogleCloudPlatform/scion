# Release Notes (2026-06-29)

The biggest day in weeks: the Claude harness was migrated from builtin to container-script provisioning, a full Cloud Run HA deployment stack landed (Dockerfile, IAP auth, stateless broker routing, Postgres locking), agent labels shipped as a core feature, and chat integrations began a config refactor with secrets migration.

## 🚀 Features
* **[Harness]:** Migrated Claude harness from builtin to container-script provisioning — `provision.py` handles Claude's 4-way auth precedence (API key → OAuth → auth-file → Vertex AI), API key pre-approval, MCP server translation, and env var overlay. Includes parity tests verifying identical output to the compiled harness. Compiled fallback preserved for existing installations (#279).
* **[Hub]:** Chat integration config refactor + secrets migration (Phase 1) — added `IntegrationConfigProvider` with YAML-based per-integration config files, well-known secret key constants for Telegram/Discord/Google Chat, and a `LoadPluginConfigFile` helper that merges file-based config with inline config while filtering secrets (#537).
* **[Agent]:** Agent-specific key-value labels added to the core data model — labels can be set at creation time, displayed on agent detail pages, and filtered/sorted in the agent list view (#531).
* **[Hub]:** Multi-stage `Dockerfile.hub` for Scion Hub — builds frontend assets, embeds them in the Go binary, uses `CGO_ENABLED=0` for a static binary compatible with Debian runtime. Iteratively refined through several commits to fix npm scripts, web asset embedding, and root Dockerfile sync.
* **[Hub]:** IAP proxy auth middleware for Cloud Run — creates hub sessions from IAP identity headers, re-evaluates admin role on every request (not just login), and reduces DB connection pool size for hosted deployments (#530 follow-up).
* **[Deployment]:** Reworked Cloud Run HA deployment config — overhauled `deploy.sh` and `hub-settings-template.yaml` with fail-closed HA preflight checks, stateless broker lifecycle routing, IAP audience normalization, and Postgres advisory locking for safe concurrent migrations.

## 🐛 Fixes
* **[Hub]:** Expire stuck pending messages in broker-message-sweep — messages that remain in `pending` status beyond a threshold are now cleaned up automatically (#545).
* **[Hub]:** Narrowed hosted HA preflight to actual HA deployments — previously the preflight blocked single-instance startups that happened to have Postgres configured (#544).
* **[Runtime]:** Made `SCION_FORCE_HOST_NETWORK` escape hatch runtime-agnostic — works correctly with Docker, Podman, and Apple Container instead of assuming Docker-only semantics.
* **[Hub]:** Stateless Cloud Run broker routing — `deriveCloudRunLogicalBrokerID` now returns errors when project/region is unavailable instead of proceeding with empty values.
* **[Hub]:** IAP audience trailing-slash trimming and `render_settings` escaping fix for deploy scripts.
* **[Hub]:** Postgres migration advisory lock with proper error handling on deferred unlock, preventing concurrent migration races.
* **[CI]:** Resolved `gofmt` and `golangci-lint` failures on main (#538).

## 📖 Docs
* **[Glossary]:** Revised runtime broker glossary with broker taxonomy — node-bound vs proxy, standalone vs embedded — with entries for Hosted Broker, Managed Agent, and cross-references (#539, #540).

## 🔧 Chores
* **[Changelog]:** Merged June 28 entry (#529 follow-up).
