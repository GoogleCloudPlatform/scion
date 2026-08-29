# Phase 2 Frontend View Integration

**Date**: 2026-08-29
**Agent**: dev-phase2-fe-views
**Branch**: scion/ma-ui-em
**Commit**: 10c9d17

## Summary

Integrated messageability indicators, message button gating, rejection error display, and mode-aware tree edges into all agent views. This builds on the Phase 2 foundation work (types, components, helpers) that was committed earlier.

## Changes

### Agent List (agents.ts)
- Added `<scion-messageability-indicator>` to the Messaging column in table view and alongside mode badges in card view
- Indicators only render when `_messageability` is present (graceful degradation)
- Message button gating: hidden for `none`-mode agents, disabled with tooltip showing denial reason for agents where `canMessage === false`, falls back to existing `can(capabilities, 'attach')` check

### Agent Detail (agent-detail.ts)
- New Messaging card in Configuration tab showing mode badge, description, and reachability counts (when `AgentMessageabilityDetail` data is present)
- Header Message button gated same as list view
- Chat composer (`canSend`) disabled when `canMessage === false`
- Warning banner above message viewer when messaging denied (non-none mode)
- Sealed banner for mode=none agents

### Quick Message Dialog (quick-message-dialog.ts)
- Parses structured `message_denied` API error responses
- Displays reason-mapped copy via `getDenialMessage()` instead of generic error
- Falls through to generic `extractApiError` for non-denial errors

### Agent Message Viewer (agent-message-viewer.ts)
- Same structured error handling pattern as quick-message dialog

### Tree View (agent-tree-view.ts)
- Mode-aware edge styling: dashed (5,5) for mode-mismatch, dotted (3,3) red for sealed endpoints, solid primary for branch-branch edges
- SVG `<title>` tooltips on mode-mismatch edges explaining the denial
- New `arrow-sealed` marker definition for sealed edges
- Message button gated per node same as list/detail views

### Icon Registration
- Added `arrow-left-right` to `USED_ICONS` in `copy-shoelace-icons.mjs` (used by messageability indicator)

## Design Decisions
- Kept `getEdgeStyle()` as a module-level pure function rather than a class method since it has no component state dependency
- Edge styling uses `parentId`/`childId` agent lookup per render; acceptable for typical tree sizes
- The `renderReducedReachCallout()` from the brief was not implemented since parent ancestry data is not readily available on the detail page; the primary reachability info comes from `_messageability` API data

## Verification
- TypeScript compiles clean (`tsc --noEmit`)
- Production build succeeds (`npm run build`)
- Prettier formatting applied
