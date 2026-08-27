# PR-B1: Quota Schema + Store Interface (Phase 2B)

**Date:** 2026-08-27
**Agent:** dev-pf-p2b-schema
**Branch:** scion/pf-p2b-schema

## Summary

Implemented the schema and store layer for the limits/quotas subsystem (F2.1),
the first deliverable of Permissions Phase 2B. This is a greenfield subsystem
with no modifications to existing functionality.

## What was built

### 1. Ent Schemas (3 new tables)
- **LimitDefinition** (`pkg/ent/schema/limitdefinition.go`) — configurable resource limit
  with name (unique), resource_type, unit, default_value, and system flag.
- **EntitlementBinding** (`pkg/ent/schema/entitlementbinding.go`) — associates a limit
  with a subject (user/group/system_default) scoped to system or project. Unique
  constraint on (limit_definition_id, subject_type, subject_id, scope_type, scope_id).
- **UsageReservation** (`pkg/ent/schema/usagereservation.go`) — tracks individual quota
  consumption. Active reservations have released_at IS NULL; released ones are
  retained for auditing.

### 2. Store Models (`pkg/store/models.go`)
- `LimitDefinition`, `EntitlementBinding`, `UsageReservation` structs
- Constants for subject types, scope types, and system limit names

### 3. Store Interface (`pkg/store/store.go`)
- `QuotaStore` interface with full CRUD for all three types plus
  `CountActiveReservations`, `ReleaseReservation`, `ReleaseReservationsByResource`
- `ErrQuotaExceeded` sentinel error (for use by PR-B2 QuotaService)
- `QuotaStore` added to composite `Store` interface

### 4. Ent Adapter (`pkg/store/entadapter/quota_store.go`)
- Complete implementation of `QuotaStore` interface
- Wired into `CompositeStore` via embedding

### 5. Seed Function (`pkg/hub/seed.go`)
- `seedLimitDefinitions` creates three system limit definitions with
  DefaultValue=0 (unlimited) per sponsor decision OQ-2 Option B:
  - `max_agents_per_project`
  - `max_projects_per_user`
  - `max_members_per_group`
- Idempotent (check-then-create pattern)
- Called from server startup after `seedRoleDefinitions`

### 6. Tests (`pkg/store/entadapter/quota_store_test.go`)
- 27 tests covering: CRUD for all three types, unique constraint enforcement,
  CountActiveReservations excluding released records, ReleaseReservation
  setting released_at without deletion, ReleaseReservationsByResource,
  ListActiveReservations, and seed idempotency.

## Design decisions

- **UUID IDs** — consistent with all other ent schemas in the project (uuid.UUID
  primary keys with Default(uuid.New)).
- **Time field naming** — used `created_at`/`updated_at` (matching the brief)
  rather than `created`/`updated` (used by Phase 1 schemas). Both work; the
  store models use `CreatedAt`/`UpdatedAt` regardless.
- **ReleaseReservation** — sets `released_at` rather than deleting the row,
  preserving audit history as specified.
- **No enforcement** — per boundaries, enforcement hooks (QuotaService, advisory
  locks, creation-time checks) are deferred to PR-B2.

## Verification

- `go build -buildvcs=false ./...` — clean
- `go test ./pkg/store/... ./pkg/store/entadapter/...` — all pass
- `golangci-lint run --new-from-rev=main` — 0 issues
