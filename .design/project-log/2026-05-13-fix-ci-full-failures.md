# Fix CI Full Failures

**Date:** 2026-05-13
**Branch:** scion/fix-broken-tests
**PR:** https://github.com/ptone/scion/pull/67

## Summary

Ran `make ci-full` and fixed all failures introduced by the grove-to-project rename.

## Issues Found and Fixed

### 1. Formatting (156 files)
The grove-to-project rename introduced formatting inconsistencies across the codebase. Fixed with `gofmt`.

### 2. Build failure: missing no_sqlite stub
`pkg/ent/entc/migrate_grove_to_project.go` has `//go:build !no_sqlite` but `cmd/server_foreground.go` calls `MigrateGroveToProjectData` without a build constraint. Added `migrate_grove_to_project_nosqlite.go` with stub.

### 3. golangci-lint errcheck violations (6 issues)
Tests in `pkg/agent/run_test.go` and `pkg/harness/codex_test.go` had unchecked `os.Setenv`/`os.Unsetenv` returns. Replaced with `t.Setenv` + `os.Unsetenv` with `//nolint:errcheck`.

### 4. Test expectation mismatches (14 tests across 9 packages)
Tests still expected old "grove" terminology:
- URL paths: `/api/v1/groves/` → `/api/v1/projects/`
- Config keys: `"groveId"` → `"projectId"`, `grove_id` → `project_id`
- Column headers: `"GROVE"` → `"PROJECT"`
- Error messages: updated to match new wording
- Scope values: `"grove"` → `"project"`

## Process Notes

- The CI pipeline runs: fmt-check → web → web-typecheck → lint → golangci-lint → test-fast → build
- The `no_sqlite` build tag excludes SQLite-dependent code, used by lint and test-fast targets
- golangci-lint runs with `--new-from-rev=main` so only flags new issues
