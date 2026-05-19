# PR #240 Feedback Fixes

**Date:** 2026-05-13
**Branch:** scion/fix-invite-ux
**Task:** Address medium-severity feedback items from upstream PR #240

## Changes Made

### 1. Keyset Pagination Subquery Optimization
**Files:** `pkg/store/sqlite/sqlite.go`

Both `ListAllowListEntries` and `ListAllowListEntriesWithInvites` used inline subqueries to look up the cursor's `created` timestamp, resulting in the subquery executing twice per page load (once for `<` comparison, once for `=` comparison).

**Fix:** Added a separate `QueryRowContext` call to look up the cursor timestamp once before the main query, then pass the resolved `time.Time` value as bind parameters. This reduces the number of subqueries from 2 to 0 per paginated request.

### 2. Missing Index on allow_list(created, id)
**Files:** `pkg/store/sqlite/sqlite.go`

The `ORDER BY a.created DESC, a.id DESC` clause had no supporting index. Added migration V53 to create a composite index `idx_allow_list_created_id` on `(created DESC, id DESC)`.

### 3. Server-Side Invite Expiry Computation
**Files:** `pkg/store/models.go`, `pkg/hub/admin_allow_list.go`, `web/src/components/pages/admin-users.ts`

The frontend was computing invite expiry using `new Date()`, which is unreliable if the user's local clock is skewed. Added an `InviteExpired` boolean field to `AllowListEntryWithInvite`, computed server-side in `handleAdminAllowListGet` before sending the response. The frontend now uses `entry.inviteExpired` instead of comparing timestamps locally.

## Verification
- `go build ./...` — passes
- `go test ./pkg/store/sqlite/ -run AllowList` — all 7 tests pass
- `tsc --noEmit` — TypeScript compiles cleanly
- Rebased onto origin/main — no conflicts
