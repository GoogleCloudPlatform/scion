# Release Notes (2026-08-10)

A critical security fix closed a cross-project agent deletion vulnerability, two follow-up fixes resolved production regressions from the tiered agent roles rollout, and the agent creation form was unified into a single page with advanced settings disclosure.

## 🚀 Features
* **[Web]:** Unified agent creation form with advanced settings disclosure — merges the two-phase create/configure flow into a single form. Default section shows Name, Project, Template, Harness, Broker, Profile, Task, and Notify. Collapsible Advanced Settings area with 5 tabs (General, Auth & Security, Environment & Labels, Limits & Resources, Prompts). Removes the `editingAgentId` round-trip flow (#1107).
* **[Web]:** Agent role badge and expanded GCP identity card on agent detail page — color-coded role badge (full/baseline/readonly/none) on the Identity card, GCP Identity card shows service account email and project for all identity modes (#1104).

## 🔒 Security
* **[Hub]:** Gate agent DELETE with lifecycle scope and project isolation — `performAgentDelete` only authorized user callers; agent-authenticated callers silently bypassed the check, allowing any valid agent JWT from any project to delete any agent hub-wide. Agent callers now require `ScopeAgentLifecycle` and must target agents in their own project (#1097).

## 🐛 Fixes
* **[Auth]:** Re-derive scopes from stored role on token refresh — fixes production regression where existing agents got 403 on all read endpoints after tiered roles merged. `RefreshAgentToken` copied old JWT scopes verbatim; legacy agents never acquired `ScopeProjectRead` (#1101). Default pre-role agents to `full` in `agentRoleAndScopes` — standing agents lost create/lifecycle/secret scopes after token refresh due to hardcoded `baseline` default (#1102).
* **[Hub]:** Add `settings.yaml` support for `project_defaults` opsettings section — bridges the file/SQLite gap with KoanfPaths registration, Layer1Snapshot wiring, and file-mode fallback (#1103).
* **[CLI]:** Send `encoding: raw` for plaintext secret values in `scion hub secret set` — server previously attempted base64-decode on raw CLI args (#1111).
* **[Web]:** Add missing `globe` icon to Shoelace icon copy script for federation admin page (#1105).
* **[Autoexpose]:** Suggest sharing proxy URL with collaborating users in notification message (#1110).

## 📖 Docs
* **[A2A Bridge]:** Fix JSON-RPC method names (PascalCase, not REST-style slash names) and Hub `settings.yaml` schema (plugin entry path, `self_managed: true` field) in README (#1112).
* **[Docs]:** Nightly documentation update for Aug 9 — federation admin UI, default_agent_role setting, OIDC wiring, server-config fields (#1099).

## 🔧 Chores
* **[CLI]:** Declutter subcommand help — move global flags to `scion help global-flags` topic, hide rclone-leaked flags (#1109).
* **[Harness]:** Bump antigravity CLI from 1.1.10 to 1.1.11 (#1108).
