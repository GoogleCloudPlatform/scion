# Project Log: pf-1e-dev2 — Phase 1E Round 2 Fixes

**Date:** 2026-08-25
**Branch:** scion/auth-refactor
**Commit:** f24c539

## Summary

Fixed Phase 1E review findings on the auth-refactor branch. One required fix and three optional cleanups.

## Changes

### R1 (Required): Backfill pagination — `pkg/hub/seed.go`

`backfillUserRoleBindings` called `ListUsers` with empty `ListOptions`, which the Ent adapter defaults to `limit=50`. Hubs with >50 users would have incomplete role-binding backfill. Same issue in `backfillProjectRoleBindings` (`ListProjects` defaults to `limit=500`) and `backfillProjectAssignPolicies`.

**Fix:** All three functions now paginate using cursor-based iteration:
- `backfillUserRoleBindings`: pages of 200 users
- `backfillProjectRoleBindings`: pages of 500 projects
- `backfillProjectAssignPolicies`: pages of 500 projects

### N1 (Optional): Dead code removal — `pkg/store/entadapter/role_store.go`

Removed `parseUUIDString` — defined but never called. The existing `parseUUID` in `group_store.go` serves the same purpose. Also removed the now-unused `uuid` import.

### O3 (Optional): Test typo — `pkg/hub/role_binding_test.go`

Renamed `TestAuthz_GroupMembershipWithoutRoleBinding_NoBypas` → `TestAuthz_GroupMembershipWithoutRoleBinding_NoBypass`.

### Coverage gap (Optional): Viewer backfill test — `pkg/hub/role_binding_test.go`

Added `TestBackfill_ViewerUserGetsRoleBinding` that creates a user with `Role: "viewer"` and verifies the backfill creates a `hub-viewer` role binding. Follows the pattern of the existing admin and member backfill tests.

## Verification

- `gofmt`: clean on all modified files
- `go build ./...`: passes
- `go vet ./pkg/hub/... ./pkg/store/entadapter/...`: clean
- `go test ./pkg/hub -timeout=600s -count=1`: all role-binding tests pass; 8 pre-existing failures in unrelated areas (skills, templates, workspace)
- `make ci`: passes
- Pushed to `origin/scion/auth-refactor`
