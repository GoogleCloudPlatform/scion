# Project Log: Finishing Phase 2 CLI Renames (Grove -> Project)

## Date: 2026-05-09

## Summary
Completed the Phase 2 CLI renames, transitioning from "grove" to "project" across all primary user-facing commands and flags. This included updating flag names, help text, descriptions, and error messages while maintaining backward compatibility through hidden, deprecated "grove" flags.

## Changes Made

### 1. Root Command (`cmd/root.go`)
- Updated global descriptions and comments to use "project".
- Ensured `--project` (with `-g` shorthand) is the primary flag for project identification.
- Kept hidden deprecated `--grove` flag for compatibility.
- Updated manual parsing in `Execute()` to handle both `--project` and `--grove`.

### 2. Broker Command (`cmd/broker.go`)
- Updated `broker provide` and `broker withdraw` flags: `--project` is now primary, `--grove` is deprecated and hidden.
- Updated all "grove" strings in command descriptions, examples, and console output to "project".

### 3. Hub Resource Commands
- **Hub Env** (`cmd/hub_env.go`): Updated `--project` flag, renamed internal variables (e.g., `envGroveScope` to `envProjectScope`), and updated help text/examples.
- **Hub Secret** (`cmd/hub_secret.go`): Updated `--project` flag, renamed internal variables, and updated help text/examples.
- **Hub Token** (`cmd/hub_token.go`): Updated `--project` flag as primary (required), deprecated `--grove`, and updated help text/examples.
- **Notifications** (`cmd/notifications.go`): Verified existing project/grove flag setup and updated help text.

### 4. Agent Lifecycle Commands
- **Create** (`cmd/create.go`): Updated `--template-scope` default value to "project" and updated help text.
- **Start** (`cmd/start.go`): Updated `--template-scope` default value to "project" and updated help text.
- **Resume** (`cmd/resume.go`): Updated `--template-scope` default value to "project" and updated help text.

### 5. Template Resolution (`cmd/template_resolution.go`)
- Ensured "project" scope is correctly mapped to internal "grove" values for Hub API communication.
- Updated help messages and error formatting.

## Verification Results
- **Build**: Successfully ran `go build ./...` with no compilation errors.
- **Flag Compatibility**: Verified that `--project` and `--grove` flags are correctly mapped to the same internal variables where applicable.
- **Help Text**: Visually inspected updated help text in source code.

## Lessons Learned
- Broad string replacements in a large codebase require careful surgical precision to avoid breaking internal logic while ensuring consistent UI.
- Maintaining a clear mapping between user-facing terms ("project") and internal storage/API terms ("grove") is essential for incremental migrations.
