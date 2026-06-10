# Release Notes (2026-06-09)

A lighter day focused on messaging improvements and fixing a hub import routing gap. Broker messages now support an interrupt prefix, Telegram formatting was fixed, and missing harness-config import routes were wired up.

## 🚀 Features
* **[Messaging]:** Support `!` prefix in broker messages as inline interrupt — messages from Telegram, webhooks, or direct channels that start with `!` are now delivered with urgent/interrupt semantics, equivalent to `--interrupt` on the CLI. Handles whitespace edge cases and defaults to "interrupt" for bare `!` messages (#375).

## 🐛 Fixes
* **[Messaging]:** Fixed literal `\n` sequences appearing in Telegram message formatting instead of actual newlines (#377).
* **[Hub]:** Registered missing harness-config import routes — the unified `/api/v1/resources/import` endpoint and the per-project `/api/v1/projects/{id}/import-harness-configs` endpoint were never wired up, causing 404 errors on the hub import screen. Added handlers, URL normalization, and proper error code constants (#376).
