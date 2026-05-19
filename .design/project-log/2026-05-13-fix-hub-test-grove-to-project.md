# Fix hub_test.go grove-to-project expected values

**Date:** 2026-05-13
**Task:** Fix two broken test assertions in cmd/hub_test.go

## Summary

Two tests in `cmd/hub_test.go` were failing because the production code (`cmd/hub.go`) was updated to return `"project"` instead of `"grove"` for scope/source labels, but the test expectations were not updated.

## Changes

- `TestGetHubEnabledScope_GroveHasOwnSetting` (line 239): changed expected `scope.Scope` from `"grove"` to `"project"`
- `TestGetHubEndpointScope_FromProject` (line 330): changed expected `scope.Source` from `"grove"` to `"project"`

## Verification

All hub-related tests pass after the fix. No other `"grove"` string literals remained in the test file.
