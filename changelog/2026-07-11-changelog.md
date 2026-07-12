# Release Notes (2026-07-11)

A harness provisioning fix day: Claude and Gemini CLI both received critical fixes for instruction injection and workspace trust, broker registration gained deployment-type labels, and auth token handling was corrected.

## 🚀 Features
* **[Broker]:** Deployment-type labels added to broker registration — co-located and external brokers are now labeled at registration time with deployment context, with tests for label propagation (#673).

## 🐛 Fixes
* **[Claude]:** Fixed workspace trust dialog for git-clone-per-agent — pre-trust `/workspace` alongside the agent workspace when they differ, and suppress release-notes/onboarding dialogs by setting version markers at provision time. Also added missing `project_instructions()` call so template instructions and skills are written to `CLAUDE.md` (#677, #678).
* **[Gemini CLI]:** Fixed system instruction provisioning — `provision.py` now reads staged `system-prompt.md` and writes it to `.gemini/system_prompt.md`, and calls `project_instructions()` for agent instructions and skills (#676).
* **[Auth]:** Honor `SCION_HUB_TOKEN` in `getHubAccessToken` — the env var was being ignored in favor of stored credentials (#674).
* **[CLI]:** Auto-query Hub for harness-config list in agent containers — detects hub-connected environment via `SCION_HUB_ENDPOINT`/`SCION_HUB_URL` and includes Hub configs without requiring `--hub` flag (#675).
* **[Build]:** Copy `proto/` into scion-base builder stage to fix gRPC compilation (#680).
