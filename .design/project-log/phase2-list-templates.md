# Phase 2: HubClient.ListTemplates + Template Type

**Date:** 2026-07-30
**Branch:** scion/ca-dev-thread-p2
**Feature:** `/scion thread` Discord command — template listing capability

## Summary

Added `ListTemplates` to the Discord plugin's `HubClient` interface, enabling
template autocomplete and validation for the upcoming `/scion thread` command.

## Changes

### `internal/discord/commands.go`
- Added `Template` struct with `Slug` and `Name` fields (near existing `AgentInfo`
  and `ProjectOption` types).
- Extended `HubClient` interface with
  `ListTemplates(ctx context.Context, projectID string) ([]Template, error)`.

### `internal/discord/hubclient.go`
- Added `hubTemplatesResponse` and `hubTemplate` response types matching the hub's
  `listTemplatesV2` JSON output (`templates` array with `slug`, `name`,
  `displayName`, `scope`, `scopeId`, `status` fields).
- Implemented `ListTemplates` on `httpHubClient`:
  - Fetches global templates via `GET /api/v1/templates?scope=global&status=active`.
  - Fetches project-scoped templates via
    `GET /api/v1/templates?scope=project&projectId=<id>&status=active` (when
    `projectID` is non-empty).
  - Merges results by slug; project-scoped templates take precedence over global.
  - Uses `DisplayName` with fallback to `Name` for the human-readable label.
  - Follows existing patterns from `ListProjects` and `ListAgents` for error
    handling, HMAC signing, and debug logging.

### Test doubles
- No existing mock/stub implementations of `HubClient` were found in the test
  suite — no updates required.

## Verification

- `go build ./...` — passes
- `go test ./... -count=1 -timeout 120s` — all tests pass
