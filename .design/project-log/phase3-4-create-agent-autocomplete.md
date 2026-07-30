# Phase 3-4: CreateAgent + Autocomplete Generalization

**Date:** 2026-07-30
**Branch:** scion/ca-mgr-scion-thread
**Phases:** 3 (HubClient.CreateAgent) and 4 (HandleAutocomplete dispatch)

## What Changed

### Phase 3: HubClient.CreateAgent

Added agent creation capability to the Discord plugin's hub client.

**Types added to `commands.go`:**
- `CreateAgentRequest` — request body with `Name` and optional `Template` fields
- `CreateAgentResponse` — response with `Slug` and `Name` fields

**Interface addition to `commands.go`:**
- `CreateAgent(ctx, projectID, req, onBehalfOf)` added to the `HubClient` interface

**Implementation in `hubclient.go`:**
- Added `longHTTPClient` field to `httpHubClient` — a separate `http.Client` with
  NO global timeout, initialized in `NewHTTPHubClient`. This is critical because
  agent creation synchronously dispatches container create+start (30-120s), while
  the default `httpClient` has a 15s timeout that would cause every create to fail.
- `CreateAgent` POSTs to `/api/v1/projects/{projectID}/agents`
- Sets `X-Scion-On-Behalf-Of` header from the `onBehalfOf` parameter for delegated
  identity (Phase 1 hub middleware)
- Signs request with existing broker HMAC
- Handles all response codes per design doc:
  - 201: success (parses agent slug/name from hub response)
  - 200: treated as conflict per decision 7b (existing agent resumed)
  - 409: slug conflict
  - 404: template/project not found
  - 400: validation error (surfaces hub message)
  - 403: permission denied

### Phase 4: HandleAutocomplete Generalization

Refactored autocomplete from hardcoded agent-only to dispatch on focused option.

**New helpers in `commands.go`:**
- `focusedOption(sub)` — finds the option with `Focused==true` in a subcommand
- `completeAgents(ctx, projectID, typed)` — extracted existing agent completion logic
- `completeTemplates(ctx, projectID, typed)` — new template completion, prefix-filters
  by slug or display name, capped at 25 choices

**HandleAutocomplete rewrite:**
- Finds the focused option first, then dispatches:
  - `"agent"` -> `completeAgents` (behavior-preserving for existing autocomplete)
  - `"template"` -> `completeTemplates` (new for `/scion thread`)
  - default -> empty choices
- Discord's 25-choice cap applied after dispatch

## Key Design Decisions

1. **Separate HTTP client for CreateAgent**: The design doc explicitly warns that
   reusing the default 15s-timeout client is "the single most likely way to ship
   a broken feature." The `longHTTPClient` has no global timeout; per-call context
   controls the deadline.

2. **200 treated as conflict**: Per design decision 7b, if the hub returns 200
   (meaning it resumed an existing agent), we return an error rather than silently
   binding a new thread to a pre-existing agent with unrelated history.

3. **Template autocomplete matches both slug and display name**: Users may type
   either; both are prefix-filtered.

## Files Modified

- `extras/scion-discord/internal/discord/commands.go` — types, interface, autocomplete
- `extras/scion-discord/internal/discord/hubclient.go` — CreateAgent implementation

## Verification

- `go build ./...` passes
- `go test ./... -count=1 -timeout 120s` passes (all existing tests)
