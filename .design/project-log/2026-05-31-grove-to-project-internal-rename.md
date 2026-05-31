# Grove-to-Project Internal Rename

**Date:** 2026-05-31
**Issue:** https://github.com/ptone/scion/issues/101
**Branch:** scion/cleanup-grove-to-project

## What was done

Renamed internal-only "grove" usages to "project" across the codebase. This covers
variable names, function parameters, comments, log messages, user-facing strings
(help text, warning messages, prompts), the embedded default settings file, and
the fs-watcher-tool CLI flag.

## Scope

- **45 production + test files changed** across `cmd/`, `pkg/`, and `extras/`
- Comments and log messages updated in `cmd/attach.go`, `cmd/cdw.go`, `cmd/clean.go`,
  `cmd/completion_helper.go`, `cmd/config.go`, `cmd/delete.go`, `cmd/list.go`,
  `cmd/message.go`, `cmd/schedule.go`, `cmd/stop.go`, `cmd/suspend.go`,
  `cmd/server_foreground.go`, `cmd/server_dispatcher.go`, `cmd/sciontool/commands/init.go`,
  `cmd/harness_config_install.go`, `cmd/logs.go`
- Internal variables/comments in `pkg/agentcache/cache.go`, `pkg/harness/`,
  `pkg/secret/`, `pkg/util/git.go`, `pkg/runtimebroker/types.go`, `pkg/projectsync/`
- Embedded file renamed: `default_grove_settings.yaml` -> `default_project_settings.yaml`
- fs-watcher-tool: added `--project` flag, kept `--grove` as deprecated alias
- Corresponding test files updated to match

## What was NOT changed (backward-compat surfaces)

All wire-format and backward-compatibility surfaces were preserved:
- JSON struct tags, container labels, environment variables, NATS topics,
  SQL schema, deprecated CLI flags, API endpoints, config key aliases,
  YAML struct tags, storage paths, query parameters, telemetry attribute values

These are tracked in issue #101 for a future coordinated breaking-change release.

## Observations

- The codebase was already substantially migrated — many variable names and all
  user-facing prompt strings in `pkg/hubsync/prompt.go` were already using "project"
- The remaining "grove" references in production code are predominantly COMPAT
  code (JSON marshal/unmarshal pairs, legacy path fallbacks, deprecated flag handling)
- Pre-existing test failures exist in this environment (Docker not available,
  hub env state) — these are not caused by this change
