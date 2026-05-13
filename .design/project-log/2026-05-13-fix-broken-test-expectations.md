# Fix broken test expectations after grove-to-project rename

**Date**: 2026-05-13

## Summary

Fixed three test failures caused by the grove-to-project rename where test expectations were not updated to match production code changes.

## Changes

1. **`cmd/sync_test.go:85`** - `TestResolveAgentID_AgentNotFound`: Updated expected error from `"not found in grove"` to `"not found in project"` to match `cmd/sync.go:550`.

2. **`pkg/hubsync/sync_test.go:80`** - `TestEnsureHubReady_GlobalFallbackWithHubEnabled`: Changed the settings YAML key from `grove_id` to `project_id`. The `Settings.ProjectID` field now has yaml tag `project_id` (in `pkg/config/settings.go:133`), so the old `grove_id` key was silently ignored, resulting in an empty `ProjectID`.

3. **`pkg/runtime/k8s_shared_dirs_test.go:304`** - `TestCreateSharedDirPVCs_MissingGroveLabel`: Updated expected error substring from `"missing scion.grove label"` to `"missing scion.project or scion.grove label"` to match `pkg/runtime/k8s_runtime.go:685`.

## Verification

All three tests pass after the fixes.
