# Project Log: pf-1g-dev5 (Phase 1G Pre-Gate Cleanup)

**Date:** 2026-08-25
**Branch:** `scion/auth-refactor`
**Base commit:** `145444f`

## Summary

Added three non-blocking items from review-arch before the formal gate cycle:

### 1. Cross-project chain denial test

Added `TestDelegationCeiling_CrossProjectChainDenied` in `pkg/hub/delegation_ceiling_test.go`.

This test pins a security property that is currently emergent from scope filtering: a parent agent in project Q cannot confer authority to a child agent in project P when the parent holds no delegation edge in P. The test creates two projects, a user with permissions in both, a parent agent with an edge in Q, and a child agent with an edge pointing to the parent scoped to P. The child is correctly denied because the parent's authority is in Q, not P.

The denial mechanism works at the `checkAgentHoldsPermission` level: when the chain walker checks the parent agent's permissions in scope (project, P), the parent's own project is Q, so the baseline read check fails. This is a valid denial path that produces the same security outcome as the scope-filtered "no delegation edge" path.

### 2. Comment on scopeType hard-coding

Added explanatory comment on the `scopeType := store.RoleScopeProject` line in `checkDelegationCeiling()` in `pkg/hub/authz_delegation_ceiling.go`, explaining that this is deliberate: system-scoped edges could never match, and restricting to project-scoped edges is protective.

### 3. Log line for missing/empty identity scope

Added a `Warn`-level log line in `checkDelegationCeiling()` in `pkg/hub/authz_delegation_ceiling.go` for the case where the identity is not an `AgentIdentity` or `ProjectID()` is empty, leaving `scopeID` empty. This makes the subsequent denial diagnosable while preserving the fail-closed behavior.

## Verification

- `go test ./pkg/hub -timeout=600s -count=1` - PASS (254s)
- `make ci` - PASS

## Files Changed

- `pkg/hub/authz_delegation_ceiling.go` - scopeType comment + missing-identity log line
- `pkg/hub/delegation_ceiling_test.go` - cross-project chain denial test
- `.design/project-log/pf-1g-dev5.md` - this log entry
