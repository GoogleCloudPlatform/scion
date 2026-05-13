# Fix Template Link Backward Compatibility

**Date:** 2026-05-13  
**Branch:** `scion/fix-template-link`  
**Commit:** b0d8fde

## Problem

After the grove-to-project rename (commit 675a58b), existing projects on hubs lost their template links. The rename added backward-compatible `MarshalJSON`/`UnmarshalJSON` methods to virtually all persistence models (`Agent`, `Project`, `Notification`, etc.) but missed two models: `store.Template` and `store.SubscriptionTemplate`.

Additionally, hub-side handler request types (`CreateTemplateRequest`, `CloneTemplateRequest`, `createTemplateRequest`) did not accept the legacy `groveId` field — so older clients sending `groveId` in template creation/cloning requests silently lost the project association.

## Root Cause

The previous fix (675a58b) focused on `hubclient` types (client-side) but missed the `store` and `hub` (server-side) types for templates.

## Changes

1. **`pkg/store/models.go`** — Added `MarshalJSON`/`UnmarshalJSON` to `store.Template` and `store.SubscriptionTemplate` following the exact pattern used by other models.

2. **`pkg/hub/template_handlers.go`** — Added `UnmarshalJSON` to `CreateTemplateRequest` and `CloneTemplateRequest` to accept `groveId` from older clients.

3. **`pkg/hub/handlers_notifications.go`** — Added `UnmarshalJSON` to `createTemplateRequest` for subscription template creation backward compat.

4. **Tests** — Added 17 new test cases across `pkg/store/models_backward_compat_test.go` and `pkg/hub/capability_marshal_test.go` covering marshal/unmarshal round-trips, `groveId` fallback, and `projectId` precedence.

## Verification

- `go build ./...` — passes
- `go vet ./...` — passes
- `go test ./pkg/store/... ./pkg/hub/... ./pkg/hubclient/...` — all pass
