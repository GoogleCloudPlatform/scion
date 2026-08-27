# Phase 1B authorized list/count implementation

Date: 2026-08-24

Implemented the accepted fail-closed authorized list/count design for templates,
harness configs, and groups.

- Added stable `(created,id)` Ent keyset cursors with opaque encoding and
  malformed-cursor coverage.
- Added bounded Hub authorization scanning (50 candidates/batch, 1,000 maximum)
  with exact authorized totals and cancellation propagation.
- Added error-returning `ActionRead` batch preflight and migrated all three
  handlers; unscoped local admins retain direct store list/count behavior.

Verification passed:

- `go test ./pkg/store/entadapter -run 'Test(TemplateAndHarnessConfigListKeysetPagination|ListGroupsKeysetPagination)$' -count=1`
- `go test ./pkg/hub -run '^TestScopedAdminListEndpointsFilterCrossProjectRowsAndCountAuthorizedMatches$' -count=1`
- `go test ./pkg/hub -timeout=600s -count=1` (200.474s)
- `make ci`

Commits: `c5d196a`, `1f79e90`.
