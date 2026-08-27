# PR-A3: User Management Handler Conversion

**Date:** 2026-08-27
**Agent:** dev-pf-p2a-usermgmt
**Branch:** scion/pf-p2a-usermgmt

## Summary

Converted 7 user management handler bypass sites from inline `Role() != "admin"`
checks to permission-based checks via route metadata. This is the third PR in the
D4 permission conversion series (PR-A3), following the same mechanical pattern
established by PR-A1 (infrastructure) and PR-A2 (settings).

## Changes

### Route metadata (`route_metadata.go`)
Updated 7 `RouteHubAdmin` entries to declare Permission/Resource/Action:

| Route Pattern | Permission | Resource | Action |
|---|---|---|---|
| `/api/v1/admin/allow-list` | `hub.allow_list.update` | `hub` | `update` |
| `/api/v1/admin/allow-list/` | `hub.allow_list.update` | `hub` | `update` |
| `/api/v1/admin/users/invite/bulk` | `user.invite` | `user` | `invite` |
| `/api/v1/admin/users/invite` | `user.invite` | `user` | `invite` |
| `/api/v1/admin/invites` | `user.invite` | `user` | `invite` |
| `/api/v1/admin/invites/` | `user.invite` | `user` | `invite` |
| `/api/v1/admin/validate-resources` | `hub.validate.execute` | `hub` | `execute` |

### Handler conversions (4 files, 7 bypass sites)
- `admin_allow_list.go`: Removed inline admin checks from `handleAdminAllowList` and `handleAdminAllowListByEmail`. Kept `user` variable (used downstream).
- `admin_invites.go`: Removed inline admin checks from `handleAdminInvites` and `handleAdminInviteByID`. Kept `user` variable (used downstream).
- `admin_user_invite.go`: Removed inline admin checks from `handleAdminUserInvite` and `handleAdminUserInviteBulk`. Kept `user` variable (used downstream).
- `admin_validate.go`: Removed inline admin check and `user` variable from `handleAdminValidateResources` (not used downstream).

### Bypass census (`bypass_census_test.go`)
Removed 7 PR-A3 allowlist entries. All permissions were already registered in
the permission registry (`permissions/registry.go`) and included in the
hub-admin role seed (`seed.go`).

### Tests (`usermgmt_permission_test.go`)
Added `TestUserMgmtPermissionConversion` covering:
- Route metadata verification for all 7 endpoints
- Route guard enforcement: super-admin allowed, hub-admin allowed, member denied
- 28 total sub-tests (7 metadata + 21 guard enforcement)

## Verification
- `go build ./...` — clean
- `go vet ./pkg/hub/...` — clean
- `go fmt` — clean
- All PR-A3 tests pass
- Bypass census test passes
- Pre-existing data race in `TestBrokerInboundRateLimit` is unrelated to these changes

## Not changed (per brief boundaries)
- `routeGuard`, `authz.go`, permission registry — untouched (PR-A1)
- `requireAdmin` function — still used by unconverted routes
- `user.suspend`/`user.promote` — super-admin-only, stays as-is
- Settings/operations/integrations handlers — other PRs
