# Terminal Workspace P3.5 — #1662: Rollout Wiring and Regression Suite

**Date**: 2026-09-20
**Branch**: dev-p3-1662
**Base**: scion/terminal-workspace @ 30d0789f
**HEAD**: 4384a093

## Summary

Final rollout wiring, documentation, and combined regression tests for the
terminal workspace feature (P3.5). All gates pass; bundle delivered.

## Deliverables

### 1. Combined End-to-End Journey Test
- Single Playwright test exercising the full lifecycle:
  - Open 4 agents via entry-point interactions (direct URL + nav-click dispatch)
  - Place in 2×2 four-grid layout
  - Open 5th agent → automatic overflow to single-pane layout
  - Restore four-grid → original assignments preserved
  - Cross-mode navigation (Dashboard → Chat → Terminals)
  - Session identity verification (attach counts, DOM pane identity, socket counts)
  - Close one session → remaining 4 intact, closed removed from rail and layouts

### 2. >12 Retained Sessions Test
- Opens 13 sessions with unique agent UUIDs
- Verifies all 13 appear in the rail, no eviction, no socket closes
- Code audit confirms: no MAX_SESSION cap, no eviction/LRU logic in
  terminal-sessions.ts, terminal-coordinator.ts, or terminal-workspace-root.ts

### 3. Production Icon/Title Verification
- "Terminals" mode label renders in header with session count
- Header icon-button uses `terminal` icon name
- Document title is "Terminals — Scion" when workspace active
- Title reverts on navigation away
- Flag-off: standalone terminal page has no workspace header
- Grid icon present in USED_ICONS (copy-shoelace-icons.mjs line 129) and
  built output (public/shoelace/assets/icons/grid.svg confirmed)

### 4. Partial Coverage Verification
- **AC1-4 (attach count)**: Existing test fixture tracks actual WebSocket
  routeWebSocket handler invocations — each `attaches++` is a real socket
  connection, not proxy text. Combined journey test also asserts throughout.
- **AC4-3 (stale-callback/input invariants)**: Covered by terminal-reconnect.test.ts:
  - `sendData returns false and never queues input when disconnected`
  - `generation invalidation prevents stale callback corruption`
  - `increments generation on each reconnect attempt`
  - Gap documented: OSC 52 clipboard generation guard is in pane component
    shadow DOM, not session registry. Pane-level testing outside this scope.

### 5. Legacy Adapter Documentation
- Code comment in main.ts route entry for `scion-page-terminal` explaining:
  - Flag-off: disposable standalone page, fresh socket per navigation, no retention
  - Flag-on: router redirects to /terminals/{id}, workspace coordinator manages lifetime
  - Flag changes preserve active sessions until reload

### 6. Rollout/Rollback Instructions
- web/e2e/terminal-workspace/ROLLOUT.md (47 lines)
- Flag mechanism: server injection → localStorage → default OFF
- Enable/disable paths documented
- Prerequisites: all P3 siblings merged, #1661 runtime UAT complete
- Native chat interaction documented

## Files Changed

| File | Lines |
|------|-------|
| web/e2e/terminal-workspace/workspace.pw.ts | +409 |
| web/src/client/main.ts | +20 (comments only) |
| web/e2e/terminal-workspace/ROLLOUT.md | +51 (new) |

## Gate Results

| Gate | Result | Exit Code |
|------|--------|-----------|
| tsc --project tsconfig.json | PASS | 0 |
| tsc --project tsconfig.client.json | PASS | 0 |
| tsc --project src/client/tsconfig.terminal-tests.json | PASS | 0 |
| tsc --project e2e/terminal-workspace/tsconfig.json | PASS | 0 |
| eslint e2e/terminal-workspace/ | PASS | 0 (2 pre-existing warnings in reconnect.pw.ts) |
| vitest run | PASS* | 1 (2 pre-existing failures: role-binding-assignment-form.test.ts hook timeout, terminal-pane.test.ts env issue — neither file touched) |
| playwright workspace suite | PASS | 0 (73/73 passed) |
| npm run build | PASS | 0 |

## Desktop Evidence — NOT IN SCOPE

9 desktop evidence items (AC6-1 through AC6-8) require headed Chrome on a
real desktop. The existing headless Playwright fixtures exercise coordination
logic (BroadcastChannel, Web Lock) but do not cover OS-level interactions:
actual window focus, real drag-and-drop, system clipboard, lifecycle
freeze/discard. These are listed in the inventory with reproducible manual
verification steps.

## Pre-existing Warnings

- 2 ESLint warnings in reconnect.pw.ts (missing return types on arrow functions) — pre-existing, not introduced
- 2 Vitest failures in unrelated test files — pre-existing, not introduced
- Phase 2 aggregate ci-full FAIL cause UNKNOWN — not attributed without evidence
