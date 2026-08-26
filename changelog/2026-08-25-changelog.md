# Release Notes (2026-08-25)

Helm chart Phase 1 shipped with settings.yaml rendering and credential guards, structured ExitCode/ExitReason fields replaced regex-parsed container status, and the auth capture journey was completed with user-scope and progeny defaults.

## 🚀 Features
* **[Helm]:** GKE Helm chart Phase 1 — renders the hub's `settings.yaml` and mounts it read-only, wires credential guards through shared helpers so `extraEnv` can't smuggle secrets, adds `hub.baseUrl` as a required HTTPS-only field (chart refuses to render without it). 278 test assertions, kubeconform validated, 10 review rounds (#1283).
* **[Hub]:** Structured ExitCode and ExitReason fields for agent crash detection — replaces regex-parsed `ContainerStatus` prose with DB-persisted structured fields. All runtimes populate natively, hub uses structured path with legacy fallback for old brokers. Migrates 22+ string-matching call sites (#1294).
* **[Hub]:** DefaultHarnessAuth setting at hub and project level — soft-default auth type for new agents following the established `DefaultHarnessConfig` pattern. Precedence: per-agent > project > hub. Backend (opsettings, project annotations, apply-defaults), frontend (admin + project dropdowns), comprehensive precedence tests (#1293).
* **[Auth]:** Complete auth capture journey with user-scope and progeny — adds `--allow-progeny` flag to `sciontool secret set`, all capture paths default to user scope with progeny enabled. Updates `scion_harness.py` and all generated copies (#1295).
* **[Build]:** Add grok-build harness to aggregate Cloud Build configs — inserts build step into all four static aggregate YAML files (#1290).

## 🐛 Fixes
* **[Hub]:** Make `hasAnyKey` progeny-aware for auth credential detection — progeny agents' `OwnerID` is the creating agent (not the user), so user-scoped secrets were never found, causing premature NoAuth fallback that gated off `resolveSecrets()` entirely. Now falls back to `ListProgenySecrets()`/`ListProgenyEnvVars()` (#1292).
* **[Grok-build]:** Iterative Vertex AI and harness fixes — set `GROK_DEFAULT_MODEL` env var and resolve model aliases (#1286), detect vertex-ai auth from GCP identity `SCION_METADATA_PROJECT_ID` (#1288), add `skipped_when_gcp_service_account_assigned` to gcloud-adc for vertex-ai (#1289), fix instructions path (`AGENTS.md` → `.grok/AGENTS.md`) and add native system prompt via `--system-prompt-override` (#1291).
* **[Web]:** Reflow hard-wrapped agent text in chat messages — removes `breaks: true` from `marked.parse()` so single newlines become spaces (standard markdown behavior) instead of line breaks (#1284). Fix broken build from `marked.parseSync()` (nonexistent method) by reverting to `marked.parse(markdown, { async: false })` (#1287).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 24 — grok-build harness with Vertex AI, auth_provider mechanism, interactive terminal requirement, file_secret_files, attachment control chars (#1282).
