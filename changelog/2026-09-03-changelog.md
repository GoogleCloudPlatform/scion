# Release Notes (2026-09-03)

Light day — default model bumped to Gemini 3.8 Flash and a multi-replica cold-start migration race fixed.

## 🚀 Features
* **Default model bumped to Gemini 3.8 Flash (#1460):** Antigravity harness default model upgraded from Gemini 3.7 Flash to 3.8 Flash (config.yaml alias, FLASH_MODEL constant, docs).

## 🐛 Fixes
* **Webchat migration race on multi-replica cold start (#1452):** Two Postgres replicas cold-starting together could race through the migration check-then-mark pattern, causing one replica's Init() to fail with a primary key violation. Fixed with `ON CONFLICT DO NOTHING` on the mark insert and a session-level advisory lock (`pg_try_advisory_lock` on a pinned `sql.Conn`) so only one replica runs migrations.
