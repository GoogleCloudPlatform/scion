# Release Notes (2026-08-07)

The invite-flow refactor completed its third phase with a unified admin user management UI, agent-side secret retrieval shipped with a companion Discord `/scion secret` command, and a cross-user task data leak in the A2A bridge was patched.

## 🚀 Features
* **[Invite Flow]:** Phase 3 — admin UI for unified user model. Replaces the allow-list tab with a merged Users tab showing invited/active/suspended status badges, status filter dropdown, Invite User dialog, bulk import, and context-sensitive row actions per user status (#1060).
* **[Secrets]:** Agent-side secret retrieval API — `GET /api/v1/agents/{id}/secrets/{key}` and `GET /api/v1/agents/{id}/secrets` (list metadata). JWT identity validation, project-scoped access, audit logging, extracted `validateAgentSecretAccess` helper. Includes `sciontool secret get/list` CLI commands for in-container use (#1066).
* **[Discord]:** `/scion secret` slash command with list, set (modal-based secure input), get, and delete operations. Ephemeral responses, HMAC-signed Hub API calls with `X-Scion-On-Behalf-Of` delegation (#1069).

## 🔒 Security
* **[A2A Bridge]:** Fix cross-user task data leak — `ScopedTaskStore` keyed ownership on project:agent route without caller identity, allowing any authenticated user to read other users' tasks under per-user auth schemes. `buildOwnerKey()` now incorporates `CallerIdentity.UserID` (#1065).

## 🐛 Fixes
* **[Hub]:** Persist `message_broker.types` across update/restart/rebuild — `AddPluginToMessageBrokerTypes` was only called during install, so plugins would silently stop routing messages after settings.yaml overwrites. Startup auto-populate replaced with full reconciliation that persists missing entries (#1063).
* **[A2A Bridge]:** Populate `supportedInterfaces` with JSON-RPC transport entry in generated agent cards — fixes "agent card has no supported interfaces" error blocking a2a-go SDK clients (#1064).
* **[Store]:** Resolve advisory lock key collision — `LockGitHubResolutionCacheEviction` and `LockTelegramWebhook` both used key `0x5C10000A`, making them indistinguishable to Postgres (#1067).
* **[Web]:** Allow clearing agent limit fields (max_turns, max_model_calls, max_duration) to null in admin settings — `buildFilePayload()` treated zero/empty as falsy and omitted them (#1070).
* **[Plugins]:** Remove dead `subscribeForProject`/`unsubscribeForProject` code from Discord and Telegram brokers. Downgrade bootstrap subscription timeout from Error to Warn (#1068).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 6 — GKE/GCLB IAP audience validation, Google IAP proxy configuration section in server-config reference (#1062).
