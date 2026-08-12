# Release Notes (2026-08-11)

A landmark day for platform integrations: a full Microsoft Teams chat plugin landed alongside sweeping Google Chat enhancements (Pub/Sub ingress, attachments, admin commands), native web chat backend foundations shipped, and GCP service account IAM authorization gates went live with fail-closed actAs checks.

## 🚀 Features
* **[Teams]:** Full Microsoft Teams chat integration plugin — bidirectional messaging broker with Azure AD OAuth2/JWT auth, Adaptive Card formatting, SQLite+PostgreSQL store, bot commands, identity linking, admin web UI with config form and downloadable app manifest, profile identity linking page, and admin setup documentation. ~14K lines across 40 files (#1106, #1123, #1128, #1127).
* **[Google Chat]:** Comprehensive Google Chat plugin enhancements — hub plugin catalog registration with admin UI, thread-level default agent routing, message deduplication with per-space send queue, card-based delete/subscribe flows (replacing dialogs for Pub/Sub compatibility), Cloud Pub/Sub ingress mode for firewall-restricted deployments, bidirectional attachment handling via Chat API media.upload (25 MB limit, path traversal defenses), admin commands (terminal, thread, send, secret), observe mode filtering with outbound mention resolution and settings toggle (#1114, #1115, #1116, #1117, #1118, #1119, #1120, #1121).
* **[Web Chat]:** Native web chat backend foundations — Phase 0 adds channel/thread_id/visibility persistence on Ent messages schema, visibility on OutboundMessageRequest, real keyset cursor pagination, and enriched UserMessageEvent SSE payload. Phase 1 registers the web channel spoke on FanOutEventBus with WebChannelBus handler, WebChatStore with portable SQL, and observer spoke registration (#1137, #1138).
* **[Hub]:** GCP service account IAM authorization gates — Policy Troubleshooter v3 actAs checks for all caller types with fail-closed UNKNOWN handling, cached permission checker (60s allow / 10s deny TTLs), hub-scoped SA mode coupling, passthrough actAs checks on create/PATCH, and project-default SA assignment gating (#1034).
* **[Auth]:** External OIDC login provider support for web UI — adds OIDC Discovery with 1-hour TTL cache and dynamic frontend provider rendering alongside existing Google/GitHub OAuth. Off by default, zero changes to existing flows (#1131).
* **[A2A Bridge]:** Federation auth with OIDC token decoding for bridge-local bookkeeping, CallerIdentity extensions (CallerKey, IsAgent, SenderLabel), transport auth (IAP) wiring, and RFC 7519 aud claim fix accepting both string and array formats (#1133).

## 🔒 Security
* **[Hub]:** Move broker HMAC nonce replay cache from in-memory map to Postgres table with TTL cleanup — at min-instances=2 the per-process cache allowed replay attacks across Cloud Run instances (#1135).

## 🐛 Fixes
* **[Hub]:** Cloud Run multi-instance stability — pin hubID to prevent per-instance key/storage divergence, return 422 for unclassified admin settings instead of silent 200, add advisory locks to three boot migrations that race at min-instances=2, and share OIDC signing keys across instances via database with CAS writes (#1134, #1139).
* **[Teams]:** Handle `28:` prefix Teams adds to bot entity IDs in channel contexts — all slash commands failed silently in channels because the bot @-mention tag was not stripped (#1130). Add registration polling, fix inbound topic from `teams.message` to canonical `scion.project` format, strip thread messageid suffix for channel link lookup (#1129). Use broker endpoint for project listing and require user registration before setup command (#1143). Fix duplicate App Secret field in admin UI by aligning to config key (#1125).
* **[Auth]:** Allow empty clientSecret for public OIDC clients (e.g. Keycloak public clients) — removes startup validation check and only sends client_secret in token exchange when non-empty (#1140).
* **[Hub]:** Resolve project slug to UUID in createAgent following the existing listAgents pattern — unblocks A2A bridge auto-provisioning which uses project slugs (#1132).

## 📖 Docs
* **[Hub]:** GCP service account IAM authorization gates documentation — two-layer SA assignment gate, config fields, hub-scoped SAs, passthrough security, IAM Security Reviewer role requirements, cache behavior, and Web UI integration (#1142).
* **[Ops]:** Cloud Run HA deployment friction fixes — IAP audience format distinction, OIDC/federation sections in settings.yaml examples, no_embed_web build tag, identity-token endpoint audience, agent token scopes format, Cloud Run URL formats (#1124).
* **[Docs]:** Nightly documentation update for Aug 10 — cross-project agent deletion security fix, tiered roles regression fixes, project_defaults opsettings, unified agent creation form, plaintext secret encoding, A2A bridge JSON-RPC corrections (#1113).
