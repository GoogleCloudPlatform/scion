# Phase 3 Frontend: Mode Change Controls, Cascade Dialog, Mode Filter

**Agent**: dev-phase3-frontend
**Date**: 2026-08-29
**Branch**: `scion/dev-phase3-frontend-work` (based on `scion/ma-ui-em`)

## Summary

Implemented all Phase 3 frontend deliverables for the messaging authorization UI:

1. **Mode Select in Agent Detail** — Replaced the static mode display in `renderMessagingCard()` with an `<sl-select>` dropdown when the viewer has the `set_message_mode` capability. Each option shows the mode icon + label + description. Falls back to the existing static badge display when the capability is absent.

2. **Graduated Confirmation Flows** — Implemented severity-based confirmation for mode changes:
   - Expanding to `project`: no confirmation, applies immediately
   - Restricting to `branch` or `lineage`: medium confirmation via `showConfirm()`
   - Quarantining to `none`: danger-variant confirmation with explicit seal warning
   - Unsealing from `none`: medium confirmation with unseal explanation
   - On cancel, the select reverts to the previous value

3. **Cascade Mode Dialog** (`cascade-mode-dialog.ts`) — New shared component following the `<scion-quick-message-dialog>` pattern. Features:
   - Mode selector for target mode
   - Dry-run preview via `POST /api/v1/agents/{id}/actions?dryRun=true`
   - Scrollable list of affected agents with current → new mode transitions
   - Flags for noteworthy transitions (unseal, seal)
   - Impact summary with agent count
   - Danger styling when target mode is `none`
   - Graceful fallback when preview endpoint is unavailable

4. **Cascade Button in Agent Detail** — "Apply to entire branch" button shown when `set_message_mode` capability is present AND agent has children. Opens the cascade dialog; on `cascade-applied` event, refreshes the agent detail.

5. **Mode Filter in Agent List** — Added `<sl-dropdown>` filter to `renderFilterBar()` with:
   - All Modes (default)
   - Individual modes: Project, Branch, Lineage, Sealed (with icons)
   - Reachability: Can message, Cannot message
   - Persisted to `localStorage` as `scion-filter-agents-mode`
   - Applied in the `displayAgents` getter alongside the existing `phaseFilter`

6. **Cascade Types** — Added `CascadeAgentDetail` and `CascadePreview` interfaces to `types.ts`.

## Files Changed

- `web/src/shared/types.ts` — Added cascade types
- `web/src/shared/message-mode.ts` — No changes (used existing exports)
- `web/src/components/pages/agent-detail.ts` — Mode select, confirmation flows, cascade wiring
- `web/src/components/pages/agents.ts` — Mode filter dropdown
- `web/src/components/shared/cascade-mode-dialog.ts` — New component

## Verification

- `npx tsc --noEmit` passes with zero errors
- All existing patterns followed (showConfirm, showToast, apiFetch, capabilities gating)
