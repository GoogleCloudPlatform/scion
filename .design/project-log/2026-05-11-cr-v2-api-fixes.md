# Project Log: Code Review v2 API Fixes
**Date:** 2026-05-11
**Author:** Developer Agent

## Summary
Implemented backward compatibility fixes for the `projectId` vs `groveId` renaming as requested in Code Review v2. These fixes ensure that both new and old clients can communicate with the Hub API and that notification events are fanned out to both legacy and new subjects.

## Changes

### 1. Project Cache API (Critical 1)
- Updated `ProjectCacheRefreshResponse` and `ProjectCacheStatusResponse` in both `pkg/hub/project_cache.go` and `pkg/hubclient/projects.go`.
- Implemented custom `MarshalJSON` and `UnmarshalJSON` to support both `projectId` and `groveId` tags.
- Aligned client and server struct definitions to use `projectId` as the primary tag.

### 2. Notifications and Sync Models (Critical 2)
- Added custom `MarshalJSON` and `UnmarshalJSON` for the following models in `pkg/store/models.go`:
    - `NotificationSubscription`
    - `Notification`
    - `ProjectSyncState`
- Added custom `MarshalJSON` and `UnmarshalJSON` for `ProjectSyncStatusResponse` in `pkg/hub/project_webdav.go`.
- These changes ensure that API responses include both `projectId` and `groveId` fields, and can consume either from client requests.

### 3. Notification Handlers (Critical 2)
- Updated `handleSubscriptionRoutes` and `handleSubscriptionTemplateRoutes` in `pkg/hub/handlers_notifications.go` to support the `groveId` query parameter as a fallback for `projectId` when listing subscriptions or templates.

### 4. Notification Dual-Publishing (Medium)
- Updated `PublishNotification` in `pkg/hub/events.go` to publish events to both `project.{id}.notification` and `grove.{id}.notification` subjects.
- This maintains compatibility for legacy subscribers watching the `grove.*` namespace.

## Verification Results
- Ran `go build ./...`: SUCCESS
- Ran `go vet ./...`: SUCCESS

## Observations
- Many structs were duplicated between `pkg/hub` and `pkg/hubclient`. Both needed to be updated to maintain consistency.
- The `encoding/json` package was missing in some files that previously didn't require custom marshalling; these were added.
