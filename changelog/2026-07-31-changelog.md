# Release Notes (2026-07-31)

A major feature day: project clone landed alongside a resolved-settings endpoint, skill injection gained scope-based collision resolution, and the Discord bot added a `/scion send` file-retrieval command. The web UI received a sweep of agent interaction improvements — quick-message buttons, terminal shortcuts on graph cards, and restructured server-config settings.

## 🚀 Features
* **[Hub]:** Resolved settings endpoint and project clone — new `GET /projects/{id}/settings/resolved` shows hub defaults per-setting; new `POST /projects/{id}/clone` deep-copies settings, labels, env vars, skills, hooks, harness configs, and templates with defer-driven rollback. Frontend displays hub-default placeholders. Canonical shared harness name list replaces 3 inconsistent hardcoded lists (#922).
* **[Agent]:** Scope-based skill destination-name collision resolution — replaces the hard error in `installResolvedSkills` with a precedence dedup pass (project > template > user > hub > platform). Adds `Scope` field to `SkillReference`, annotated at all injection sites. Collisions logged and recorded in `resolved-skills.json` (#932).
* **[Discord]:** `/scion send <path>` command — send files from the shared scratchpad by absolute path or partial-name search with button picker. Symlink traversal protection and path confinement to `/scion-volumes/` (#931).
* **[Web]:** Restructure admin server-config General tab into 3 cards (General, Agent Defaults with sub-tabs, Project Default Settings). Adds `DefaultModel` and `DefaultThinkingLevel` to hub agent defaults pipeline. Message Broker moved to Hub Server tab, Telemetry toggle moved to Agent Defaults (#930).
* **[Web]:** Quick-message button for agents in detail, list, and graph views — opens a modal dialog (Enter sends, Shift+Enter for newline), capability-gated via existing message check (#928).
* **[Agent]:** Route message attachments through scratchpad shared volume, fixing silent delivery failure in isolated workspace modes (#926).
* **[Hub]:** Auto-provision default scratchpad shared directory for new projects via `project_defaults.default_scratchpad` toggle (default: ON) (#925).
* **[Web]:** Terminal button on agent graph card — icon-only connect-to-terminal shortcut in graph view, gated by attach capability, disabled for offline agents (#924).

## 🐛 Fixes
* **[Web]:** Web skill picker now generates canonical `skill://scion/<slug>` URIs instead of single-segment `skill://slug` that the hub rejects after URI validation landed (#933).
* **[Web]:** Middle-truncate long skill URIs in project view to always preserve the skill name (the identifying part at the end), with full URI on hover (#934).
* **[Web]:** Fix agent label display (remove duplicate from Status tab), add label-filter control to project-level agent list, and fix missing tag icon on the filter (#929).
* **[Web]:** Remove graph-view toggle from projects list page (no graph of projects) and scope agent-detail graph link to the agent's own project (#927).
* **[Hub]:** Accept `project:update` as a valid UAT scope — minting tokens with this scope previously returned a 400 despite enforcement already expecting it (#923).
