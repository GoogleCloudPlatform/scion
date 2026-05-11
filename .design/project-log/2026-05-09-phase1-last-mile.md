# Phase 1 - Last Mile: Grove to Project Renaming

## Summary
Completed the renaming of internal Go functions, variables, and parameters from "Grove" to "Project" in `pkg/hub`, `pkg/hubsync`, and `cmd` directories.

## Changes
- **pkg/hub/handlers.go & pkg/hub/server.go**: Renamed all main handler functions (e.g., `handleGroves` -> `handleProjects`, `listGroves` -> `listProjects`). Updated route registrations.
- **pkg/hub/handlers_github_app.go**: Renamed `handleCheckGroveGitHubStatus` -> `handleCheckProjectGitHubStatus` and updated its implementation.
- **pkg/hubsync/prompt.go**: Renamed `GroveMatch` struct to `ProjectMatch` and updated all related functions.
- **cmd/server_dispatcher.go**: Renamed `resolveGrovePath` to `resolveProjectPath`.
- **cmd/template_helpers.go**: Renamed `IsGrove` to `IsProjectScoped`.
- **cmd/templates.go**: Renamed parameters and internal logic in `printTemplateListHubMode`.
- **File Renames**:
    - `pkg/hub/handlers_grove_test.go` -> `pkg/hub/handlers_project_test.go`
    - `pkg/hub/authz_grove_owner_test.go` -> `pkg/hub/authz_project_owner_test.go`
    - `pkg/hub/grove_webdav.go` -> `pkg/hub/project_webdav.go`

## Constraints Followed
- NO CLI surface changes (UI strings still use 'grove' where appropriate for user familiarity).
- NO REST API path string changes (e.g., `/api/v1/groves` remains unchanged).
- NO JSON tag changes (e.g., `json:"groveId"` remains unchanged).
- `go build ./...` passes.

## Observations
- Many internal functions were already partially renamed or used a mix of `grove` and `project` terminologies.
- The `Resource` type in authorization logic still uses `"grove"` as a string for `Type` and `ParentType` to maintain compatibility with existing policies and database records (`ScopeProject = "grove"` in `pkg/store/models.go`).
