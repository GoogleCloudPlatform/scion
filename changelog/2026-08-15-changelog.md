# Release Notes (2026-08-15)

A three-part plugin secret management fix chain closed file-config and runtime-activation gaps, a role persistence bug that silently reverted UI-promoted users on every login was resolved, and agents gained self-service notification subscription management.

## 🚀 Features
* **[Web Chat]:** Native chat post-Wave-2 iteration — mobile swipe navigation, attachment handling, DM fixes, markdown rendering, image overlay, iOS fixes, config toggle, and 30+ rounds of UI refinements. 257+ frontend tests passing (#1185).

## 🐛 Fixes
* **[Auth]:** Prevent `admin_emails` from overriding UI-promoted user roles — `determineUserRole()` silently reverted UI-granted role changes on every login/refresh. `admin_emails` now acts as a floor (additive-only), all UI-set roles are preserved, deleted users get 401/403, suspended users get 403, and token refresh reads stored role from DB. 642 of 726 added lines are tests (#1187).
* **[Plugin]:** Three-part plugin secret management fix — (1) extend `migratePluginSecrets` to read from per-plugin config files, not just inline `settings.yaml` entries (#1181); (2) strip secret keys from file-based plugin config via `stripSecretKeysInPlace` helper with copy-before-strip safety and deduplicated warnings (#1183); (3) call migration before stripping in the runtime activation path, closing a gap where boot-time migrated but `activateInstalledIntegration` did not (#1184).
* **[Hub]:** Allow agents to manage their own notification subscriptions — agents can now list, create, update, and delete subscriptions (previously 403). Agent identity qualified by (project, slug) to prevent cross-project data leaks, scope checks gate reads on `project:read` and writes on `project:agent:notify`, and ack requires ownership (#1182).
* **[Web]:** Sync SA list changes to default SA dropdown without page reload — dispatches `sa-list-changed` CustomEvents on register, verify, mint, and delete operations, and clears stale default SA selection when the selected SA is deleted (#1178).

## 📖 Docs
* **[Docs]:** GCP-to-AWS Workload Identity Federation guide — flow diagram, AWS IAM role setup with trust policy, critical gotchas (custom OIDC provider breaks Google federation, `:aud` vs `:oaud` condition keys), three credential approaches, and troubleshooting (#1186).
* **[Docs]:** Nightly documentation update for Aug 13 — native chat Wave 2, DB-backed runtimes/profiles broker dispatch, harness naming corrections (#1180).
