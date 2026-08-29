# Phase 3 Backend: dryRun Support for Cascade Endpoint

**Date:** 2026-08-29
**Agent:** dev-phase3-backend
**Branch:** scion/dev-phase3-backend

## Summary

Added `dryRun=true` query parameter support to the `set_message_mode` cascade
endpoint. This allows the frontend to preview which agents will be affected by
a cascade operation before applying it.

## Changes

### `pkg/hub/handlers_agent_message_mode.go`

1. **New `CascadeAgentDetail` struct** — per-agent transition detail with
   `agent_id`, `agent_name`, `current_mode`, and `new_mode` fields.

2. **Enhanced `CascadeResult`** — added `Details []CascadeAgentDetail` field
   (populated for both dry-run previews and actual cascade operations).

3. **`handleSetMessageMode` dry-run path** — reads `?dryRun=true` from the
   query string. When set:
   - Parses and validates the request normally
   - Fetches the agent and enforces full D7 authorization (no bypass)
   - Calls `cascadeMessageMode` in preview mode (dryRun=true)
   - Includes the root agent in the response details
   - Returns the preview without modifying any agent records
   - Implicitly sets `cascade=true` when `dryRun=true` (dry-run's purpose is
     to preview cascade effects)

4. **`cascadeMessageMode` dryRun parameter** — when `dryRun=true`, the function
   uses the same query and filtering logic but skips `UpdateAgent` calls and
   audit emission. This ensures the preview cannot disagree with what apply
   would do.

5. **`agentDisplayName` helper** — returns the best display name for an agent
   (prefers Name → Slug → ID).

## Design Decisions

- **Same code path for preview and apply:** The `dryRun` flag is a parameter to
  `cascadeMessageMode` rather than a separate function. This guarantees the
  preview always reflects what a real apply would do.

- **Root agent included in preview:** The dry-run response includes the root
  agent as the first entry in Details, so the frontend can show the complete
  picture of all mode transitions.

- **Implicit cascade on dry-run:** When `dryRun=true`, cascade is automatically
  enabled even if not set in the body, since the only purpose of dry-run is to
  preview cascade effects.

## Verification

- `go build ./pkg/hub/...` passes with no errors
