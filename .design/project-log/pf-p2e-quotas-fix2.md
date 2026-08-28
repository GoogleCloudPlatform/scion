# Project Log: pf-p2e-quotas-fix2

**Date:** 2026-08-28
**Branch:** `scion/pf-p2e-quotas-ui`
**File:** `web/src/components/pages/admin-quotas.ts`

## Summary

Applied two E2 review fixes to the admin quotas UI component.

## Changes

### Fix 1: Handle non-ok API responses in `loadEntitlements()`

The `loadEntitlements()` function silently ignored HTTP error responses from
the entitlements and usage endpoints. While the `entitlementsError` state
existed, it was only populated in the `catch` block (for exceptions), not
for non-ok HTTP responses.

Added `else` branches that call `extractApiError()` to surface the error
to the user. The usage error only sets `entitlementsError` if no
entitlements error was already captured, avoiding overwriting a more
relevant error message.

### Fix 2: Remove redundant `res.status !== 204` check in delete handler

The delete handler's error check was `!res.ok && res.status !== 204`. Since
HTTP 204 is a 2xx status code, `res.ok` is already `true` for 204 responses,
making the second condition redundant. Simplified to `!res.ok`.

## Verification

- `npm run build` passes
- `npm run lint` shows no new errors (all existing warnings/errors are pre-existing)
