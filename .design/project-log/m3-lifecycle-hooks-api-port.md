# M3: Lifecycle Hooks Hub API Port

**Date:** 2026-06-08  
**Agent:** lh-port-m3  
**Branch:** scion/lifecycle-hooks-port  

## What was done

Ported the admin-only CRUD HTTP API for lifecycle hooks (M3 of the lifecycle hooks feature, issue #35).

### Files changed
- **pkg/hub/audit.go** — Added `LifecycleHookEventType`, `LifecycleHookEvent`, `LifecycleHookExecutionEvent` types and `LogLifecycleHookEvent`/`LogLifecycleHookExecutionEvent` to the `AuditLogger` interface + `LogAuditLogger` implementations. Also added convenience functions matching main's existing audit pattern (nil-safe, fire-and-forget).
- **pkg/hub/handlers_lifecycle_hooks.go** — New file: Create/Get/List/Update/Delete handlers for `/api/v1/admin/lifecycle-hooks`. Includes GCP SA resolver adapter, validation error formatting, and request/response types.
- **pkg/hub/server.go** — Registered 2 routes: collection and by-ID, alongside existing admin endpoints.
- **pkg/hub/handlers_lifecycle_hooks_test.go** — 25 tests covering all CRUD operations, authz (admin-only enforcement for all 5 endpoints), validation rejection, version conflict, scope immutability, not-found, and method-not-allowed.
- **pkg/hub/audit_gcp_test.go** — Updated `mockAuditLogger` to satisfy expanded `AuditLogger` interface.

### Endpoints
| Method | Path | Description |
|--------|------|-------------|
| POST   | /api/v1/admin/lifecycle-hooks | Create hook |
| GET    | /api/v1/admin/lifecycle-hooks | List hooks (filter: scopeType, trigger, enabled) |
| GET    | /api/v1/admin/lifecycle-hooks/{id} | Get hook by ID |
| PUT    | /api/v1/admin/lifecycle-hooks/{id} | Update hook (optimistic locking via stateVersion) |
| DELETE | /api/v1/admin/lifecycle-hooks/{id} | Delete hook |

### How authz/audit/routes were wired
- **Authz:** Hub-admin only, using main's pattern: `GetUserIdentityFromContext` + `user.Role() != "admin"` → `Forbidden(w)`. Matches `admin_invites.go` and other admin handlers exactly.
- **Audit:** Added to `AuditLogger` interface with 2 new methods. Convenience function `LogLifecycleHookEvent` follows the same nil-guard + fire-and-forget pattern as `LogRegistrationEvent`, `LogGCPTokenGeneration`, etc. The execution audit (`LifecycleHookExecutionEvent`) is defined for M5 but not yet called.
- **Routes:** Registered via `s.mux.HandleFunc` in `setupRoutes()`, placed with the other `/api/v1/admin/` routes.

### Deviations from reference
- No deviations needed — the reference handler was already well-adapted to main's patterns (uses `extractID`, `writeJSON`, `readJSON`, `Forbidden`, `NotFound`, `MethodNotAllowed`, `BadRequest`, `writeError`). The authz pattern matches main's admin handlers exactly.

### Verification
- `go build ./...` — clean
- `go test ./pkg/hub/ -run LifecycleHook` — 25/25 pass
- `go test -race ./pkg/hub/ -run LifecycleHook` — 25/25 pass, no races
