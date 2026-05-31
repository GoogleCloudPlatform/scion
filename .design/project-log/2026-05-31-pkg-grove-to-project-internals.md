# pkg/ Internal Grove-to-Project Rename

**Date**: 2026-05-31
**Task**: Rename internal grove references to project in pkg/ files

## Changes Made

Updated internal comments, parameter names, and doc strings in 9 files:

- `pkg/agentcache/cache.go` - comments (per grove -> per project, for a grove -> for a project, for a grove path -> for a project path), parameter `grovePath` -> `projectPath`
- `pkg/agentcache/cache_test.go` - test name "global grove" -> "global project"
- `pkg/harness/resolve.go` - comment "(template, grove, global)" -> "(template, project, global)"
- `pkg/harness/claude_code.go` - comment ".../grove/.scion/agents/" -> ".../project/.scion/agents/"
- `pkg/runtimebroker/types.go` - comments "git-anchored groves" -> "git-anchored projects", "grove-level" -> "project-level"
- `pkg/secret/secret.go` - comment "user, grove, runtime_broker" -> "user, project, runtime_broker"
- `pkg/secret/localbackend.go` - comment "grove/broker" -> "project/broker"
- `pkg/projectsync/projectsync.go` - comment "grove identifier" -> "project identifier"
- `pkg/util/git.go` - comments about grove IDs -> project IDs, shared workspace grove -> project

## Intentionally Unchanged

The following were examined and intentionally left as-is (backward-compat surfaces):

- JSON struct tags (`json:"groveId"`, `json:"grovePath"`, etc.)
- NATS topic strings (`"scion.grove."`)
- Container label strings (`"scion.grove_id"`)
- Environment variable strings (`"SCION_GROVE_ID"`)
- Query parameter strings (`"groveId"`)
- Storage path strings (`"groves/"`, `"grove-configs/"`)
- Telemetry attribute values (`"grove_id"`)
- Error code strings (`"global_grove_disabled"`)
- Legacy compat Marshal/Unmarshal comments ("legacy grove fields")
- All comments in `pkg/config/` describing legacy/compat behavior
- All comments in `pkg/runtimebroker/handlers.go` describing compat params/paths

## Verification

- `go build ./...` passes
- `go test ./pkg/agentcache/... ./pkg/util/...` passes
