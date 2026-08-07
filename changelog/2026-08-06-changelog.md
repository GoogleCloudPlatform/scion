# Release Notes (2026-08-06)

The invite-flow refactor shipped its first two phases — `invited` user status merged into the User entity replacing the standalone allow-list, and new admin API endpoints for single and bulk invitations. GKE/GCLB IAP audience support extends HA deployment to GKE.

## 🚀 Features
* **[Invite Flow]:** Add `invited` as a third User status (alongside active/suspended) — merges the allow-list into the User entity, fixes the `invite_only` deadlock. Auth gate `IsUserInvitedOrActive` replaces `IsEmailAllowListed`, with automatic `invited→active` transition on first OAuth login and idempotent data migration (#1054). New admin API endpoints: `POST /api/v1/admin/users/invite` (single, 201/409) and `POST /api/v1/admin/users/invite/bulk` (JSON or CSV, max 1000). Allow-list POST/DELETE/import deprecated as wrappers. Audit events for `user_invited` and `user_invited_bulk` (#1058).
* **[Auth]:** Support GKE/GCLB IAP audiences in HA preflight validation — accepts both Cloud Run and GCLB backend-service audience formats, enabling HA Hub deployment on GKE behind GCLB+IAP. Fail-closed preserved for malformed audiences (#1059).

## 🐛 Fixes
* **[Discord, Telegram]:** Route inbound chat attachments through shared directory infrastructure — fixes attachment delivery in isolated workspace modes with normalized agent path separators (#1055).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 5 changelog — updated Antigravity auth docs to reflect `vertex-ai` as the sole ADC-based path (#1057).
