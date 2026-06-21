# Release Notes (2026-06-20)

The Antigravity harness was simplified by dropping gnome-keyring in favor of file-based OAuth, the skill creation UX was overhauled with a combined create+publish flow, and the metrics pipeline gained critical fixes for GCP project resolution and hook metric routing.

## 🚀 Features
* **[Antigravity]:** Removed gnome-keyring dependency and switched to file-based OAuth token placement — the token is written directly to `~/.gemini/antigravity-cli/antigravity-oauth-token`, eliminating DBUS/keyring daemon complexity. Dockerfile drops 4 packages (#460).
* **[Antigravity]:** Use official install script for CLI installation (#453).
* **[Skills — Create UX P1]:** Combined create+publish flow on the skill creation page — SKILL.md textarea with auto-populated fields from YAML frontmatter (debounced parsing, manual edit tracking), drag-and-drop file upload with signed-URL pattern and SHA-256 hashing, inline multi-step progress view, per-file status indicators with retry support, and file validation (50 files max, 10MB/file, 50MB total) (#455).
* **[Config]:** Added `name` field to harness-config `config.yaml` so config authors can declare the intended name independently of the directory name. Resolution priority: CLI flag > config.yaml > harness field > URL-derived. Includes path traversal validation and JSON schema pattern constraint (#456).
* **[Web]:** "Capture Auth" button on agent detail page for no-auth running agents — calls `capture_auth.py` inside the container with exit code handling (#459).
* **[Metrics]:** Diagnostic logging when the telemetry pipeline receives data without a cloud exporter (sync.Once warning on first invocation). GCP project ID resolution from metadata server as fallback, fixing the pipeline on instances with working metadata but no explicit credentials. Hook metrics now route through the pipeline OTLP receiver instead of creating per-invocation exporters, preventing Cloud Monitoring sampling rate violations (#458).
* **[Metrics]:** Registered `ModelResponse` hook to enable token metrics from Claude Code (#458).

## 🐛 Fixes
* **[Antigravity]:** Use `TARGETARCH` Docker build arg instead of `uname -m` for correct multi-platform support under cross-compilation (#457).
