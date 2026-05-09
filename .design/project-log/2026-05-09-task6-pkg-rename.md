# Task 6: pkg/grovesync -> pkg/projectsync rename

## Work Completed
- Renamed `pkg/grovesync` directory to `pkg/projectsync`.
- Renamed files within `pkg/projectsync`:
    - `grovesync.go` -> `projectsync.go`
    - `grovesync_test.go` -> `projectsync_test.go`
- Updated package name in `pkg/projectsync/*.go` to `projectsync`.
- Updated package documentation and comments in `pkg/projectsync/projectsync.go` to refer to "project-level" and "project sync".
- Updated `cmd/sync.go`:
    - Updated import `github.com/GoogleCloudPlatform/scion/pkg/grovesync` to `github.com/GoogleCloudPlatform/scion/pkg/projectsync`.
    - Renamed all usages of `grovesync.` to `projectsync.`.
    - Renamed internal function `runGroveSync` to `runProjectSync`.
    - Renamed internal function `resolveGroveWorkspacePath` to `resolveProjectWorkspacePath`.
    - Updated command help text, examples, and status messages to use "project" instead of "grove".

## Verification
- Ran `go build ./pkg/projectsync/...` which completed successfully.
- Verified that no other Go files in the project import `pkg/grovesync`.
- `cmd/sync_test.go` was verified to not import `grovesync`.

## Observations
- The build of the entire project currently fails due to unrelated changes in `pkg/api` and `pkg/config` (handled by other tasks), but `pkg/projectsync` itself is building correctly.
- Kept `GroveID` in `Options` and API calls within `pkg/projectsync` to maintain compatibility with the current Hub API structure, as those renames are out of scope for this specific task.
