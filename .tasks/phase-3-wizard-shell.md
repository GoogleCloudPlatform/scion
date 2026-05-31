# Phase 3: Wizard Shell (Web UI + Launch Behavior)

**Branch:** workstation-improvements  
**Design docs:** `.design/workstation-onboarding-wizard.md` §3–5, `.design/workstation-onboarding.md` §7  
**Prereq:** Phase 2 complete (`/system/status`, `/system/check`, `/system/runtime`, `/system/init`, `/system/identity` all exist)  
**Commit all changes to the current branch as you go.**

---

## Overview

Build the `/onboarding` route and Lit page with a step-by-step wizard, first-run detection, and auto-open behavior. The image and linked-grove steps are wired up in later phases; this phase builds the wizard shell and first 4 steps.

---

## 3.1 — Route registration

In `web/src/client/main.ts` (routes around line 127-158):
- Add a `/onboarding` route pointing to `scion-page-onboarding`
- First-run redirect: on app load, call `GET /api/v1/system/status`; if `!status.complete`, navigate to `/onboarding` (store result in `sessionStorage` as `onboardingStatus`)

## 3.2 — `web/src/components/pages/onboarding.ts`

Create a new Lit page `scion-page-onboarding`. It implements a linear wizard with these steps:

| # | Step | Key actions |
|---|---|---|
| 0 | **Welcome / Identity** | Display name + email fields (prefilled from `GET /system/status`); `PUT /system/identity` on Next |
| 1 | **System Check** | Call `GET /system/check`; render pass/warn/fail pills; block Next on any "fail" result |
| 2 | **Runtime** | Show detected runtime from `GET /system/runtime`; allow switching; `PUT /system/runtime` on confirm |
| 3 | **Harnesses** | Checkbox list: Claude Code, Gemini, Codex, OpenCode; `POST /system/init` with selected harnesses |
| 4 | **Images** | Placeholder step (wired in Phase 4); show "coming soon" or skip button for now |
| 5 | **First Workspace** | Placeholder step (wired in Phase 5); show skip for now |
| 6 | **Done** | Mark `sessionStorage` `onboardingComplete = true`; "Go to Dashboard" button → navigate to `/` |

State machine:
- Track `currentStep: number` in component state
- Each step has a `validate()` that must pass before advancing
- Steps 4 and 5 are skippable (show "Skip for now" button)
- Resumable: on mount, read `sessionStorage onboardingStatus`; advance past already-complete steps

Styling: use existing Shoelace components (`sl-card`, `sl-button`, `sl-input`, `sl-checkbox`, `sl-progress-bar`). Look at `web/src/components/pages/admin-server-config.ts` and `invite.ts` for patterns.

## 3.3 — Daemon launch behavior (D8)

In `cmd/server_daemon.go` `printWorkstationQuickstart()` (line 361-384):
- Change the URL printed to include `/onboarding` when the machine is un-onboarded
- Print the URL **before** the process backgrounds itself (it currently already does this — verify)
- Add auto-open: after printing the URL, call `openBrowser(url)` if: stdin is a TTY (`term.IsTerminal(os.Stdin.Fd())`), and `SCION_NO_BROWSER` env var is not set
- `openBrowser` uses `exec.Command("open", url)` on macOS, `exec.Command("xdg-open", url)` on Linux, skips on other OS — no-op if command fails

Use `GET /system/status` (or a simpler check: does `~/.scion/settings.yaml` exist?) to decide whether to point to `/onboarding` vs `/`.

---

## Commit Instructions

- `feat: add /onboarding wizard shell with steps 0-3 and done step (W1)`
- `feat: auto-open browser and print onboarding URL on server start (D8)`
- Run `go build ./...` and `go vet ./...` for the Go change before committing
- For the web changes: the project uses Vite/Lit; verify `web/` builds if a build step is available, otherwise at minimum check TypeScript compiles (`cd web && npx tsc --noEmit` or equivalent)
- Do not open PRs — commit directly to `workstation-improvements`
