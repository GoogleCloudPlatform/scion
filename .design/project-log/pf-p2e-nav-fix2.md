# Project Log: pf-p2e-nav-fix2

**Date:** 2026-08-28
**Branch:** `scion/dev-pf-p2e-nav-tokens`
**Commit:** `50ef66e2`
**PR:** #1355 (E3 review findings)

## Summary

Applied 4 fixes from the E3 review of the nav and token-list components.

## Changes

### Fix 1: Race condition in admin capabilities probe (`nav.ts`)

Added a stale-result guard to `checkAdminCapabilities()`. After the async
`apiFetch('/api/v1/admin/roles')` returns, the code now checks whether
`adminCheckUserId` still matches the userId captured before the fetch. If the
user changed while the probe was in flight, the stale result is discarded.

### Fix 2: Missing `expandsTo` on `agent:manage` alias (`token-list.ts`)

The `agent:manage` entry in `FALLBACK_SCOPES` had `isAlias: true` but no
`expandsTo` field. Added the canonical set of 6 agent UAT scopes from
`permissions.UATManageScopes()` in the Go registry:
`agent:attach`, `agent:create`, `agent:delete`, `agent:list`,
`agent:port_access`, `agent:read`.

### Fix 3: Missing `aria-label` on scope filter input (`token-list.ts`)

Added `aria-label="Filter scopes"` to the `<sl-input>` used for scope
filtering in the create-token dialog.

### Fix 4: Missing `aria-expanded` on scope group header (`token-list.ts`)

Added `aria-expanded=${!isCollapsed}` to the scope group header `<div>` that
already had `role="button"` and `tabindex="0"`.

## Verification

- `npm run build` passes cleanly.
- `npm run lint` shows only pre-existing warnings/errors unrelated to these changes.
