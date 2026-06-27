# Release Notes (2026-06-26)

A harness-heavy day: the Codex harness received critical auth fixes for file-based credentials, a new GitHub Copilot CLI harness shipped end-to-end tested, OpenCode gained Vertex AI auth support, and several provisioning issues were resolved.

## 🚀 Features
* **[Harness]:** GitHub Copilot CLI harness bundle — complete harness with build integration, provisioner with resilient auth fallback to no-auth mode when hub-registered configs haven't staged auth keys yet, and env var fallback for tokens in container environment (#506).
* **[OpenCode]:** Vertex AI auth support — autodetects GCP project + location env vars and writes `VERTEXAI_PROJECT`/`VERTEX_LOCATION` to `outputs/env.json`. Lowest priority fallback after api-key and auth-file.
* **[Harness]:** Stage `required_files` as secrets instead of bind-mounting — reads file content on host and stages as 0600 secret files under `agent_home/.scion/harness/secrets/`, fixing read-only filesystem crashes when harnesses try to write to credential files (#498).

## 🐛 Fixes
* **[Codex]:** Write fresh writable `auth.json` from staged secret in auth-file mode, fixing `lchown: read-only file system` crash at Codex startup (#501).
* **[Codex]:** Validate `CODEX_AUTH` secret is valid JSON before writing to `~/.codex/auth.json`, surfacing clear errors at provisioning time instead of opaque startup failures (#499).
* **[Codex]:** Updated no-auth hint to suggest `codex login --device-auth` instead of generic message (#504).
* **[Hub]:** Registered missing `/api/v1/message-channels` route that was causing 404s for `--channel` flag in the CLI (#502).
* **[Sciontool]:** Prevent `__pycache__` creation during harness provision by setting `PYTHONDONTWRITEBYTECODE=1`, fixing `scion delete` permission errors from root-owned bytecache on bind-mounted agent home (#505).
* **[Web]:** Move Name field above Workspace Type in new project form (#507).
* **[Web]:** Clean `dist` directory before build to prevent stale chunk accumulation (#508).

## 🔧 Chores
* **[CI]:** Fixed TypeScript errors in metrics-dashboard causing CI failure (#500).
