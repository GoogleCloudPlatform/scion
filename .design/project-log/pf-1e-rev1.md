# Phase 1E Review Round 1 — pf-1e-rev1

**Date:** 2026-08-25
**Role:** Code Reviewer (independent)
**Scope:** 6 commits (9cc0722..97350b8), 37 files, ~10K insertions

## Verdict

**REQUEST CHANGES** — one Required finding.

## Required

- **R1: Backfill pagination** — `backfillUserRoleBindings` and `backfillProjectRoleBindings` use `ListUsers`/`ListProjects` without pagination. The Ent adapter defaults to limit=50 (users) and limit=500 (projects). Hubs exceeding these thresholds will have incomplete backfill. While the legacy fallback covers this during migration, Phase 1F removal of the fallback would hard-fail unbackfilled principals.

## Optional / Nit

- Dead code: `parseUUIDString` in `role_store.go` (never called).
- Consider `OnDelete: Restrict` on `role_binding.role_definition_id` instead of the default `SetNull`.
- `getEffectivePermissions` has an N+1 query pattern (acceptable today, should be batch-loaded before Phase 1F).
- Test typo: `NoBypas` → `NoBypass`.

## What Passed

- Authorization dual-read in `isProjectOwnerOrAdmin` is correctly fail-closed.
- Backfill is idempotent (verified by test).
- Schema design is sound: proper indexes, unique constraint on bindings, FK relationships.
- All 24 new tests pass; `make ci` passes cleanly.
- Seed correctly derives permissions from the registry.
- Project membership is keyed by project ID (survives renames).

## Gates

- `go test ./pkg/hub -timeout=600s -count=1` — passed (10 unrelated SQLite OOM failures).
- `make ci` — **PASSED** (format OK, auth guards OK, build OK).
