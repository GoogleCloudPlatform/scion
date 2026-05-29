# cmd/ Production Source Files: grove→project Rename

**Date**: 2026-05-29
**Author**: Developer agent
**Commit**: 2606f523

## Summary

Renamed Tier 1 "grove" identifiers, comments, and help text strings to "project" in cmd/ production source files (non-test).

## Changes

1. **cmd/server.go**: Renamed exported constant `GlobalGroveName` → `GlobalProjectName`, updated comment and help text strings ("groves" → "projects" in server description)
2. **cmd/server_broker.go**: Updated all references to `GlobalGroveName` → `GlobalProjectName`, renamed local variable `globalGrove` → `globalProject` (was already done by prior commit), updated function comment
3. **cmd/hub_env.go**: Renamed local variable `groveAliasSet` → `projectAliasSet`
4. **cmd/hub_secret.go**: Renamed local variable `groveAliasSet` → `projectAliasSet`

## Intentionally Preserved

The following "grove" references were intentionally left unchanged as they fall outside Tier 1 scope:
- Deprecated CLI flag definitions (`"grove"` in StringVar, MarkDeprecated, MarkHidden)
- Command aliases (`Aliases: []string{"grove", ...}`)
- Flag reads (`cmd.Flags().Changed("grove")`, `GetString("grove")`)
- Subcommand Use strings (`Use: "groves"`)
- Container label strings (`Labels["scion.grove"]`)
- Filesystem path components (`"groves"` directory)
- Config key references in comments (`hub.groveId`, `grove_id`)
- Broker topic patterns (`scion.grove.X`)
- Migration function calls (`MigrateGroveToProjectData`)
- CLI mode command paths (`"grove.reconnect"`, `"config.cd-grove"`)

## Verification

- `go build ./cmd/...` passes
- `go vet ./cmd/...` passes
