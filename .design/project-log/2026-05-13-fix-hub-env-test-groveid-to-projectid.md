# Fix hub_env_test.go: groveId -> projectId in test settings

**Date**: 2026-05-13
**Task**: Fix two failing tests in `cmd/hub_env_test.go`

## Problem

Two tests were failing with `cannot infer project ID: not linked with Hub`:
- `TestRunEnvList_BareGroveFlag`
- `TestResolveEnvScope_SentinelInfersFromSettings`

## Root cause

The `setupEnvProjectWithHubProjectID` test helper wrote settings JSON with the old key `"groveId"` inside the `"hub"` object. After the grove-to-project rename, the `HubClientConfig.ProjectID` field uses koanf tag `projectId`. The koanf config loader has backward compatibility for `hub.grove_id` (snake_case, V1 format) but not for `hub.groveId` (camelCase). So `settings.Hub.ProjectID` was empty when `resolveEnvScope` tried to infer the project ID.

## Fix

Changed `"groveId"` to `"projectId"` in the hub settings map within `setupEnvProjectWithHubProjectID` (line 228 of `cmd/hub_env_test.go`).

## Verification

All env-related tests pass: `TestRunEnvList_*` and `TestResolveEnvScope_*`.
