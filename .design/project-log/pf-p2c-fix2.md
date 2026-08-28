# Fix: Correct Misleading Comment in C2 Combined Roles Test

**Date:** 2026-08-28
**Agent:** dev-pf-p2c-fix2
**Branch:** scion/pf-p2c-scoped-admin-tests
**PR:** #1354

## Summary

Fixed a misleading comment in `TestScopedAdmin_CombinedHubAndProjectRoles` in
`pkg/hub/scoped_admin_test.go`.

The comment said "Create a project and add hub-admin as project-admin there" but
the code sets `OwnerID: hubAdmin.ID` and `CreatedBy: hubAdmin.ID`, making the
user the project **owner**, not a project-admin.

Changed to: "Create a project owned by hub-admin".

## Verification

- `go test ./pkg/hub/ -run TestScopedAdmin_CombinedHubAndProjectRoles -v -count=1` — PASS
