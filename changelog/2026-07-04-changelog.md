# Release Notes (2026-07-04)

A quieter holiday day focused on auth correctness: the hub's credential detection was fixed to evaluate all config-driven auth types, GCP service account assignments are now honored for file-based credential skipping, and several regressions from the builtin harness removal were patched.

## 🐛 Fixes
* **[Hub]:** Fixed `hasRequiredAuthCredentials` to auto-detect auth type before checking requirements — when `HarnessAuth` is empty (auto mode), the hub now iterates all auth types from `authMeta.Types` instead of short-circuiting on the compiled api-key default. Correctly detects file-based credentials like `CODEX_AUTH` and `gcloud-adc` (#605).
* **[Hub]:** Honor `skipped_when_gcp_service_account_assigned` — when a project has a verified GCP service account, file requirements marked with this flag are treated as satisfied, preventing false no-auth fallback (#604).
* **[Hub]:** Accept `projectPath`/`projectSlug` keys in broker `startAgent` handler for backwards compatibility.
* **[Harness]:** Restored missing `StageCaptureAuthAssets` call in `provision.go` — the builtin harness removal (#600) accidentally deleted this line, causing a build error.
* **[Shell]:** Improved bash shebang lines to use `#!/usr/bin/env bash` consistently, avoiding stale bash versions.
