# Tier 1 Grove→Project Rename: agent, runtime, broker, logging

**Date:** 2026-05-29
**Scope:** `pkg/agent/`, `pkg/runtime/`, `pkg/broker/`, `pkg/util/logging/`, `pkg/sciontool/telemetry/`, `pkg/sciontool/hooks/handlers/`

## Summary

Completed Tier 1 (internal identifiers only) rename of "grove" → "project" across six packages. This covers local variables, function parameters, unexported functions, comments, human-readable strings, and test names/data.

## What Changed

### pkg/agent/ (13 files)
- `manager.go`: Variable `deletionGroveName` → `deletionProjectName`; comments updated
- `list.go`: Variable `grovesToScan` → `projectsToScan`; comments updated
- `provision.go`: Variable `groveSlug` → `projectSlug`; comments and error messages updated
- `resolve.go`: Comment updated
- `run.go`: ~15 comments updated; label keys and env var strings left untouched
- `msgbuffer.go`: Debug strings and comments updated; `"grove_id"` slog key left untouched
- `msgbuffer_test.go`: Test data values "grove-a"→"project-a", "grove-b"→"project-b"
- `provision_test.go`: Variables `resolvedGroveDir`→`resolvedProjectDir`, `groveTplDir`→`projectTplDir`, `inGroveAgentDir`→`inProjectAgentDir`; test env value updated
- `run_test.go`: Comments and test URL path segments updated
- `delete_test.go`, `stop_project_containers_test.go`, `list_test.go`, `provision_compose_test.go`: Test data and comments updated

### pkg/runtime/ (12 files)
- `k8s_runtime.go`: Params `groveName`→`projectName` in `sharedDirPVCName` and `cleanupSharedDirPVCs`; local var renamed
- `factory.go`: Param `grovePath`→`projectPath`; debug string updated
- `factory_test.go`: Variables `groveScionDir`→`projectScionDir`, `groveSettings`→`projectSettings`
- `common.go`, `docker.go`, `podman.go`, `apple_container.go`: Comments updated
- `common_test.go`: Variable `groveDir`→`projectDir`; test data updated
- `k8s_shared_dirs_test.go`, `k8s_secrets_test.go`, `secrets_test.go`, `podman_test.go`: Test data values updated

### pkg/broker/ (2 files)
- `broker.go`: Function params `groveID`→`projectID` in topic construction functions; comments updated; topic strings (wire protocol) left untouched
- `broker_test.go`: Test table name and comments updated; topic strings left untouched

### pkg/util/logging/ (7 files)
- `request_log.go`: Struct field `GroveIdx`→`ProjectIdx`; function params `groveID`→`projectID`; comments updated; URL prefixes left untouched
- `logging.go`, `gcp_handler.go`, `cloud_handler.go`: Comments updated; `"grove_id"` attribute keys left untouched
- `*_test.go`: Test data values updated

### pkg/sciontool/telemetry/ (2 files)
- `providers.go`, `gcp_exporter.go`: Variable `groveID`→`legacyProjectID` (to avoid collision with existing `projectID` variable in scope)

### pkg/sciontool/hooks/handlers/ (1 file)
- `status_test.go`: Test value "my-grove"→"my-project"

## What Was NOT Changed (Tier 2/3)
- Container label keys: `"scion.grove"`, `"scion.grove_id"`, `"scion.grove_path"`
- Environment variable strings: `"SCION_GROVE_ID"`, `"SCION_GROVE"`
- NATS topic prefixes: `"scion.grove."`
- Telemetry attribute strings: `"scion.grove.id"`, `"grove_id"` (GCP metric labels)
- Structured log attribute keys: `"grove_id"` in slog calls
- URL API paths: `/api/v1/groves/`
- Filesystem paths: `/scion-volumes/`, PVC naming conventions

## Issues Encountered
- **TestRequestLogMiddleware_ProducesCorrectJSON**: After renaming the expected value from "test-grove" to "test-project", the test failed because the URL path `/api/v1/groves/test-grove/agents` still contained "test-grove" as the path segment extracted as the project ID. Fixed by updating the URL path segment value (test data) while keeping the `/api/v1/groves/` prefix (API endpoint) unchanged.
- **Telemetry variable collision**: In `providers.go` and `gcp_exporter.go`, the variable `groveID` couldn't simply become `projectID` since that name was already in scope. Renamed to `legacyProjectID` to indicate it maps to the legacy `"grove_id"` label.

## Verification
- `go build ./...` — passed
- `go vet ./...` — passed
- All tests in affected packages — passed
