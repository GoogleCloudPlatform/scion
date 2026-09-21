# Project Log: #1701 Correction — Overflow at NEW-OPEN Boundary

**Date**: 2026-09-21
**Commit**: `815f656b`
**Issue**: #1701 (correction to initial fix 3d3167c)

## What changed

The initial #1701 fix removed `active: 'single'` from `select()` to prevent
layout clobbering on agent navigation. However, it also removed the overflow
behavior where opening a 5th agent when all four slots are occupied switches
to single. This correction restores that overflow contract.

### Key design decision

The overflow check belongs in `open()` (new agent creation), NOT in `select()`
(navigation to existing agent). This preserves both behaviors:

1. **Navigation** (`select`) — never changes active preset (core #1701 fix)
2. **New agent** (`open`) — overflows to single when multi preset is at capacity

### Workspace root wiring

- `create()` → `layoutManager.open()` (checks overflow at creation time)
- `select()` → `layoutManager.select()` (never overflows on navigation)

## Files changed

- `web/src/client/terminal-layout.ts` — `open()` overflow logic
- `web/src/client/terminal-workspace-root.ts` — correct method dispatch
- `web/src/client/terminal-layout.test.ts` — 10 new overflow unit tests
- `web/e2e/terminal-workspace/workspace.pw.ts` — 3 focused e2e tests

## Verification

All gates pass. Pre-existing failures in unrelated test files documented in
the full report.
