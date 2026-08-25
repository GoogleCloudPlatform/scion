# Release Notes (2026-08-24)

The grok-build harness landed as a new container-script bundle for xAI's grok CLI, Vertex AI support was added via the grok native `auth_provider` mechanism, and a series of harness auth infrastructure fixes closed broker-mode file-secret and auth-type filtering gaps.

## 🚀 Features
* **[Harness]:** New grok-build harness bundle for xAI's grok CLI — `config.yaml`, `provision.py` with auth (API key, auth-file, Vertex AI), 11 grok lifecycle event hooks wired to sciontool, TOML-based MCP config translation (stdio/sse/streamable-http), Dockerfile + cloudbuild.yaml for multi-arch images, web UI + Go embed registration, and 66 unit tests (#1265).
* **[Harness]:** Vertex AI support for grok-build via `auth_provider` — uses `gcloud auth print-access-token` for auto-refreshing GCP tokens, writes `config.toml` with custom model entries pointing at Vertex AI Model Garden, global endpoint default with regional override via `GOOGLE_CLOUD_REGION` (#1277).

## 🐛 Fixes
* **[Harness]:** Three harness auth infrastructure fixes — (1) populate `file_secret_files` from broker-staged file secrets in `auth-candidates.json`, fixing `ProvisionError` for all container-script harnesses using auth-file type in broker mode (#1273); (2) move `SourcePath` clearing before `ValidateAuth` and guard empty paths, fixing silent fallback to no-auth in broker mode (#1279); (3) filter `ResolveAuth` file mappings by selected auth type, preventing `ValidateAuth` from failing on files belonging to unselected auth types (#1280).
* **[Grok-build]:** Iterative harness fixes — handle installer symlinks in Dockerfile by using `cp -fL` to dereference (#1269), remove `GROK_HOME` from `env_template` to fix auth path resolution inside containers (#1270), switch from `-p` (single-prompt) to interactive terminal mode and remove non-functional vertex-ai auth type (#1275), remove stale vertex_ai bundle contract test fixture (#1278).
* **[Hub]:** Stop `syncBuiltImage` from mutating `config.yaml` in storage — the mutation changed `ContentHash`, triggering endless cache invalidation cycles on config updates. Image names now derived from `config.yaml` image field instead of harness slug (#1281).
* **[Hub]:** Accept text files with unusual control characters (e.g. vertical tab `0x0B`) in attachment upload — Go's `http.DetectContentType` treats vertical tab as binary; `isTextContentType` now also accepts `application/octet-stream` when the caller confirmed a text-like extension (#1274).

## 📖 Docs
* **[Harness]:** Document interactive terminal requirement for `command.base` — the Scion runtime manages terminals via PTY and expects tools to stay running (#1276). Clarify `env_template` variables and `{{ .AgentHome }}` host-side semantics with reference table and warning callout (#1271).
* **[Docs]:** Nightly documentation update for Aug 23 — AES-256-GCM secret encryption, user-scoped templates, DM-to-thread promotion (#1267).
