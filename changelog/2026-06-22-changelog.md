# Release Notes (2026-06-22)

Skill publishing was simplified with a new multipart upload path replacing the 3-step signed-URL flow, and Antigravity auth received several fixes for token format handling and keyring capture.

## 🚀 Features
* **[Skill Bank]:** Replaced signed-URL upload with multipart POST for skill version publishing — server now handles hash computation, storage upload, and publishing atomically in a single request. Added `DeleteSkillVersion` store method for cleaning up failed draft uploads. Web UI simplified by removing client-side SHA-256, concurrent upload semaphore, per-file retry logic, and finalize step. Skill create page gained a "Publish first version" toggle for combined create+publish flow (#474).

## 🐛 Fixes
* **[Antigravity]:** Accept nested AGY token format with `auth_method` envelope — validation now handles both flat and nested `{"token": {...}, "auth_method": "..."}` layouts (#471).
* **[Antigravity]:** Capture token from gnome-keyring when AGY doesn't persist the file — `capture_auth.py` falls back to `secret-tool lookup` via saved DBUS session address (#477).
* **[Antigravity]:** Deduplicate capture-auth entries when the same secret key appears in multiple auth types, and treat "already exists" errors as silent skips (#478).
* **[Skill Bank — M5]:** Code review fixes — `cmd.Context()` for Ctrl+C support, case-insensitive URI scheme detection (RFC 3986), pointer fields for partial updates, store constants instead of hardcoded strings, fix for clearing pinned hashes (#470).
