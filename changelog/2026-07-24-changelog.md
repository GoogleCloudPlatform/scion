# Release Notes (2026-07-24)

Private-repo skill resolution shipped with `gh://` URI support and per-URI credential selection, skill URI input validation and auto-transform landed across hub/CLI/web, model alias resolution was fixed to resolve before storage, and a silent notification loss bug was closed.

## 🚀 Features
* **[Skills]:** `gh://` private-repo skill resolution via project git credentials — injects GitHub token into skill resolver, adds `ProvisionCredentials` channel and `?token=SECRET_NAME` query param for per-URI credential selection, with cache authorization validation (#859).
* **[Skills]:** Skill URI input validation and auto-transform — `NormalizeSkillURI` converts GitHub tree/blob URLs to canonical `gh://` form, rejects `scion://` with clear errors, validates `gh://` shorthand structure. Applied at hub (422 on invalid) and CLI (stderr notice on auto-convert) (#866).

## 🐛 Fixes
* **[Hub]:** Resolve model aliases before storing in `AppliedConfig` and `SCION_MODEL` — aliases were stored raw, causing harness provision scripts to receive unresolved tier names (#857).
* **[Hub]:** Don't silently mark notification dispatched when agent has no `RuntimeBrokerID` — was permanently losing notifications; now leaves them undelivered for future retry. Same fix applied to nil-dispatcher path (#861).
* **[Image]:** Force fresh `@anthropic-ai/claude-code` install with `@latest` tag and npm cache clear — stale packument from core-base was resolving to a month-old version (#867).
* **[Hermes]:** Remove nodesource apt repo after nodejs install to fix arm64 QEMU build — stale InRelease file caused "Cannot allocate memory" in emulated builds (#868).
* **[CLI]:** Use `skill://` instead of `scion://` in help text examples — `scion://` has no registered resolver (#863).

## 📖 Docs
* **[Docs]:** GCP setup tutorial — end-to-end Cloud Run Hub + Discord + GKE Autopilot deployment guide covering infrastructure, IAM, dispatch verification, and maintenance (#858).

## 🔧 Chores
* **[Harness]:** Update claude model config and gemini-cli medium model alias to gemini-3.6-flash.
