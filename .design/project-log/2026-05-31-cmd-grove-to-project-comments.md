# cmd/ grove-to-project: comments, help text, and messages

**Date:** 2026-05-31
**Task:** Rename "grove" to "project" in comments, help text strings, and user-facing messages across the `cmd/` directory.

## What was done

Updated comments, help text strings, log messages, and user-facing output in the following files to replace "grove" with "project":

- `cmd/attach.go` - 5 comment updates
- `cmd/cdw.go` - 2 comment updates
- `cmd/clean.go` - 3 comment updates
- `cmd/delete.go` - 3 updates (2 comments, 1 string)
- `cmd/stop.go` - 5 updates (4 comments, 1 help text)
- `cmd/suspend.go` - 1 help text update
- `cmd/message.go` - 4 updates (2 comments, 2 help text)
- `cmd/schedule.go` - 1 comment update
- `cmd/list.go` - 20 updates (comments, strings, help text, variable rename)
- `cmd/completion_helper.go` - 7 updates (comments, parameter rename)
- `cmd/server_foreground.go` - 7 updates (comments, log messages, warning text)
- `cmd/config.go` - 6 updates (comments, help text, label strings)
- `cmd/logs.go` - 1 comment update
- `cmd/harness_config_install.go` - 1 string update
- `cmd/sciontool/commands/init.go` - 5 comment updates
- `cmd/server_dispatcher.go` - 5 updates (comments)

## What was preserved (backward compatibility)

All backward-compat surfaces were left unchanged per the task spec:
- Deprecated `--grove` flag definitions
- `"grove"` as scope/alias values
- Container labels (`scion.grove`)
- API endpoints (`/api/v1/groves/`)
- Config keys (`grove_id`, `hub.groveId`)
- Deprecated subcommands and aliases
- CLI mode map keys
- Filesystem paths (`groves` directory)
- Error codes (`global_grove_disabled`)

## Verification

- `go build ./...` passes cleanly with all changes.
