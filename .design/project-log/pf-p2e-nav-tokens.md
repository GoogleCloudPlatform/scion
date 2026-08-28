# PR-E3: Capabilities-Based Admin Nav & Token Scope Polish

**Branch:** `scion/pf-p2e-nav-tokens`
**Date:** 2026-08-28
**Agent:** dev-pf-p2e-nav-tokens

## Summary

Two UI improvements to the web frontend:

### 1. Capabilities-Based Admin Nav (`nav.ts`)

Replaced the binary `role === 'admin'` nav check with a three-tier
capabilities-based approach:

- **Super-admin** (`user.role === 'admin'`): Sees all admin nav items
- **Hub-admin** (detected via admin endpoint probe): Sees scopeable admin
  items (users, groups, settings, health, integrations, etc.) but NOT
  diagnostics or maintenance
- **Regular member**: Sees no admin items

Admin items are now split into two arrays:
- `ADMIN_SCOPEABLE_ITEMS` — visible to hub-admin + super-admin
- `ADMIN_SUPERADMIN_ITEMS` — visible only to super-admin (diagnostics, maintenance)

Hub-admin detection probes `GET /api/v1/admin/roles`. If it returns 200,
the user has admin capabilities. The probe is cached per user ID to avoid
repeated requests. Since the backend enforces all access control on admin
endpoints, the nav is purely a UX convenience — showing a link to a page
the user can't access just results in an error or empty page.

### 2. Token Scope Selector Polish (`token-list.ts`)

Replaced the flat 2-column checkbox grid with a grouped, searchable
scope selector:

- **Grouped by resource type**: Agent, Broker, Group, etc. Each group
  has a collapsible header showing the group name and selection count
- **Search/filter**: Sticky search input at the top filters by scope
  value, description, or resource type label
- **Alias separation**: Alias scopes (like `agent:manage`) are visually
  distinct with a highlighted card, a "collection" icon, and an expansion
  count badge (e.g., "Alias — expands to 6 scopes")
- **Descriptions**: Inline descriptions for every scope
- **Selected count**: Header shows "(N selected)" when scopes are checked
- **Scrollable container**: Max height of 360px with overflow scroll

The `AVAILABLE_SCOPES` constant was renamed to `FALLBACK_SCOPES` (with
`ScopeOption[]` typing instead of `as const`) since it serves as a
fallback when the dynamic `/api/v1/auth/scopes` fetch fails. Each scope
option now includes `resource` and `isAlias` fields for grouping.

## Files Changed

| File | Change |
|------|--------|
| `web/src/components/shared/nav.ts` | Capabilities-based admin nav |
| `web/src/components/shared/token-list.ts` | Grouped scope selector with search |
| `web/scripts/copy-shoelace-icons.mjs` | Added `collection` icon |
| `pkg/hub/permission_registry_test.go` | Updated scope drift test for renamed constant |

## Verification

- `make ci` passes (including the scope-drift detection test)
- `npm run typecheck` passes
- Pre-existing lint warnings unchanged; no new errors introduced
