# Project Log: dev-phase1-foundation

**Date:** 2026-08-29
**Agent:** dev-phase1-foundation
**Branch:** `scion/dev-phase1-foundation`
**Commit:** `56ff187`

## Summary

Implemented the foundational UI pieces for messaging authorization (Phase 1):

### Deliverable 1: MessageMode Type
- Added `MessageMode` type alias (`'none' | 'lineage' | 'branch' | 'project'`) to `web/src/shared/types.ts`
- Added optional `messageMode?: MessageMode` field to the `Agent` interface

### Deliverable 2: Constants Module
- Created `web/src/shared/message-mode.ts` following the pattern of `agent-state-display.ts`
- Exports `MESSAGE_MODE_DISPLAY` (icon, color, label, description per mode)
- Exports `MODE_SORT_ORDER` (most permissive first: project=0, branch=1, lineage=2, none=3)
- Exports `getMessageModeDisplay()` helper with fallback to `'project'` for undefined modes

### Deliverable 3: Badge Component
- Created `web/src/components/shared/message-mode-badge.ts` following `status-badge.ts` pattern
- Registered as `<scion-message-mode-badge>` custom element
- Supports `mode`, `size` (small/medium), `showLabel`, `showTooltip` properties
- Uses Shoelace CSS variables for theming (success/primary/warning/danger)
- Wraps in `<sl-tooltip>` with mode description when `showTooltip=true`
- Registered in `HTMLElementTagNameMap` for type-safe usage

### Additional
- Registered two new Shoelace icons (`globe2`, `person-lines-fill`) in `copy-shoelace-icons.mjs`

## Verification
- TypeScript type-check passes (`tsc --noEmit`)
- Shoelace icons copied successfully (139 icons)
