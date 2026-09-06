# Release Notes (2026-09-04)

P0 fix for GITHUB_TOKEN injection breaking clone-per-agent projects, Cloud Run sandbox UID reverted after gVisor capability discovery, and several missing API routes and action switch cases filled in.

## 🚀 Features
* **Agent secrets server handler (#1467):** Implements `POST /api/v1/agent/secrets`, enabling agents to fetch secrets from the hub at runtime rather than receiving them via command-line arguments.
* **Cloud Run lifecycle testing (#1453):** Adds 785 lines of lifecycle test coverage for Cloud Run runtime instance management — create, start, stop, delete flows with proper cleanup. *(Contributor: Saksham Khandelwal)*

## 🐛 Fixes
* **GITHUB_TOKEN injection mode set to always (#1472):** P0 — GITHUB_TOKEN stored as `as_needed` broke clone-per-agent and worktree-per-agent for PAT-authenticated private repos. Changed injection mode to `always` with data migration for existing projects.
* **Sandbox UID reverted to 0 with CAP_SETUID safety net (#1465):** Cloud Run gVisor sandbox lacks `CAP_SETUID`/`CAP_SETGID`, causing `sciontool init` to fail with EPERM after PR #1439 set UID 1000. Reverted to root with a runtime capability check that skips privilege drops when unavailable.
* **Model alias re-resolution at broker dispatch (#1463):** When the hub dispatched `SCION_MODEL` with an unresolved size alias (e.g. "large"), the broker now detects and re-resolves it using the on-disk harness config.
* **Broker callback route added (#1470):** The Teams broker plugin's `DeliverCallback()` was silently failing — no handler existed for `POST /api/v1/broker/callback`. Added handler with broker HMAC authentication and server-side tests.
* **Message mode action routing (#1469):** Web UI was sending `message_mode` requests to `/actions` (parsed as action="actions", returning 404). Fixed to send action name in the URL path (`/set_message_mode`).
* **Missing action switch cases (#1468):** Added missing cases to agent and project action switch statements for new permission-related actions.
* **Dead GetWSTicket code removed (#1471):** Removed the unused `GetWSTicket()` interface method, its implementation, response type, and mock stubs — no route was ever registered and WebSocket auth uses session cookies.
