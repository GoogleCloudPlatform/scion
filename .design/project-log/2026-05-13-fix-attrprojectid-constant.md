# Fix AttrProjectID constant value

**Date:** 2026-05-13
**Branch:** scion/fix-broken-tests
**PR:** #67

## Summary

Updated the `AttrProjectID` constant in `pkg/util/logging/logging.go` from `"grove_id"` to `"project_id"` to align with the project-wide grove-to-project rename.

## Changes

1. **`pkg/util/logging/logging.go`** - Changed `AttrProjectID = "grove_id"` to `AttrProjectID = "project_id"`
2. **`pkg/util/logging/cloud_handler_test.go`** - Updated test name and error message from `grove_id` to `project_id`
3. **`pkg/util/logging/request_log_test.go`** - Updated two test assertions that used hardcoded `"grove_id"` key lookups to use `"project_id"`

## Verification

- `go build ./...` passes
- `go test ./pkg/hub/ -run LogQuery -v` passes
- `go test ./pkg/util/logging/ -v` passes (all 40+ tests)

## Observations

- The constant was the root cause of `TestRequestLogMiddleware_ProducesCorrectJSON` and `TestRequestLogMiddleware_HandlerEnrichment` failures, since the JSON output key changed from `grove_id` to `project_id` but the tests were still looking up `"grove_id"`.
- There are many other `"grove_id"` strings throughout the codebase (log messages, struct tags, database columns) that are separate concerns and not part of this fix.
