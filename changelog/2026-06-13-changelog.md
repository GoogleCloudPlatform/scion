# Release Notes (2026-06-13)

A2A multi-turn conversations shipped as a series of commits, transforming the bridge from single-turn MVP to full multi-turn lifecycle support. Alongside that, secrets handling was fixed for double-encoding, the project detail agent list gained filtering, and several CI/build issues were resolved.

## 🚀 Features
* **[A2A Bridge — Multi-Turn Lifecycle]:** Tasks no longer auto-close on the first content message. Content messages are now broadcast with `state=working` and `Final=false`, keeping the task alive. Task lifecycle is driven solely by agent state-change messages: `working/thinking/executing` → working, `waiting_for_input` → input-required, `completed/error/stalled` → terminal states. This enables agents to ask clarifying questions, send progress updates, and emit interim artifacts before completing.
* **[A2A Bridge — Follow-Up Messages]:** `message/send` with a `taskID` routes to the existing agent, continuing the conversation. Verifies task ownership, rejects terminal-state tasks, and works with both blocking and non-blocking modes.
* **[A2A Bridge — Capability Advertisement]:** Agent cards now advertise `streaming=true` and `pushNotifications=true`, reflecting the implemented multi-turn support.
* **[Hub]:** Restored harness-config build UI and executor that was accidentally removed by PR #412 — includes the build image button, dialog, log streaming, and seeded operation (#420).
* **[UI]:** Added filter and sort controls to the project detail agent list (#414).

## 🐛 Fixes
* **[Secrets]:** Fixed secret API base64 handling — store decoded plaintext instead of base64-encoded values, preventing double-encoding when secrets are injected as environment variables. Added 128KB `MaxBytesReader` limits and frontend-side base64 encoding via `TextEncoder` (#418).
* **[A2A Bridge]:** Preserve `input-required` state on content messages instead of unconditionally resetting to `working`. Use `projectcompat` topic helpers instead of hardcoded patterns. Added `TouchTask` store method for timestamp refresh (#421).
* **[Ent]:** Regenerated ent client to remove stale `discordpendinglink` import (#419).
* **[Auth]:** Stricter email validation using `net/mail.ParseAddress` (#411).
* **[Hub]:** Fixed duplicate `no_auth` keys and missing field schema attribute (#412).
* **[Skill Bank]:** Fixed SQLite pin compatibility and skill name validation (#415).

## 🔒 Security
* **[Runtime]:** Protected metadata server shutdown endpoint from unauthorized access (#422).

## 🔧 Chores
* **[CI]:** Added esbuild as explicit dev dependency for Vite 7 compatibility (#416).
* **[Build]:** Bumped esbuild and vite (→ v8.0.16) in web frontend (#413).
* **[Style]:** Applied `gofmt` to all unformatted Go source files (#417).
