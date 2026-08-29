# Phase 1 — Docs Slice: Port 45 Design-Log Files

**Date:** 2026-08-29

## What Was Done

Ported 45 `.design/project-log/` markdown files from two source branches onto a
new branch (`scion/ca-msg-em6-docs-slice`) based on current upstream/main at
commit `a7ac9c489`.

This is Phase 1 of the em9-unify re-derivation — a docs-only slice with zero
code changes.

## Source Branches and Commits

| Source branch | Commit | File count |
|---|---|---|
| `scion/ca-msg-em9-unify` | `47a7c6736` | 40 |
| `scion/messaging-v2` | `91c9e3146` | 5 |
| **Total** | | **45** |

## File Count Breakdown

- **39 shared** — files present on both em9-unify and messaging-v2 (extracted
  from em9-unify)
- **1 em9-only** — `defect-principalkindFromAddress-fold.md` (exists only on
  em9-unify)
- **5 v2-only** — files that exist only on messaging-v2:
  - `2026-08-27-def26-rename-placeholder-test.md`
  - `def12-cli-command.md`
  - `def12-store-detection.md`
  - `def12-volume-exercise.md`
  - `def25-grove-fixture-rename.md`

## Acceptance Checks — Results

| Check | Result |
|---|---|
| File count (`git diff --stat`) | ✅ 45 files changed, 3339 insertions(+), 0 deletions |
| Additive check (zero deletions on main-existing files) | ✅ Empty output — all 45 files are new |
| `check-security-marker-gates.sh` | ✅ Exit 0 — all gates pass |
| `check-conversation-upsert-guard.sh` | ✅ Exit 0 — no violations |
| `check-authz-guards.sh` | ✅ Exit 0 — no violations |
| `go build ./...` | ✅ Exit 0 |
| Branch pushed to origin | ✅ `scion/ca-msg-em6-docs-slice` |
