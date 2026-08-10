# Release Notes (2026-08-09)

Federation authentication became runtime-configurable via a new admin UI, the agent role system gained configurable defaults (changed from baseline to full), and three critical fixes resolved OIDC wiring, auth env resolution for the create-and-edit flow, and harness config reimport.

## 🚀 Features
* **[Auth]:** Federation admin UI — runtime management of federation config via opsettings pattern. Issuer CRUD with conditional fields, `atomic.Pointer` hot-reload of `FederationAuthenticator` (lock-free auth path), admin API integration with semantic validation. 19 files, +2266 lines (#1091).
* **[Auth]:** Add `default_agent_role` setting at hub and project level with admin UI dropdown. Change default agent role from `baseline` to `full` for usability. Fix fail-open bugs in role fallback chain (#1090).

## 🐛 Fixes
* **[OIDC]:** Wire federation config end-to-end — federation config wasn't wired into `hub.ServerConfig`, `V1ServerConfig` was missing oidc/federation fields (silently dropped from settings.yaml), and `/.well-known/` discovery endpoints were intercepted by the SPA catch-all in combo mode. Verified end-to-end against deployed hub + bridge (#1093).
* **[Hub]:** Add `GatherEnv` two-pass to `DispatchAgentProvision` — the create-and-edit UI flow only did single-pass env resolution, so `as_needed` keys like `GOOGLE_CLOUD_PROJECT` were never resolved, causing auth provisioning to fail (#1096).
* **[Hub]:** Resolve harness config reimport `git+https://` round-trip bug — `resources/catalog.go` now generates plain `https://` URLs. `NormalizeTemplateSourceURL` and `IsBuiltinManaged` updated to handle legacy `git+https://` URLs already stored in existing databases. Broken for all 7 built-in harness configs on every hub (#1095).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 8 — tiered agent authorization roles, OIDC identity provider, federation authentication, A2A bridge HA, `scion secret` CLI, Restart Hub, and server-config settings. 10 files, 213 insertions (#1092).
