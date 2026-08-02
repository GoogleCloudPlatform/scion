# Release Notes (2026-07-30)

A productive day focused on multi-GitHub credential support, chat integration enhancements, and a new batch skill-import workflow. The Discord bot gained a `/scion thread` command for one-step thread+agent creation, and a critical Vertex AI auth bug was fixed.

## 🚀 Features
* **[Skills]:** Batch-add injected skills from a GitHub directory (Phase 1) — paste a GitHub directory URL to discover skill subdirectories, select via checkbox interstitial, and add all checked skills as individual URI references in a single atomic PUT. Includes a security fix to strip userinfo/credentials from URLs before logging (#914).
* **[Discord]:** `/scion thread` command — create a Discord thread and a Scion agent in one step. Adds `X-Scion-On-Behalf-Of` delegated identity middleware with HMAC-signed header, `CreateAgent` and `ListTemplates` hub client methods, template autocomplete, and full orchestration with in-thread progress feedback (#915).
* **[Discord, Telegram]:** Show build version and git commit hash in `/help` command output via new `pkg/version` package with build-time ldflags injection. Dockerfiles accept `GIT_COMMIT` and `BUILD_VERSION` build ARGs (#916).
* **[Agent]:** Convention-based multi-GitHub credential support — secret key derivation (`GH_OWNER__REPO`, `GH_OWNER`) inserted into `tokenForRef` precedence chain before the default cascade, enabling differently-scoped tokens for multiple repos. Purely additive, backward compatible (#919).

## 🐛 Fixes
* **[Claude Harness]:** Fix `explicit_type` population for auth — `ANTHROPIC_VERTEX_PROJECT_ID` disappeared from the agent container after stop+resume because `explicit_type` was sourced from harness-config metadata (often empty) instead of the resolved auth type. `ApplyAuthSettings` now sources from the `SCION_HARNESS_SELECTED_AUTH` signal with fallback (#921).

## 📖 Docs
* **[Docs]:** Add multi-GitHub credential convention to the user-facing secrets guide — naming convention, normalization rules, setup examples, credential resolution order, and injection mode semantics (#920).
* **[Docs]:** Update stale `SharingModeWorktreePerAgent` doc comment in `pkg/store` — reflects Phase 1 hub-managed and node-local support (#917).

## 🔧 Chores
* **[Skills]:** Platform skill updates — `scion-cli-operations` (remove obsolete rules, add `whoami` tip, shell escape warning), `scion-agent-manage` (add Creating Agents section, hierarchy teardown rule), `scion-messaging` (tone guidance, anti-relay rule, system message format) (#918).
