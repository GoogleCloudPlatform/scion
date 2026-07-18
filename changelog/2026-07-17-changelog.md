# Release Notes (2026-07-17)

Thinking-level support deepened with CLI flag and env injection, OIDC transport auth shipped for IAP-protected hubs, three GKE dispatch fixes unblocked hosted broker deployments, and harness skill injection defaults were corrected across all harnesses.

## 🚀 Features
* **[Agent]:** Added `--thinking-level` flag to `scion start` (0-100) and `SCION_THINKING_LEVEL` env var injection into agent containers, following the same pattern as `SCION_MODEL` (#794).
* **[Codex]:** Implement `reasoning_effort` from thinking level — maps the 0-100 thinking level to Codex's reasoning effort parameter (#792).
* **[Auth]:** Extracted `pkg/transportauth` for shared OIDC transport auth — pure refactor as Phase 1 of HA Cloud Run OIDC workstream (#790).
* **[Auth]:** Broker OIDC transport auth for IAP-protected hubs (Phase 3) (#791).

## 🐛 Fixes
* **[Broker]:** Three GKE hosted broker dispatch fixes — skip local `ImageExists` check for non-local runtimes (fixes docker.io lookup errors on GKE), return empty chain for missing 'default' template on hosted brokers, and scan all profiles for cloudrun type instead of relying on `active_profile` (#788).
* **[Harness]:** Inverted `include_skills` default to `False` and audited all harnesses — skills are now installed as individual files by the Go provisioner, not concatenated by `project_instructions()` (#784).
* **[Copilot]:** Use relative paths for `config_dir`, `skills_dir`, `instructions_file` — tilde paths produced broken literal paths via `filepath.Join`. Updated authoring guide with warning (#785).
* **[Hermes]:** Added Vertex AI auth support with region fallback chain and cached `_build_vertex_env` result (#786).
* **[Web]:** Register `hammer` and `cloud-download` Shoelace icons (#789).
