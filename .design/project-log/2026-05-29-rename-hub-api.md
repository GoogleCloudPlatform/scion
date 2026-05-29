# Tier 1 grove→project Rename: hub, api, hubclient, wsprotocol, store

**Date**: 2026-05-29
**Author**: Developer agent
**Branch**: grove-rename2

## Summary

Completed Tier 1 (internal identifiers) grove→project renames across `pkg/hub/`, `pkg/api/`, `pkg/wsprotocol/`, `pkg/hubclient/`, and `pkg/store/models.go`.

## Key Finding: Very Few Tier 1 Targets

These packages are overwhelmingly Tier 3 (backward-compat shims). After scanning all `.go` files, only **4 files** had genuine Tier 1 renames:

| File | Change | Category |
|------|--------|----------|
| `pkg/hub/events.go` | `gid` → `pid` loop var in `PublishBrokerConnected`/`Disconnected` | Local variable |
| `pkg/hub/server.go` | `deprecateGroveEndpoint` → `deprecateLegacyEndpoint` | Unexported function |
| `pkg/wsprotocol/protocol.go` | `NewConnectMessage` param `groves` → `projectIDs` | Function parameter |
| `pkg/hub/handlers_agent_test.go` | `grove` → `project` local var + test fixture names | Test variable |

## What Was Intentionally Left Alone

~95% of "grove" references in these packages are Tier 3 backward-compatibility code:

- **MarshalJSON/UnmarshalJSON methods** — Every model type has compat shims that emit `groveId`, `grove`, `groveName` JSON fields alongside the new `projectId`, `project`, `projectName` fields. These are the wire protocol contract.
- **Exported struct fields in aux types** — `GroveID`, `GroveName`, `Groves` inside marshal helper structs.
- **API endpoint paths** — `/api/v1/groves/` route aliases remain for client compat.
- **Query parameter fallbacks** — `r.URL.Query().Get("groveId")` in multiple handlers.
- **Event subject strings** — `"grove."+projectID+".agent.status"` dual-publish subjects.
- **Filesystem paths** — `filepath.Join(globalDir, "groves", slug)` fallback for workspace migration.
- **Environment variables** — `SCION_GROVE_ID` is an external contract.

## Verification

- `go build` — all assigned packages compile cleanly
- `go vet` — clean on all assigned packages (pre-existing vet issue in `pkg/util/logging` is out of scope)
- `go test` — all tests pass (pkg/hub 123s, all others cached/fast)

## Process Notes

- The task description estimated ~93 refs in models.go, ~57 in hubclient/types.go, etc. In practice, nearly all of these are Tier 3 marshal/unmarshal code that must be preserved.
- Careful scanning of each reference was essential to avoid breaking wire protocol compatibility.
