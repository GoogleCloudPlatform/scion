# Project Log: PR-E1 Role & Binding Management UI

**Date:** 2026-08-28
**Branch:** `scion/pf-p2e-roles-ui`
**Author:** dev-pf-p2e-roles-ui

## Summary

Created two new admin pages for managing RBAC role definitions and role bindings, completing the PR-E1 deliverable.

## Changes

### New Files

- **`web/src/components/pages/admin-roles.ts`** — Role definitions management page at `/admin/roles`
  - Lists all roles in a table with name, scope type, permission count, and system/custom badge
  - System roles are visually distinguished and have no edit/delete actions
  - Create dialog with name, description, scope type, and grouped permission multi-select
  - Edit dialog (pre-populated) for custom roles; scope type is immutable after creation
  - Delete dialog with warning about active bindings
  - 403 CanDelegate errors shown as feedback alerts

- **`web/src/components/pages/admin-role-bindings.ts`** — Role bindings management page at `/admin/role-bindings`
  - Lists bindings with principal, role name (resolved from role definitions), scope, and created date
  - Pagination with limit/offset (25 per page)
  - Create dialog with principal type (user/agent), principal ID, role selector, scope type, and conditional scope ID input
  - Client-side validation: scope ID required when scope type is "project"
  - Delete confirmation dialog per binding
  - 403 errors from CanDelegate or D10 guard shown as feedback alerts

### Modified Files

- **`web/src/client/main.ts`** — Added route entries and admin route guards for both pages
- **`web/src/components/shared/nav.ts`** — Added nav entries after Groups, before Diagnostics
- **`web/src/components/index.ts`** — Added component exports

## API Integration Notes

- Verified actual JSON field names from Go struct tags: `system` (not `is_system`), `scopeType` (camelCase), `createdAt`/`updatedAt`, `roleDefinitionId`, etc.
- Permission registry items use Go field names (no json tags): `ID`, `Resource`, `Action`, `Description`
- Uses `apiFetch` wrapper for automatic 401/403 handling
- Role name resolution for bindings done client-side via role definitions lookup

## Verification

- `make ci` passes (format, lint, compat guards, authz guards, tests, build)
- `npm run typecheck` passes (TypeScript type checking)
- `npm run build` produces correct production bundles
