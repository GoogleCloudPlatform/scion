# Project Log: #1701 Correction 3 — Available-Slot Open Must Place Agent in Visible Slot

**Date:** 2026-09-21
**Issue:** #1701 (correction 3)
**Commit:** dcb3a79a

## Problem

Live UAT on cumulative 12417749 found that when a four-pane preset has available
(empty) slots and a new agent is opened through the production UI, the agent gets
attached (gen1) but remains hidden — it is not assigned to any slot, and the empty
placeholders remain visible.

## Root Cause

In `terminal-layout.ts`, `open()` had two branches:
1. At capacity → overflow to single (correct)
2. Not at capacity → delegates to `select()` (only sets `single[0]`, never fills
   an empty slot in the active multi preset)

`select()` by design only updates `single[0]` without touching multi-pane
assignments. This is correct for navigation. But `open()` in the not-at-capacity
case needed to additionally place the new agent into the first available (null)
slot of the active multi preset.

## Fix

Added a third branch to `open()`:
- Multi preset with available slot and new session → fill first null slot
- Multi preset with session already assigned → delegate to select (idempotent)
- Single mode → delegate to select (unchanged)

Updated JSDoc for `open()` and `select()` to reflect the changed invariant.

## Fixture Gap Diagnosis

The prior E2E test "open with empty slots does not overflow" only verified that:
- The active preset didn't change to single
- A second socket attach occurred

It did NOT verify:
- That the newly opened agent was visible (not hidden)
- That the agent had a slot index (grid position)
- That the placeholder count decreased
- That the agent's pane was attached in a visible state

This is a **fixture fidelity gap**: the test asserted the *negative* (no overflow)
without asserting the *positive* (agent is visible in a slot). The defect — agent
attached but hidden — passed the negative assertion perfectly because the preset
did stay as `four`.

The updated test now additionally asserts:
- Agent visibility (not hidden, display not none)
- Grid position (has col/row style)
- Placeholder count decreased by 1
- Gen1 (one attach, zero closes)

A new full-flow E2E test exercises the production path with 3 agents in a
four-pane preset, verifying slot 2 placement, visibility, and layout state.

## Files Changed

- `web/src/client/terminal-layout.ts` — open() logic, JSDoc updates
- `web/src/client/terminal-layout.test.ts` — 7 new unit tests, 4 updated
- `web/e2e/terminal-workspace/workspace.pw.ts` — 1 updated E2E test, 1 new E2E test, 1 fixed existing test

## Gates

- `tsc --noEmit` (3 configs): PASS
- `eslint`: PASS
- `prettier --check`: PASS
- `vitest run`: 70/70 terminal-layout tests PASS (4 pre-existing failures in unrelated tests)
- `playwright test workspace.pw.ts`: 80/82 PASS (2 pre-existing failures: grid.svg build output tests)
- `npm run build`: PASS
