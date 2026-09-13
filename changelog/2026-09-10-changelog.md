# Release Notes (2026-09-10)

GCP Secret Manager replication location support for compliance-constrained orgs, continued authorization-audit regression fixes, grok-build Vertex AI integration hardened, and template pagination fixed for large hubs.

## 🚀 Features
* **grok-build hook events and quality improvements (#1536):** Wired 4 missing hook events (`PermissionDenied`, `SubagentStart`, `PreCompact`/`PostCompact`), increased stop hook timeout from 10s to 60s, updated model aliases to grok-4.5/grok-4.6, added `--no-ask-user` flag for headless reliability, and enriched session echo payloads.

## 🐛 Fixes
* **GCP Secret Manager user-managed replication (#1533):** The `gcpsm` backend hardcoded automatic/global replication, breaking GCP orgs enforcing `constraints/gcp.resourceLocations`. Adds an optional `ReplicationLocations` config field exposed via `settings.yaml`, the admin settings UI, and the HA postgres config store. Backward-compatible — empty preserves current behavior.
* **Agents listing global templates/harness configs (#1535):** Global-scoped resources are parentless and could not match project-scoped agent bindings in `AuthorizeReadBatch`. Grants `hasAdminView` to agents with `ScopeProjectRead`, routing them to the direct store query. Same 1487-regression family as #1494 and #1502.
* **Harness-config bootstrap in hosted mode (#1525):** New harness configs from binary updates were never seeded on hosted deployments because the bulk `SkipIfAnyExist` guard short-circuited after the first existing config. Removed the bulk skip; per-resource `OverwritePolicy` handles dedup.
* **Template pagination overflow (#1528):** Lowered `limit` from 200 to 100 to stay within `authorizedListMaxPageSize`, added `apiFetchAllPages()` helper with cursor pagination and `MAX_PAGES=50` safety bound, and applied it to template fetches in agent-create, project-settings, and resource-list. Hubs with 100+ templates no longer silently drop entries.
* **grok-build Vertex AI publisher prefix (#1530):** When the broker pre-resolved `SCION_MODEL` from a size alias (e.g. `large` → `grok-4`), the provisioner passed it without the required `xai/` publisher prefix, causing Vertex AI 400 errors. Models without a `/` now fall back to the default vertex model.
* **grok-build global region normalization (#1529):** `GOOGLE_CLOUD_REGION=global` (propagated after #1517) produced the invalid hostname `global-aiplatform.googleapis.com`. Normalizes `global` to empty string, yielding the correct `aiplatform.googleapis.com`.
* **Authorization catalog completeness (#1523):** Added 2 missing `MutationClassification` entries (`ensureHostSARecord:CreateGCPServiceAccount` and `CompleteBrokerJoin:DeleteJoinToken` second call site), fixing the `TestMutationClassificationBidirectional` CI failure on main.

## 📖 Docs
* **Nightly doc update (#1527):** Added `DefaultRuntimeBroker` to admin-settings, full 5-step broker resolution cascade table to multi-broker guide, broker health monitoring and unregistration to runtime-broker guide, and sandbox passthrough-to-assign callout to auth docs.
