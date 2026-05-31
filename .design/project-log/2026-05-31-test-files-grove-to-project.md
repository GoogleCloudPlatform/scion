# Test Files: grove-to-project Rename

## Summary
Updated 17 test files to rename internal `groveDir` variables to `projectDir`, update comments from "grove" to "project" terminology, and fix test assertions to match already-updated production output.

## Files Changed
- `cmd/create_test.go` - `groveDir` -> `projectDir`, comment update
- `cmd/delete_test.go` - `groveDir` -> `projectDir`, comments updated, function doc updated
- `cmd/completion_helper_test.go` - `groveDir` -> `projectDir`, comments (kept `--grove` flag refs)
- `cmd/list_test.go` - Fixed assertions to match new production output ("project" not "grove")
- `cmd/project_test.go` - Comments and test names updated
- `cmd/message_test.go` - Comment "grove-scoped" -> "project-scoped"
- `cmd/sync_test.go` - `groveDir` -> `projectDir`, comments
- `cmd/hub_test.go` - `groveDir` -> `projectDir`, comments
- `cmd/hub_env_test.go` - `groveDir` -> `projectDir`, comments
- `cmd/notifications_test.go` - Comment fix
- `cmd/root_test.go` - Comment fix
- `cmd/server_dispatcher_test.go` - `grove` -> `project` variable and store objects
- `cmd/sciontool/commands/init_test.go` - Comments updated
- `pkg/harness/claude_code_test.go` - Test path `"grove"` -> `"project"`
- `pkg/secret/localbackend_test.go` - Comment fixes only
- `pkg/secret/gcpbackend_test.go` - Comment fixes only
- `pkg/util/git_test.go` - Comment fix

## Preserved (backward compat)
- All `--grove` flag testing
- All `grove_id` config key references
- All `SCION_GROVE_ID` env var references
- All `/api/v1/groves/` API endpoint paths
- All `scion.grove.*` NATS topic strings
- All arbitrary test IDs containing "grove" (e.g., "grove-del-123")
- All broker_test.go NATS topic tests (untouched)

## Verification
- `go build ./...` passes
- All tests compile successfully
- Pre-existing test failure (`TestDeleteStopped_RequiresGroveContext`) confirmed to be caused by missing Docker in test environment, not by these changes
