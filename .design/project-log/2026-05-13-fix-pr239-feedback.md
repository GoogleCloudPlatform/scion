# Fix PR #239 Feedback Items

**Date:** 2026-05-13
**Branch:** scion/fix-broken-tests

## Changes Made

### 1. AttrMsgProjectID constant (HIGH)
- **File:** `pkg/util/logging/message_log.go`
- Changed `AttrMsgProjectID` from `"grove_id"` to `"project_id"` to align with the grove-to-project rename.
- Updated test in `message_log_test.go`: renamed test case from "grove_id promoted" to "project_id promoted" and updated test value.

### 2. MigrateGroveToProjectData no-op stub (MEDIUM)
- **File:** `pkg/ent/entc/migrate_grove_to_project_nosqlite.go`
- Changed stub to return `nil` instead of `fmt.Errorf(...)`, matching the documented no-op behavior.
- Removed unused `"fmt"` import.

## Verification
- `go build ./...` - passed
- `go test ./pkg/util/logging/... -v` - all 60 tests passed
- `go test ./pkg/hub/ -run LogQuery -v` - passed
- Committed and pushed to `scion/fix-broken-tests`.
