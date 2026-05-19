# Rebase grove-to-project fix branch and address review feedback

**Date:** 2026-05-13
**Agent:** rebase-grove-v2
**Branch:** scion/rebase-grove-v2

## Task

Rebase `scion/fix-grove-bugs` onto `origin/main` and fix PR review feedback from the v8 code review.

## What was done

### Rebase
- Cherry-picked 3 commits from `scion/fix-grove-bugs` onto `origin/main` (which had one commit ahead: `76b53cf fix: repair CI`)
- Resolved merge conflict in `pkg/projectsync/projectsync_test.go` — took the HEAD (main) version which already had correct "project" test data

### Review feedback fixes
Addressed 4 issues from the v8 review:

1. **BrokerHeartbeat UnmarshalJSON** (`pkg/hubclient/runtime_brokers.go`): Added to support incoming heartbeats from older brokers that send `groves` instead of `projects`
2. **ProjectHeartbeat UnmarshalJSON** (`pkg/hubclient/runtime_brokers.go`): Added to support legacy `groveId` field in individual project heartbeats
3. **listAgents projectId query param** (`pkg/runtimebroker/handlers.go`): Updated handler to check `projectId` first, falling back to `groveId` — consistent with `handleAgentByID` and `handleAgentAttach`
4. **CloneTemplateRequest MarshalJSON/UnmarshalJSON** (`pkg/hubclient/templates.go`): Added dual-field support, matching the pattern already used by `CreateTemplateRequest`

### Tests added
- `TestBrokerHeartbeat_UnmarshalJSON` — 3 subtests (projects key, groves key, precedence)
- `TestProjectHeartbeat_UnmarshalJSON` — 3 subtests (projectId, groveId, precedence)
- `TestCloneTemplateRequest_UnmarshalJSON` — 3 subtests
- `TestCloneTemplateRequest_MarshalJSON` — verifies dual-field output
- `TestCreateTemplateRequest_UnmarshalJSON` and `MarshalJSON` — existing but now verified

## Verification

- `go build ./...` — PASS
- `go vet ./...` — PASS
- `go test ./pkg/hubclient/...` — PASS (all 55 tests)
- `go test ./pkg/runtimebroker/...` — PASS
- `go test ./pkg/projectsync/...` — PASS
