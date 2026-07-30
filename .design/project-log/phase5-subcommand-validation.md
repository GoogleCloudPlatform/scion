# Phase 5: `/scion thread` Subcommand Registration + Validation

**Date:** 2026-07-30
**Author:** ca-dev-thread-p5
**Scope:** `extras/scion-discord/internal/discord/`

## Summary

Registered the `/scion thread` subcommand and implemented all pre-side-effect
validation logic (Phase 0, steps 0.1-0.6 from the design doc). This phase is
validation-only; actual thread creation and agent creation will be added in
Phase 6.

## Changes

### commands.go

1. **Subcommand registration** -- Added `thread` subcommand to
   `RegisterCommandsForGuild` with two options:
   - `title` (string, required, MaxLength 100)
   - `template` (string, optional, Autocomplete true)

2. **Ephemeral response** -- Added `"thread": true` to `ephemeralCommands` map
   (per design decision 6).

3. **Dispatch** -- Added `case "thread": h.HandleThread(s, i)` to the
   `HandleSlashCommand` switch block.

4. **`HandleThread` method** -- Implements all six validation steps:
   - **0.1 Resolve parent channel** -- Uses `threadParentID()` to detect if
     invoked from inside a thread; if so, targets the parent channel for
     sibling-thread creation (decision 1a).
   - **0.2 Resolve channel link** -- Uses `resolveChannelLink()` to find the
     project binding. Clear error if unlinked.
   - **0.3 Check user registration** -- Uses `store.GetUserMapping()` to verify
     the Discord user is linked to a Scion identity. Clear error if not.
   - **0.4 Slugify title** -- Uses `api.Slugify` from `pkg/api` (NOT the local
     incompatible `slugify` in `brokerauth.go`). Rejects titles that produce an
     empty slug.
   - **0.5 Check slug conflicts** -- Uses `hubClient.ListAgents()` to detect
     existing agents with the same slug. Fails with a clear error naming the
     conflict (decision 7b).
   - **0.6 Validate template** -- If a template name was provided, uses
     `hubClient.ListTemplates()` to verify it exists. Fails before any side
     effects (decision 4c).

5. **Help text** -- Added `/scion thread` to `helpText()`.

6. **HubClient interface** -- Added `ListTemplates(ctx, projectID)` method and
   `TemplateInfo` type.

### hubclient.go

- Implemented `ListTemplates` on `httpHubClient`: merges global-scope and
  project-scope active templates from the Hub API (`GET /api/v1/templates`).
- Added `hubTemplatesResponse` and `hubTemplate` response types.

## Verification

- `go build ./...` -- passes
- `go test ./... -count=1 -timeout 120s` -- all existing tests pass
- `go vet ./...` -- clean

## Design References

- Full design doc: `/scion-volumes/scratchpad/projects/chat-admin/arch-scion-thread-cmd.md`
- This is Implementation Phase 5 from the design doc's phasing table.
- Phase 5 has no dependencies on other phases and can proceed independently.
