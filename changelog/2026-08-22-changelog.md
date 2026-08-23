# Release Notes (2026-08-22)

Three P0 permission escalation defects were patched from a permissions consensus advisory, the default harness switched from Claude to Antigravity, and clickable file-path links landed in chat messages.

## ⚠️ BREAKING CHANGES
* **[Config]:** Default harness config switched from `claude` to `antigravity` — new setups will default to the Antigravity harness. Existing installations with explicit `default_harness_config` in their settings are unaffected (#1252).

## 🔒 Security
* **[Hub]:** Patch three P0 permission escalation defects from the permissions consensus advisory — **D6:** empty agent role now resolves to least-privilege (`AgentRoleNone`) across all paths, scheduled dispatch children persist explicit role, one-shot migration applied, empty-creator dispatches refused. **D7:** `requireAdmin` rejects `ScopedUserIdentity` before consulting embedded `Role()`. **D8:** `createGroup` authorized, `project:` slug reserved, slug collision requires system marker + ProjectID, canonical membership lookup. 26 files, +1165/−71 lines (#1250).

## 🚀 Features
* **[Chat]:** Clickable file-path links in chat messages — `/workspace/...` and `/scion-volumes/...` paths render as links that open an on-demand file viewer dialog, fetching files from existing workspace/shared-dir APIs (#1251).

## 🐛 Fixes
* **[Auth]:** Redirect logged-out browser users to login on proxy routes — previously showed a raw JSON 401 error because proxy routes bypass `sessionAuthMiddleware` (which has redirect logic) and reach `UnifiedAuthMiddleware` (JSON-only). Adds browser-navigation check in `sessionToBearerMiddleware` scoped strictly to proxy routes (#1246).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 21 — Includes 22 authz-guard bypass security fixes, fail-closed authorization, hub default model/thinking-level stamping, register_url for Discord and Telegram (#1248).
