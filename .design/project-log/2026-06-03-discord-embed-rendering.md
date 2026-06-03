# Discord Embed Rendering (Phase 2, Items 3, 4, 11)

**Date:** 2026-06-03
**Branch:** discord-chat

## What was done

Added rich embed rendering functions to `extras/scion-discord/internal/discord/format.go` for Phase 2 of the Discord broker plugin:

### New functions

- **`activityColor(activity string) int`** — Maps activity/status strings (COMPLETED, RUNNING, ERROR, etc.) to Discord sidebar colors using a switch statement.
- **`RenderStateChangeEmbed(msg, agentSlug)`** — Builds a colored Discord embed for `TypeStateChange` messages with title, description, timestamp, project footer, and optional summary field. All fields are truncated to Discord limits.
- **`RenderInputNeeded(msg, agentSlug, requestID)`** — Builds an embed + button components for `TypeInputNeeded` messages. Supports structured choices (rendered as Primary buttons in action rows, max 5 per row) and a default Reply/Dismiss button pair.
- **`FormatWithEmbed(msg, agentSlug)`** — Length-aware formatter: returns plain text for ≤2000 chars, an embed for ≤4096 chars, and an embed + remainder text for longer content.
- **`SplitLongMessage(text, maxLen)`** — Splits text into chunks preferring newline boundaries, falling back to rune-boundary splitting.

### Tests

Added `format_test.go` with 33 tests covering all new functions and edge cases (color mapping, truncation, button layout, length thresholds, newline splitting, content preservation).

## Observations

- The `OpenAskUserModal` function in `modals.go` was already present on the branch, resolving the pre-existing build dependency from `callbacks.go`.
- The design doc in `.design/discord-chat.md` Section 8.1 provided exact specs for color values, custom_id formats, and embed field structure, which this implementation follows precisely.
- Discord's `discordgo.Button` uses value types (not pointers), so type assertions in tests use `.(discordgo.Button)` not `(*discordgo.Button)`.
