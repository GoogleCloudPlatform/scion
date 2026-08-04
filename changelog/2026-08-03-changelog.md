# Release Notes (2026-08-03)

Project templates landed as a managed hub resource, the A2A bridge admin completed its final two phases (project editor and dev-mode rebuild), and two critical database migration fixes prevented hub crash-loops on upgrade. A major docs catch-up consolidated 15 days of changelog-driven documentation updates.

## 🚀 Features
* **[Hub]:** Project templates as a managed hub resource — full CRUD, SDK, CLI, and web UI for creating and managing project templates at the hub level (#1012). Allow git remote override in project clone so templates carry configuration while users supply their own repository (#1015).
* **[A2A]:** Project/agent exposure editor in admin UI (Phase 4) — structured add/remove/save editor with admin overlay validation tests (#999). Dev-mode rebuild, `make a2a-bridge` target, catalog descriptions, and admin setup documentation (Phase 5, final) (#1000).
* **[Web]:** Horizontal layout option and keyboard shortcuts for the agent graph view — left-to-right layout via `transposeLayout()` helper, segmented toolbar toggle, and updated edge geometry (#1014). Refresh All from Source button on harness configs list — parallel reimport with per-row progress indicators (#1010).
* **[Chat]:** `/scion terminal` command for Discord and Telegram — resolves agent name via Hub API and returns web terminal URL. Adds `HubBaseURL()` to HubClient interface (#1008).

## 🐛 Fixes
* **[Store]:** P0: backfill NULL `scope_id` values in `access_policies` before schema migration enforces NOT NULL — prevents hub crash-loops on upgrade (SQLSTATE 23502) (#1007). Pre-migration dedup for the unique constraint added in #993 — removes duplicate policy rows (keeping oldest) before `AutoMigrate` applies the index (#1001).
* **[Web]:** Fix edge arrowhead geometry and row spacing in agent graph view — stroke no longer pokes past the arrowhead tip, tip aligns with card edge, and arrows stay axis-aligned (#1013). Move auto-expose checkbox to configure-only form, keeping the primary create form simpler (#1011).
* **[Discord]:** Show thread-level default agent in `/scion info` — displays both thread and channel defaults when invoked from a thread (#1009).
* **[Autoexpose]:** Suggest sharing proxy URL with collaborating users in auto-expose notification (#1006).

## 📖 Docs
* **[Docs]:** Documentation catch-up for Jul 19 – Aug 1 changelogs — 13 changelog-driven updates, 5 new pages (Port Forwarding, Pre-Start Hooks, Scheduling, Messaging, Discord setup), significant expansions to External Channels, Skills, CLI Reference, and Lifecycle Hooks. 35 files, ~1645 insertions (#1003).
* **[A2A]:** Consolidate and revamp A2A Protocol Bridge documentation — dedicated 384-line page replacing scattered docs (#1004).
* **[HA]:** Complete rewrite of GCP HA setup guide based on 25-entry friction log — fixes showstoppers, adds Discord deployment, consolidates IAM prerequisites. 394 insertions, 316 deletions (#1005).
