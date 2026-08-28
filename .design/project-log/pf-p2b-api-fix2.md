# PR-B3 Upstream Review Fix — Quota API

**Date:** 2026-08-28
**Branch:** `scion/pf-p2b-api`
**Author:** dev-pf-p2b-api-fix2

## Summary

Fixed 1 HIGH and 6 MEDIUM findings from upstream review on the quota API endpoints (PR-B3).

## Changes

### HIGH: quotaService nil panic in getMyUsage
- Added nil guard at the top of `getMyUsage` that returns an empty `myUsageResponse` when `quotaService` is nil, preventing a panic when the store doesn't support quotas.

### MEDIUM-1: authzService nil check
- Added nil guard in `requireWritePermissionForQuota` before calling `s.authzService.Decide()`. Returns 403 Forbidden when authzService is nil.

### MEDIUM-2: Trailing slash on by-ID routes
- Added `strings.TrimSuffix(path, "/")` in `handleAdminLimitByID` before splitting the path. The `handleAdminEntitlementByID` and `handleAdminUsageByLimit` use `extractID()` which already handles trailing content via `strings.Index`.

### MEDIUM-3 & MEDIUM-4: TrimSpace on limit definition names
- Added `strings.TrimSpace(req.Name)` in both `createLimitDefinition` and `updateLimitDefinition` before the empty-name check, so whitespace-only names are properly rejected.

### MEDIUM-5 & MEDIUM-6: SubjectType/SubjectID validation on entitlements
- Added validation for empty `SubjectType` and `SubjectID` in both `createEntitlement` and `updateEntitlement`, returning 400 Bad Request when either is missing.

## Tests Added

- `TestQuotaAPI_UsageMe_NilQuotaService` — verifies empty response (not panic) when quotaService is nil
- `TestQuotaAPI_GetLimitDefinition_TrailingSlash` — verifies trailing slash still resolves
- `TestQuotaAPI_CreateLimitDefinition_WhitespaceOnlyName` — verifies whitespace-only name rejected
- `TestQuotaAPI_UpdateLimitDefinition_WhitespaceOnlyName` — same for update
- `TestQuotaAPI_CreateEntitlement_EmptySubjectType` — verifies empty subject_type rejected
- `TestQuotaAPI_CreateEntitlement_EmptySubjectID` — verifies empty subject_id rejected
- `TestQuotaAPI_UpdateEntitlement_EmptySubjectType` — same for update
- `TestQuotaAPI_UpdateEntitlement_EmptySubjectID` — same for update

## Verification

`make ci` passes with all new and existing tests green.
