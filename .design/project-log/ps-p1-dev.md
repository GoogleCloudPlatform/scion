# Project Log: Injected Skills Phase 1 — Data Model + Store

**Date:** 2026-07-22  
**Agent:** ps-p1-dev  
**Branch:** scion/project-skills-phase1  
**Issue:** #542 (Injected Skills feature, Phase 1)

---

## What Was Implemented

### 1. Ent Schema: `pkg/ent/schema/skill_injection.go`

Created the `SkillInjection` ent entity with the following fields:
- `id` (UUID, immutable, auto-generated)
- `scope` (string: "project" | "user")
- `scope_id` (string: project UUID or user UUID)
- `skill_uri` (string: full skill URI, may include version pin)
- `skill_as` (optional string: alias)
- `optional` (bool, default false)
- `sort_order` (int, default 0)
- `created_at` (time, immutable, auto-set)
- `created_by` (optional string)

Index: `(scope, scope_id)` for efficient queries by scope.

Ran `go generate ./pkg/ent/...` to produce all generated boilerplate in `pkg/ent/skillinjection/`.

### 2. Store Model: `pkg/store/models.go`

Added:
- `SkillInjection` struct with JSON tags
- `ToSkillReference()` method that converts to `api.SkillReference`
- `SkillInjectionScopeProject` and `SkillInjectionScopeUser` constants

### 3. Store Interface: `pkg/store/store.go`

Added:
- `SkillInjectionStore` interface embedded in the top-level `Store` interface
- Interface methods: `ListSkillInjections`, `AddSkillInjection`, `UpdateSkillInjection`, `RemoveSkillInjection`, `SetSkillInjections`
- Added `api` package import (needed for `api.SkillReference` in `SetSkillInjections`)

### 4. Entadapter Implementation: `pkg/store/entadapter/skill_injection_store.go`

Implemented `SkillInjectionStore` using the ent ORM client:
- `ListSkillInjections`: queries by scope+scope_id, ordered by sort_order ASC
- `AddSkillInjection`: creates a new record, sets `created_at` timestamp
- `UpdateSkillInjection`: updates mutable fields (skill_uri, skill_as, optional, sort_order); clears skill_as when set to empty
- `RemoveSkillInjection`: hard-deletes by UUID; returns ErrNotFound if missing
- `SetSkillInjections`: uses a transaction — deletes all existing entries for (scope, scope_id), then bulk-inserts the new list with sort_order = index

Added a `nullableString` helper to convert empty strings to nil pointers for optional ent fields.

### 5. CompositeStore Updated: `pkg/store/entadapter/composite.go`

- Embedded `*SkillInjectionStore` in `CompositeStore`
- Wired `NewSkillInjectionStore(client)` in `NewCompositeStore`

### 6. Tests: `pkg/store/entadapter/skill_injection_store_test.go`

10 test functions covering:
- `TestSkillInjection_AddListRemove` — basic CRUD, field round-trip
- `TestSkillInjection_RemoveNotFound` — ErrNotFound on missing ID
- `TestSkillInjection_Update` — mutable field updates
- `TestSkillInjection_UpdateClearsAlias` — clearing optional skill_as
- `TestSkillInjection_ListOrderedBySortOrder` — ordering guarantee
- `TestSkillInjection_ScopeIsolation` — different scopes are independent
- `TestSkillInjection_SetSkillInjectionsAtomicReplace` — full list replacement
- `TestSkillInjection_SetSkillInjectionsEmpty` — clearing all entries
- `TestSkillInjection_SetSkillInjections_DoesNotAffectOtherScopes` — scope isolation for SetSkillInjections
- `TestSkillInjection_ToSkillReference` — conversion to api.SkillReference

---

## Key Decisions

### No API or business logic
Phase 1 is strictly the data model and store layer, per the spec. No HTTP handlers or provisioner integration was added.

### `index.Fields` instead of `index.On`
The task brief used `index.On("scope", "scope_id")` in the schema example, but the correct ent API is `index.Fields("scope", "scope_id")`. Fixed before running codegen.

### Hard delete for `RemoveSkillInjection`
Unlike skills (which soft-archive), skill injections are transient list entries with no version history. Hard delete is appropriate and simpler.

### `nullableString` helper
The ent generated setters require pointers for optional string fields (`SetNillableSkillAs`, `SetNillableCreatedBy`). A small local helper converts empty strings to nil to avoid storing empty strings in optional columns.

### Transaction pattern for `SetSkillInjections`
Followed the existing pattern from `skill_registry_store.go`: `s.client.Tx(ctx)` with deferred rollback and explicit commit. Delete-then-insert ensures atomicity.

### Import of `api` package in `store.go`
The `SetSkillInjections` interface method takes `[]api.SkillReference`, which required adding `"github.com/GoogleCloudPlatform/scion/pkg/api"` to store.go's imports. This is consistent with how `models.go` already imports the api package.

---

## Test Results Summary

```
go test ./pkg/store/... -count=1

ok   github.com/GoogleCloudPlatform/scion/pkg/store           0.005s
ok   github.com/GoogleCloudPlatform/scion/pkg/store/entadapter 4.818s
ok   github.com/GoogleCloudPlatform/scion/pkg/store/storetest  1.488s
```

All 10 new SkillInjection tests pass. Full store test suite (400+ tests) passes with no regressions.

`go build ./...` is clean.
