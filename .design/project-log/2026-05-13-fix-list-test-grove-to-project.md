# Fix list_test.go GROVE -> PROJECT column expectation

**Date:** 2026-05-13
**Task:** Fix TestDisplayAgentsLocalMode and TestDisplayAgentsHubMode failures

## Summary

Two tests in `cmd/list_test.go` were failing because the expected column header name was still `"GROVE"` while the production code (`cmd/list.go`) had already been updated to use `"PROJECT"`.

## Changes

- `cmd/list_test.go` line 149: Changed expected column `"GROVE"` to `"PROJECT"` in `TestDisplayAgentsLocalMode`
- `cmd/list_test.go` line 210: Changed expected column `"GROVE"` to `"PROJECT"` in `TestDisplayAgentsHubMode`

## Observations

- The empty-list messages ("No active agents found in the current grove." and "No active agents found across any groves.") still use "grove" in production code, and the test expectations match, so those were left unchanged.
- There are 5 other pre-existing test failures in the `cmd` package unrelated to this change.
