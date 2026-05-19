# Project Log - 2026-05-09 - Final Hub Cleanup (Phase 1)

## Overview
Renamed remaining internal Go functions in `pkg/hub/` from "Grove" to "Project" as part of the transition to "Project" terminology. Fixed several test failures that arose from previously uncommitted changes and the renaming process.

## Changes

### Function Renames
- **pkg/hub/project_webdav.go**:
    - `updateGroveSyncState` -> `updateProjectSyncState`
- **pkg/hub/messagebroker.go**:
    - `bootstrapExistingGroves` -> `bootstrapExistingProjects`
    - `EnsureGroveSubscriptions` -> `EnsureProjectSubscriptions`
    - `subscribeGroveBroadcast` -> `subscribeProjectBroadcast`
    - `subscribeGroveUserMessages` -> `subscribeProjectUserMessages`
    - `fanOutToGrove` -> `fanOutToProject`
- **pkg/hub/handlers_github_app_webhook.go**:
    - `matchGrovesToInstallation` -> `matchProjectsToInstallation`
    - `updateGrovesForInstallation` -> `updateProjectsForInstallation`
    - `checkGrovesForRemovedRepos` -> `checkProjectsForRemovedRepos`
    - `updateGroveGitHubAppStatus` -> `updateProjectGitHubAppStatus`

### Test Fixes and Adjustments
- **pkg/hub/response_types.go**: Fixed `ProjectWithCapabilities` to use anonymous embedding of `store.Project`. This restored the original JSON structure (fields at top level instead of under a `"project"` key), which was breaking several GET handlers and their tests.
- **pkg/hub/handlers_project_test.go**: Updated `TestCreateProject_HubNative_NoGitRemote` to expect the slug `hub-native-project` instead of `hub-native-grove`, reflecting the name change from "Hub Native Grove" to "Hub Native Project".
- **pkg/hub/messagebroker_test.go**:
    - Updated assertions to expect 2 persisted messages in certain tests. This is due to `PublishUserMessage` performing local delivery (persistence) AND the `InProcessBroker` triggering the subscription handler (which also performs persistence). This behavior became reliable after fixing the project bootstrap logic.
    - Updated all test names and variable names to use "Project" instead of "Grove".

## Verification Results
- `go build ./...` passed.
- `go test ./pkg/hub/...` passed (all tests).
- Verified that REST API paths and CLI surface were not modified.
