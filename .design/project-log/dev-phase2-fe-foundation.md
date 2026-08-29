# Phase 2 Frontend Foundation — Types, Denial Mapping, Indicator Component

**Agent**: dev-phase2-fe-foundation
**Date**: 2026-08-29
**Branch**: `scion/ma-ui-em`
**Commit**: `3938f17`

## Summary

Implemented the foundational frontend pieces for Phase 2 (Reachability Metadata) of the message authorization UI.

## Changes

### 1. Types Extension (`web/src/shared/types.ts`)
- Added `AgentMessageability` interface with `canMessage`, `canReachViewer`, and `reason` fields
- Added `AgentMessageabilityDetail` extending `AgentMessageability` with `reachableAgentCount` and `reachableUserCount`
- Added `_messageability?: AgentMessageability | AgentMessageabilityDetail` field to the `Agent` interface

### 2. Denial Reason Mapping (`web/src/shared/message-mode.ts`)
- Added `MessageDenialReason` union type covering all 6 denial reasons
- Added `DENIAL_REASON_COPY` record mapping reason codes to user-facing copy with `{recipient}` and `{sender}` placeholders
- Added `getDenialMessage()` function for template substitution with fallback to generic message

### 3. Messageability Indicator Component (`web/src/components/shared/messageability-indicator.ts`)
- New `<scion-messageability-indicator>` component following the `<scion-message-mode-badge>` pattern
- 4-state rendering: bidirectional (success arrow-left-right), outbound-only (neutral arrow-right), inbound-only (neutral arrow-left), denied (muted x-circle with reason copy)
- Renders `nothing` when `messageability` is absent (graceful degradation)
- Accessible via `aria-label` and `role="img"` on the icon wrapper
- Size variants: 12px icon for small, 14px for medium

## Verification
- TypeScript compiles clean (`tsc --noEmit` exits 0)
- All changes pushed to `origin/scion/ma-ui-em`
