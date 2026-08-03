# Release Notes (2026-07-27)

A major platform skills and agent infrastructure day: project-level pre-start customization hooks shipped with Ent schema and CRUD API, A2A bridge gained per-user access token auth, six platform skills were added or expanded (scheduler, git-operations, workspace orientation, messaging, recovery, shell safety), and the GitHub skill resolver received auth, caching, and rate-limit fixes.

## 🚀 Features
* **[Agent]:** Project-level pre-start customization hooks — new `ProjectPreStartHook` Ent entity with project-scoped shell scripts attached to the `EventPreStart` hook point, staged as `pre-start.d/30-project-custom` with abort-on-failure wiring (#879).
* **[A2A Bridge]:** Per-user access token auth — `hubUAT` and `hubJWT` auth schemes with `UATValidator` that introspects tokens via Hub `/auth/me` endpoint, SHA-256 keyed cache with configurable TTL, and `CallerIdentity` context propagation (#864).
* **[Skills]:** Scion scheduler skill — covers `scion schedule` command family with decision table for schedule vs inline delays, blocked-wait pairing rule, and lifecycle management patterns (#876).
* **[Skills]:** Git-operations skill — working tree reset safety (`-fd` vs `-fdx`), rebase-after-deletion guidance, separated from CLI-specific and workspace-mode-specific skills (#887).
* **[Skills]:** Mandatory workspace orientation boilerplate — documents `SCION_WORKSPACE_MODE` and `SCION_WORKSPACE_GIT` env vars, per-mode behavior, and shared-directories invariant for all agents (#882).

## 🐛 Fixes
* **[Skills]:** GitHub skill resolver — auth wiring fallback to project-scoped `GITHUB_TOKEN` provision credential, full-SHA short-circuit bypassing API calls, unauthenticated-call warning, and cache init error logging (#878).
* **[Hub]:** Graceful raw/base64 fallback in all four secret-write handlers — fixes silent 400 regression where web UI sent raw text but backend required base64 (#862).
* **[CI]:** `gofmt` formatting in `auth_test.go`, `composite.go`, and `project_pre_start_hook.go` (#889).

## 📖 Docs
* **[Skills]:** Scion messaging skill — code-traced 2000-rune message length limit documentation, inbound message type discrimination, `--notify` deprecation note, sleep anti-pattern scoping (#875, #881).
* **[Skills]:** Agent management — troubleshooting triage table (broker split-brain, signing key, pressure), direct-question obligation in briefing, recovery and shell safety sections, model override guidance (#869, #883, #886).
* **[Skills]:** Scheduler whoami recipe for self-scheduling agents (#880).
