# Release Notes (2026-06-18)

A productive day spanning the harness config lifecycle, template import UX, agent visualization, and build infrastructure. The harness journey P1 landed with source URL tracking and reimport flows, while template imports gained a discovery/selection dialog.

## 🚀 Features
* **[Harness Config — Journey P1]:** Added `source_url` field to track import origin for harness configs and templates. New reimport endpoint (`POST /reimport`) re-imports from stored or overridden source URL. CLI `scion harness-config update` command with `--url` and `--all` flags. Web UI shows source URL as clickable link with a "Refresh from Source" button (#447).
* **[Templates]:** Template discovery and selection dialog for bulk imports — discover endpoints scan for available resources without importing, and when multiple templates are found, a checkbox dialog lets users choose which to import. Single-template sources import directly (#437).
* **[Agent Viz]:** Customizable agent colors via color picker (persisted in localStorage), replay button for seek-to-start, sender-colored comms cards with smarter collapsed summaries that understand structured JSON payloads (#436).
* **[Build]:** Sync built image reference back to Hub after `scion build` — auto-syncs the updated `config.yaml` to Hub with recalculated file manifest and content hash, so the locally-built image is actually used at agent start (#444).
* **[Server]:** Startup warning when the server binary is built without embedded web assets, with a self-contained HTML page served from the static asset handler (#445).

## 🐛 Fixes
* **[Harness]:** Pass Hub-hydrated harness-config path to `harness.Resolve` in both run and provisioning paths — previously Hub-managed configs hydrated into temp directories were invisible, causing fallback to `Generic{}` harness with an empty shell command (#450).
* **[Templates]:** Import progress streaming now works on per-project endpoints (NDJSON support), and single-template imports are correctly scoped to the discovered resource (#443).
* **[Messaging]:** Tightened web channel default to actual web clients only (not CLI or API callers) (#448).
* **[Build]:** Use default Docker builder for local builds instead of custom container builder, fixing intermediate image resolution; use `BUILDX_BUILDER` env var to avoid mutating global Docker config (#442).

## 🔧 Chores
* **[Deps]:** Bumped dompurify (→3.4.9, →3.4.11), js-yaml (→4.2.0), vite (→6.4.3), rclone (→1.74.3), astro (→6.4.8) (#430, #431, #432, #438, #439, #440, #441).
