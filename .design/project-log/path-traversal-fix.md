# Path Traversal Fix in deleteGrove

**Date:** 2026-05-11
**Branch:** fix/workspace-path-fallback
**Commit:** fix: restore path traversal protection and update tests for projects fallback

## Problem

The `deleteGrove` function in `pkg/runtimebroker/handlers.go` had a broken path traversal check. It used `filepath.Dir(grovePath)` to derive the base directory for the `strings.HasPrefix` validation. Since `filepath.Dir(X)` always returns the parent of X, the check was tautologically true and provided zero protection. An attacker could delete arbitrary directories by passing a slug like `../../etc`.

## Fix

Replaced the single `filepath.Dir` derivation with explicit hardcoded base directories:
- `filepath.Join(globalDir, "projects")` — the new primary path
- `filepath.Join(globalDir, "groves")` — the legacy fallback path

The `strings.HasPrefix` check now validates the resolved grove path against BOTH allowed bases, rejecting any path that escapes either directory.

## Test Updates

Three tests were asserting the old `"groves"` default path. Since the fallback logic now returns `"projects"` when neither directory exists on disk (the common case in test environments), these were updated:
- `TestHubNativeGrovePath` in `pkg/hub/handlers_grove_test.go`
- `TestCreateAgentGroveSlugResolvesGrovePath` in `pkg/runtimebroker/handlers_test.go`
- `TestStartAgentGroveSlugResolvesGrovePath` in `pkg/runtimebroker/handlers_test.go`

## Verification

- `go build ./...` — passes
- `go vet ./...` — passes
- All targeted tests pass: `TestDeleteGrove_PathTraversal_Blocked`, `TestCreateAgentGroveSlugResolvesGrovePath`, `TestStartAgentGroveSlugResolvesGrovePath`, `TestHubNativeGrovePath`, `TestFindAgentInHubNativeGroves`
