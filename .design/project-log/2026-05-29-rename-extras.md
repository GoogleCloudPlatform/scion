# Extras: grove→project Rename (Tier 1)

**Date:** 2026-05-29
**Scope:** `extras/` directory — fs-watcher-tool, scion-chat-app, scion-a2a-bridge, scion-broker-log

## Summary

Renamed internal Go identifiers, comments, and human-readable strings from
"grove" to "project" across all four extras projects. This is Tier 1 of the
rename effort — wire protocol strings, SQL column names, env vars, NATS topics,
URL paths, CLI flag names, JSON/YAML struct tags, and container labels are
intentionally preserved.

## Changes by Project

### fs-watcher-tool
- Renamed `grove.go` → `project.go` (via `git mv`)
- `GroveDiscovery` → `ProjectDiscovery`, `NewGroveDiscovery` → `NewProjectDiscovery`
- Config field `Grove` → `Project`, local var `grove` → `project`
- Log prefix `[grove]` → `[project]`
- **Kept:** `--grove` CLI flag name, `scion.grove=` Docker label filter

### scion-chat-app
- **state.go:** `SpaceLink.GroveID` → `.ProjectID`, `.GroveSlug` → `.ProjectSlug`,
  `AgentSubscription.GroveID` → `.ProjectID`, function params `groveID` → `projectID`
- **commands.go:** Updated all `client.Groves()` → `.Projects()`,
  `client.GroveAgents()` → `.ProjectAgents()`, `hubclient.ListGrovesOptions` →
  `.ListProjectsOptions`, local vars and log keys, user-facing help text
- **notifications.go:** `groveID` params → `projectID`, comments updated,
  `link.GroveID` → `link.ProjectID`
- **main.go:** `adminClient.Groves()` → `.Projects()`, `grovesResp` → `projectsResp`,
  comment and log updates
- **Kept:** `scion.grove.` NATS topic strings, `parts[0] == "grove"` wire
  protocol check, SQL `grove_id`/`grove_slug` column names, `/api/v1/groves` URL

### scion-a2a-bridge
- **bridge.go:** `grovePattern` local var → `legacyPattern`
- **server_test.go:** `[]GroveConfig{` → `[]ProjectConfig{`, comment updates
  ("wrong grove" → "wrong project", etc.)
- **Kept:** `GroveConfig = ProjectConfig` type alias (backward compat for YAML),
  `scion.grove.` topic strings, `/groves/` legacy URL routes, `SCION_GROVE_ID`
  env var fallback, `cfg.Groves` YAML field

### scion-broker-log
- No grove references needing changes (already clean)

## Preserved (Wire Protocol / External Interface)

- NATS topics: `scion.grove.<id>.user.<user>.messages`
- Container labels: `scion.grove`, `scion.grove_id`
- SQL DDL/DML: `grove_id`, `grove_slug` column names
- CLI flags: `--grove`
- Environment variables: `SCION_GROVE_ID`
- URL paths: `/api/v1/groves`, `/groves/{slug}/agents/{slug}/...`
- YAML config keys: `groves:` (legacy field)
- JSON struct tags: unchanged

## Pre-existing Issue

`extras/scion-chat-app/internal/identity/identity_test.go` has a pre-existing
vet error (`NewMapper` call missing argument) unrelated to this rename. The
error existed before the rename changes.

## Verification

- `go build ./...` — all 4 projects pass
- `go vet ./...` — all pass (except pre-existing identity_test.go issue)
- `go test ./...` — all pass
