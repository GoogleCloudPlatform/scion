# #1701 Correction 2 — Combined Journey Test Update

**Date:** 2026-09-21
**Commit:** 39408909
**Base:** 50beb1b2 (correction 1)
**Agent:** dev-p3-1701-fix3

## Problem

Review R1 of correction 1 (50beb1b2) found that the combined regression
journey E2E test was not updated for overflow behavior. The test still
expected `activePreset` to remain `'four'` after opening a 5th agent, but
the overflow logic correctly switches to `'single'` when the four-pane
layout is at capacity.

## Changes

**File:** `web/e2e/terminal-workspace/workspace.pw.ts`

1. **Step 3** (combined journey test): Updated assertions to expect
   `activePreset='single'` and `visiblePaneCount=1` after the 5th agent
   opens, reflecting the overflow-to-single behavior.

2. **Step 4**: Removed redundant `clickPreset(page, 'single')` call since
   the test is already in single view after overflow. Updated comment to
   reflect the flow.

## Gate Results

- **Playwright E2E (81 tests):** 79 passed, 2 failed
  - Combined journey test: **PASSED**
  - 2 pre-existing failures: `grid.svg exists in production build output`
    and `grid.svg is served from built output at correct URL` — both require
    a production build artifact directory not present in the test environment.
    These failures are unrelated to this change.
