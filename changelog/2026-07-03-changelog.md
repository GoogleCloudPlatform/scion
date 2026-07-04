# Release Notes (2026-07-03)

A milestone cleanup and expansion day: the entire builtin harness system was deleted (-3486 lines), Slack landed as a full chat integration, the hub gained graceful SSE reconnect for Cloud Run, and Claude's provisioner received OAuth capture and Vertex AI fixes.

## 🚀 Features
* **[Slack]:** Full Slack chat integration as a standalone plugin module — Events API and Socket Mode support, Block Kit formatting, slash command tree (`/scion setup, register, msg, agents, status`), ask-user modal flow, SQLite state store with WAL, hub API client with HMAC signing, per-user registration with code flow (#591).
* **[Slack]:** Slack integration management added to the chat admin UI (#599).
* **[Hub]:** Auto no-auth fallback in pre-dispatch validation — when auth is `auto` and the harness supports `drop-to-shell`, the hub now accepts the agent without credentials instead of rejecting it for missing env vars (#595).
* **[Hub]:** `SCION_IMAGE_REGISTRY` env var takes precedence over `settings.yaml` for image registry resolution (#594).

## 🐛 Fixes
* **[Hub]:** Graceful SSE reconnect before Cloud Run's 3600s hard timeout — server sends a `reconnect` event at 3500s and closes cleanly so the client auto-reconnects per the SSE spec (#590).
* **[Hub]:** Fixed `BootstrapBundledResources` to update existing configs when content changes — split skip logic into `SkipCreate` (respects user deletions) and an always-run update path with content hash comparison (#592, #596).
* **[Claude]:** Fixed Vertex AI auth detection in `provision.py` (#598).
* **[Claude]:** Capture OAuth token (`sk-ant`) from `setup-token` as `CLAUDE_CODE_OAUTH_TOKEN`, restricted to `oat` prefix and most-recent token (#587).
* **[Claude]:** Added `fable` as XL model alias and updated no-auth login command.
* **[Agent]:** Fixed no-auth auto-resolve UI — `HarnessAuth="none"` now propagated back through broker response so the web UI shows the Capture Auth button correctly. Auth method display shows resolved method instead of "container-script" (#586).
* **[Message]:** Closed remaining silent-drop gaps for `--plain`/`--notify`/`--channel` flag combinations with `--in`/`--at` and local mode (#584).
* **[Harness]:** Image status follow-up fixes — error propagation, nil checks, file permissions, atomic write, and `ValidateStorage` hash error handling (#585, #593).
* **[Gemini]:** Set Gemini CLI default model to 3.5 Flash.
* **[Hub]:** Address review comments on `handlers_integrations.go` (#588).
* **[CI]:** Fixed `gofmt` struct field alignment in `types.go` and `models.go` (#589).

## 🗑️ Removals
* **[Harness]:** Removed the entire dead builtin harness system — **-3486 lines**. All harnesses now use container-script provisioning (#600).

## 🔧 Chores
* **[Deps]:** Bumped `golang.org/x/net` in scion-broker-log (#597).
