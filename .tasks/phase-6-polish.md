# Phase 6: Polish, Tests & Docs

**Branch:** workstation-improvements  
**Design docs:** `.design/workstation-onboarding.md` §7 Phase 6  
**Prereq:** Phases 0–5 complete  
**Commit all changes to the current branch as you go.**

---

## Overview

Tests, doc updates, and any remaining polish to make the feature complete.

---

## 6.1 — Go tests

Write tests for the new Go code. Use the existing test patterns in the codebase (look at `pkg/hub/*_test.go` for handler test patterns).

Key test cases:
- `requireWorkstation` middleware: returns 404 when `Workstation = false`, passes through when `true`
- `assertLoopback`: rejects non-loopback addresses
- `ClassifyPath` (`pkg/hub/fs_safety.go`):
  - Managed path overlap → `IsManaged: true`
  - Already-linked path → `AlreadyLinked: true`
  - Normal path → clean classification
- `ComputeOnboardingStatus`: correctly reports each field
- `PUT /system/identity`: writes config and returns updated identity
- `POST /system/init`: idempotent; seeds only selected harnesses
- `POST /system/fs/validate-path`: returns error JSON on managed-path overlap
- `GET /system/fs/list`: rejects paths outside `$HOME`

## 6.2 — README and docs updates

In the repo README (`README.md`) and any relevant docs:
- Remove or update the "Sadly - not yet able to provide pre-built binaries" note (Homebrew tap exists now)
- Add a "Workstation Quick Start" section: install via brew, run `scion server start`, browser opens to `/onboarding`
- Update the Quick Start to reference the onboarding wizard
- In CLI help / `cmd/server_daemon.go` quickstart output: ensure "developer token" label is consistent

## 6.3 — Any remaining polish

- Verify the first-run redirect works end-to-end (un-initialized machine → `/onboarding`; initialized machine → `/`)
- Verify "Skip for now" on images and workspace steps correctly reaches Done
- Verify the two-step linked-grove create fails gracefully and shows retry UI
- Verify `scion server start` on a machine with no `~/.scion` auto-opens the browser to `/onboarding`
- Verify all fenced endpoints return 404 when `Workstation = false` (simulate prod mode)

---

## Commit Instructions

- `test: add tests for workstation onboarding API and security fencing`
- `docs: update README with workstation quick start and brew install`
- Run `go test ./pkg/hub/... ./pkg/config/... ./pkg/runtime/...` and confirm no failures
- Do not open PRs — commit directly to `workstation-improvements`
