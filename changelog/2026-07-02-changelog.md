# Release Notes (2026-07-02)

A day of polish and depth: harness-config images gained build status tracking with registry probing, the plugin system got hub-mediated install/update flows, skills and templates were factored across repos, and no-auth provisioning learned to auto-fallback gracefully.

## 🚀 Features
* **[Harness]:** Track and display container image status per harness-config — new `image_status` column with local and remote registry checks (including anonymous Docker Hub auth), async refresh on detail page, startup recheck with errgroup concurrency, and list/filter/badge UI (#583).
* **[Harness]:** No-auth auto-fallback and auto-run suggested command — when no credentials are available, provisioning falls back to no-auth mode automatically and surfaces the suggested auth command (#582).
* **[Harness]:** Auto-inject workspace skills during provisioning — skills in the workspace `skills/` directory are automatically made available to agents at provision time (#573).
* **[Chat Admin]:** Hub-mediated plugin updates and first-time install (Phase 4) — `UpdatePlugin` rebuilds from source and restarts, `InstallPlugin` handles first-time build+load, `GET /integrations/available` lists installable plugins, and `FanOutEventBus` gains mutex-protected spoke management for thread-safe dynamic plugin lifecycle (#570).
* **[Web]:** Labels card on agent detail Configuration tab — displays agent labels as `sl-tag` pills, hidden when no labels are set (#581).

## 🐛 Fixes
* **[Capture Auth]:** Propagate exec exit codes and standardize conflict handling — broker now unwraps `exec.ExitError` for real exit codes instead of hardcoding 0. All `capture_auth.py` scripts detect "already exists" and exit with code 3 (`EXIT_CONFLICT`), parsed by a frontend dialog offering Force Update or Cancel (#577).
* **[Build]:** Slimmed Cloud Build source upload from ~2365 files / 38.5 MiB to ~942 files / 12.5 MiB via `.gcloudignore`, with anchored patterns to preserve `image-build/scripts/` and template embeds (#579).
* **[Build]:** Included Dockerfiles in cloud-build context and refocused build pipeline on harness catalog with gemini-cli build step (#576).
* **[Build]:** Run `go mod tidy` before `go build` in plugin install/update to prevent stale module errors (#574).
* **[Hermes]:** Added `--break-system-packages` to pip install for PEP 668 compatibility (#580).
* **[Hermes]:** Installed `python3-pip` in Hermes harness Dockerfile (#578).
* **[Antigravity]:** Bumped AGY_VERSION to 1.0.16 in Dockerfile.
* **[Base]:** Added Playwright CLI to base image.

## 🔄 Refactor
* **[Skills/Templates]:** Factored skills and templates across scion, teamv1, and contrib repos — created `scion-cli-operations` and `git-sandbox` workspace skills, promoted `scion-messaging` and `agent-status-signals` from teamv1, moved fork templates to correct destinations, retired status boilerplate from default `agents.md` (#575).

## 📖 Docs
* **[Glossary]:** Reworked Modes section with availability tiers (single-node hosted vs HA hosted) and tenancy as an orthogonal dimension (#571, #572).
