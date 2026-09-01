# Release Notes (2026-08-31)

The authorization foundation is refactored from dual Policy+RoleBinding paths to a single RoleBinding-only model with AccessConstraints, Cloud Run sandbox hardened for non-root execution with proper shared volume mounts, and the webchat DDL-block index that broke Init on pre-existing databases is removed.

## ⚠️ BREAKING CHANGES
* **Authorization foundation refactor (#1435):** Replaces the parallel Policy and RoleBinding grant paths with a single positive-authority model. RoleBindings are positive-only grants, AccessConstraints are monotonic restrictions. Includes delegation safety, scope-aware list authorization, RoleBinding-backed project membership, offline recovery, and full legacy Policy removal (58 commits).

## 🚀 Features
* **Antigravity GEMINI_API_KEY auth (#1431):** API-key added as lowest-priority auth method (vertex-ai > oauth-token > api-key), accepting both GEMINI_API_KEY and GOOGLE_API_KEY and auto-setting modelProvider to Gemini in settings.json.

## 🐛 Fixes
* **Webchat DDL-block index removed (#1437):** The UNIQUE index on `webchat_topic.conversation_id` in the Init DDL block broke Init on databases predating that column — the migration to add it was unreachable because Init failed first. Web chat was silently disabled on affected hubs.
* **Cloud Run sandbox non-root execution (#1439):** Sandbox now runs as UID 1000 (scion user) instead of root; Claude Code v2.1.246 rejects `--dangerously-skip-permissions` as root. Agent directories chowned for non-root writability.
* **Shared volume mounts in sandbox (#1438):** Shared dirs now mount at `/scion-volumes/<name>` instead of the host path, matching the k8s runtime convention. Adds InWorkspace and ReadOnly support.
* **Workspace-mode label persistence (#1442):** Clone-per-agent projects no longer report `shared-plain` — the per-agent case was missing from workspace-mode persistence in project create and clone handlers.
* **Default harness fallback (#1444):** Web UI agent creation form fallback changed from gemini-cli to antigravity, matching the backend's embedded default_settings.yaml.
* **Gemini-cli env overlay literals (#1434):** Shell-style `${...}` literals in vertex-ai env overlay replaced with `os.environ.get()` calls — GOOGLE_CLOUD_LOCATION was being set to the literal string `${GOOGLE_CLOUD_REGION}`.
* **Metadata emulator directory listings (#1433):** Added `instance/` and `project/` directory listing handlers; the Node.js gcp-metadata library probes these to detect GCE, and 404s caused ADC detection to fail in Cloud Run sandboxes.
* **GCP token audit log level (#1430):** Successful token generation events demoted from Info to Debug to reduce log noise; failure events remain at Warn.
