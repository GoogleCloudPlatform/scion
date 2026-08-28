# Project Log: pf-p2d-fix2 — Mechanical Cleanup in UAT Enforcement Tests

**Date:** 2026-08-28
**Branch:** `scion/pf-p2d-enforcement`
**File:** `pkg/hub/uat_enforcement_test.go`

## Summary

Applied 7 mechanical fixes from PR#1358 D2 review findings.

## Changes

1. **Replaced manual prefix checks with `strings.HasPrefix` (x2):**
   - `TestUATEnforcement_AgentManageExpansionContents`: replaced `len(scope) > 6 && scope[:6] == "agent:"` with `strings.HasPrefix(scope, "agent:")`
   - `TestValidUATScopes_Completeness`: replaced `len(scope) >= len(prefix) && scope[:len(prefix)] == prefix` with `strings.HasPrefix(scope, prefix)`

2. **Added `"strings"` import** to support the `strings.HasPrefix` calls.

3. **Replaced `NewScopedUserIdentity(nil, ...)` with `makeScopedIdentity(...)` (x5):**
   - `TestEnforceUATConstraints_NewResourceTypes` (2 call sites: scope-present and scope-absent sub-tests)
   - `TestEnforceUATConstraints_BrokerHubLevel`
   - `TestEnforceUATConstraints_UserHubLevel`
   - `TestEnforceUATConstraints_GCPServiceAccountHubLevel`

   The nil-user pattern was not intentional for edge-case testing — these tests only exercise the `enforceUATConstraints` constraint gate which checks scopes and project constraints, not the underlying user identity. Using `makeScopedIdentity` wraps a real member user, which is more realistic.

## Verification

All UAT enforcement tests pass:
```
go test ./pkg/hub/ -run TestUAT -v -count=1        # PASS
go test ./pkg/hub/ -run TestEnforceUATConstraints -v -count=1  # PASS
```
