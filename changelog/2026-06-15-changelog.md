# Release Notes (2026-06-15)

A settings loading fix resolved a split-brain bug for git projects, the Skill Bank web UI received QA polish, and agent-viz gained Markdown rendering for inter-agent messages.

## 🚀 Features
* **[Agent Viz]:** Render inter-agent comms transcript as Markdown with per-message collapse — messages expand from a one-line plain-text summary to full formatted Markdown on click. Includes security hardening (HTML escaping before `marked.parse()`), lazy parsing for performance, and 1,000-char summary truncation (#427).

## 🐛 Fixes
* **[Config]:** Fixed split-brain settings loading for git projects with `project-id` — in-repo `.scion/settings.yaml` was silently skipped when split storage was configured, causing global settings to override project-level settings. The merge chain is now: defaults → global → in-repo → external → env. Also fixed `scion config dir` to show the effective config directory and added warnings for profiles/runtimes in in-repo settings.
* **[Web — Skill Bank]:** QA fixes — added `storage.googleapis.com` to CSP `connect-src` for GCS uploads, fixed upload retry to use `/upload` endpoint, changed registry create default trust level from `trusted` to `pinned`, fixed case-sensitive `SKILL.md` validation, hid `scopeId` for user-scoped skills, and defaulted version to `1.0.0` (#429).
* **[Runtime]:** Fixed Apple container list JSON parsing for new status format.
* **[Build]:** Split `make all` and `make install` targets so `sudo` doesn't need `go`/`npm` in PATH — workflow is now `make all && sudo make install`.

## 🔧 Chores
* **[Docs]:** Changelog entries for June 10-14 merged to main (#433).
