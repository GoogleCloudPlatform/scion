# pf-1g-dev6: Fix Decide() fail-open gap on delegation ceiling errors

**Date:** 2026-08-25
**Branch:** scion/auth-refactor
**Base commit:** d8e22796

## Problem

In `pkg/hub/authz.go` lines 290-301, when `checkDelegationCeiling` returned a
non-nil error, `Decide()` only denied the request if `isMintingOperation(request.Action)`
was true. This left non-minting, non-read-only actions (ActionDelete, ActionStop,
ActionUpdate, etc.) **allowed on store errors** — a fail-open gap.

The `walkDelegationChain` function in `authz_delegation_ceiling.go` already used
`!isReadOnlyOperation(req.Action)` consistently at every error-handling site, but
the calling code in `Decide()` used the narrower `isMintingOperation` predicate,
creating an inconsistency between the two layers.

## Fix

**One-line change in `pkg/hub/authz.go`:**

```go
// Before (buggy):
if isMintingOperation(request.Action) {

// After (fixed):
if !isReadOnlyOperation(request.Action) {
```

Also updated the comment on the else branch from "For reads, log and allow" to
"For read-only operations (read, list, verify), log and allow" to accurately
describe the predicate's scope.

## Test

Added `TestAuthzDecideFailClosedOnStoreErrorForMutatingActions` in
`pkg/hub/authz_request_test.go`. The test:

1. Creates a `faultyGetUserStore` wrapper that overrides `GetUser` to return a
   non-`ErrNotFound` error for a specific user ID, simulating a genuine store fault.
2. Creates a delegation edge pointing to this faulty user as the delegator.
3. Verifies that `Decide()` denies ActionDelete, ActionStop, and ActionUpdate
   with a "fail-closed" reason.
4. Verifies that ActionRead and ActionList remain allowed (read-only fail-open).

This pins the fix by exercising the exact `ceilingErr != nil` branch in `Decide()`.

## Verification

- `go test ./pkg/hub -timeout=600s -count=1` — PASS (253s)
- `make ci` — PASS
