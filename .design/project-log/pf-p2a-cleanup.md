# PR-A7: Engine Cleanup + Bypass Census Regression

**Date:** 2026-08-28
**Agent:** dev-pf-p2a-cleanup
**Branch:** scion/pf-p2a-cleanup

## Summary

Final Track 1 PR: converted the last 4 `IsUnscopedLocalPlatformAdmin` handler-level bypass sites in engine internals to permission-based checks, deprecated `requireAdmin`, and tightened the bypass census test.

## Changes

### Deliverable 1: capabilities.go — 3 sites converted

Removed the `IsUnscopedLocalPlatformAdmin` admin short-circuits from `ComputeCapabilities`, `ComputeScopeCapabilities`, and `ComputeCapabilitiesBatch`. Super-admins now get all actions via the `CheckAccess` → `Decide` → `checkAccessForUser` step-1 admin bypass. Hub-admins now get correct capabilities from their role bindings instead of zero (previous behavior returned zero for hub-admins because the short-circuit only matched super-admins and the fallthrough was policy-only).

**Notable fix:** `ComputeCapabilitiesBatch` previously used `checkAccessPrecomputed` in its fallthrough loop, which lacked the admin bypass. Changed to use `CheckAccess` per action so the Decide pipeline (including admin bypass) is exercised correctly. The owner/ancestry/project-owner short-circuits are preserved as optimizations before the `CheckAccess` fallthrough.

### Deliverable 2: audit_authz.go — 1 site converted

Replaced the `IsUnscopedLocalPlatformAdmin` check in `handleAuthzExplain` with a `Decide` call for `hub.audit.read` permission (action: `manage`). Using `manage` action instead of `read` ensures the hub-member-read-all seed policy doesn't inadvertently grant explain-for-other capability to all hub members.

Added `hub.audit.read` to the permission registry with `CapabilityNone` (not surfaced in capability projections) and no role binding — only super-admins can explain for other principals, enforced via the step-1 admin bypass in Decide.

### Deliverable 3: Bypass census test tightened

Removed PR-A7 entries (capabilities.go × 3, audit_authz.go × 1) and all PR-A2 through PR-A6 comment placeholders from the allowlist. Added explanatory comment marking remaining entries as engine-internal keeps. Final allowlist contains only:
- Engine-internal keeps (authz.go, authz_candelegate.go, authz_delegation_ceiling.go)
- Authorization infrastructure (authorize.go, route_metadata.go, identity.go)
- AdminModeMiddleware bypass (admin_mode.go)
- Auth/identity infrastructure references (handlers_auth.go, authz_candelegate.go)

### Deliverable 4: requireAdmin deprecated

Added deprecation comment to `requireAdmin` in authorize.go directing new code to use permission-based route metadata. Function retained as routeGuard fallback.

## Tests Added

- `TestComputeCapabilities_HubAdminGetsOnlyPolicyGrantedActions`: hub-admin user gets exactly policy-granted actions (not all, not zero)
- `TestComputeScopeCapabilities_AdminGetsAllScopeActions`: super-admin regression for scope capabilities
- `TestComputeCapabilitiesBatch_AdminGetsAllAfterConversion`: super-admin regression for batch capabilities
- `TestExplainAPI_MemberWithoutAuditReadCannotExplainForOthers`: member denied explain-for-other
- `TestExplainAPI_SuperAdminCanExplainForOthersViaDecide`: super-admin can explain via Decide path

## Verification

`make ci` passes. All existing capability, explain, and bypass census tests continue to pass.
