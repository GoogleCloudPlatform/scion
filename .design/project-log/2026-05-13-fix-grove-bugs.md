# Fix Grove-to-Project Rename Bugs

**Date**: 2026-05-13
**PR**: https://github.com/ptone/scion/pull/69
**Branch**: scion/fix-grove-bugs

## Summary

Fixed 3 grove-to-project rename bugs discovered during a survey of the codebase.

## Changes

### 1. hubclient JSON tags (notifications.go, templates.go)
- `CreateSubscriptionTemplateRequest.ProjectID`, `SubscriptionTemplate.ProjectID`, and `CreateTemplateRequest.ProjectID` had JSON tags still set to `groveId`
- Updated primary tags to `projectId` and added `UnmarshalJSON`/`MarshalJSON` methods for backward compatibility with the legacy `groveId` field
- Follows the same dual-tag pattern already used by `Notification`, `Subscription`, `Agent`, `Project`, and other structs in the codebase

### 2. projectsync test expectations (projectsync_test.go)
- `TestBuildWebDAVURL` expectations referenced `/api/v1/groves/` but `buildWebDAVURL()` already builds `/api/v1/projects/`
- `TestSync_ValidationErrors` expected "grove ID is required" but production code returns "project ID is required"
- Updated all test expectations to match production behavior

### 3. agent-viz logparser compile error (parser.go)
- Line 191 called `extractGroveInfo()` which was previously renamed to `extractProjectInfo()`
- Updated the call site to match the function name

## Verification

- `go build ./...` passes
- `go test ./pkg/hubclient/...` passes
- `go test ./pkg/projectsync/...` passes
- `go test ./extras/agent-viz/internal/logparser/...` passes
