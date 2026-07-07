# Release Notes (2026-07-06)

A major consolidation day: all harness provisioner scripts were collapsed onto a shared `scion_harness.py` library (-3376 lines), the workstation onboarding wizard was reworked with runtime detection and dir-browser improvements, and the docs-site received a comprehensive refresh.

## 🚀 Features
* **[Onboarding]:** Reworked workstation onboarding wizard — runtime detection with status display, disabled unavailable options, Apple Container prioritized over Podman, in-wizard registry input, dir-browser auto-navigate into new folders, Tab completion with typeahead filter, and auto-fill project name from path (#623).

## 🐛 Fixes
* **[Gemini CLI]:** Require `gcloud-adc` or GCP SA for Vertex AI auth — added `"gemini-cli"` alongside `"gemini"` in all five legacy compiled auth functions so the hub doesn't skip the credential file requirement (#619).
* **[Hub]:** Preserve `updated` timestamp when updating image status fields — prevents all harness-configs from appearing re-written on every startup (#615).
* **[Claude]:** Restored `sk-ant` scrollback extraction to `capture_auth` — the harnesslib consolidation lost the Claude-specific logic that extracts OAuth tokens from tmux scrollback (#621).
* **[Harness]:** Fixed no-auth command injection — replaced double `sh -c` nesting with a raw shell command string embedded directly into `cmdLine`, applied to both Docker/Podman and Kubernetes paths (#618).
* **[Build]:** Use `git reset --hard` instead of `git pull` in rebuild executors so local dirty state never blocks rebuilds, with stderr surfaced in error messages (#614).
* **[Claude]:** Changed no-auth login command to `setup-token`.

## 🔄 Refactor
* **[Harness]:** Consolidated all provisioner scripts onto `scion_harness.py` library — **-3376 lines**. Canonical library at `harnesses/scion_harness.py` with vendored copies generated into each harness bundle, drift-gate test enforcing byte-identical copies, `provisioner.lib` config field (`vendored` | `injected`), and updated staging logic (#613).

## 📖 Docs
* **[Docs Site]:** Complete docs-site refresh on operational-mode axis — fixed JSON-in-YAML settings examples, corrected auth CLI commands (`scion hub auth`/`scion hub token`), corrected Group/Project and Runtime Broker concepts, documented YAML+JSON settings support (#626).
