# Fix: UnmarshalJSON Backward-Compat Regression

**Date:** 2026-05-11
**Commit:** 23bd23e6
**Branch:** scion/rename-strategy

## Problem

Commit `bc4a5887` (rebase conflict resolution) introduced a systematic bug in 20 `UnmarshalJSON` methods across the codebase. The backward-compatibility condition that maps legacy `groveId` JSON fields to the new `ProjectID` struct field was changed from:

```go
if receiver.ProjectID == "" && aux.GroveID != "" {
    receiver.ProjectID = aux.GroveID
}
```

to:

```go
if receiver.ProjectID == "" && aux.ProjectID != "" {
    receiver.ProjectID = aux.GroveID
}
```

The bug: `aux.ProjectID` is a **promoted field** from the embedded `Alias` type, which points to the **same memory** as `receiver.ProjectID` (since the alias is initialized with a pointer to the receiver). This makes the condition tautologically false — if `receiver.ProjectID` is empty, then `aux.ProjectID` is also empty. Legacy `groveId` values in JSON payloads are silently dropped.

## Fix Applied

### 1. UnmarshalJSON (20 locations)
Changed `aux.ProjectID` back to `aux.GroveID` in the condition check across:
- `pkg/hubclient/`: agents.go, messages.go, notifications.go (3), projects.go (2), scheduled_events.go, schedules.go, tokens.go (2)
- `pkg/store/models.go`: 9 locations

### 2. project_discovery.go (3 locations)
Restored `pi.GroveID = settings.ProjectID` assignments that had been collapsed into duplicate `pi.ProjectID = settings.ProjectID` lines during rebase.

### 3. templates.go (1 location)
Fixed `CloneTemplateRequest.GroveID` field back to `ProjectID` with `json:"projectId"` tag, since the hub API expects `projectId` in clone requests.

## Verification
- `go build ./...` — passes
- `go vet ./...` — passes
- 10 files changed, 24 insertions, 24 deletions

## Observations
- This class of bug (promoted field shadowing in alias-based UnmarshalJSON) is easy to introduce during mechanical renames and hard to catch without specific test coverage for legacy field deserialization.
- The pattern of using `type Alias <Type>` with embedded pointer means any field on the alias that shares a name with the receiver will reference the same memory — a subtlety that automated find-and-replace misses.
