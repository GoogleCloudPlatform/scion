# WS-2 Docs: Messaging Page Update for S4 Conversation Model

**Date:** 2026-08-27
**Task:** S5 WS-2 — Update docs site messaging page for conversation model changes
**File:** `docs-site/src/content/docs/hosted/user/messaging.md`

## Summary

Updated the Messaging & Notifications documentation page to reflect the conversation model changes shipped in S4 of the messaging-v2 refactor.

## Changes Made

### 1. New Section: Conversation Model
Added after "Real-Time Delivery", before "Developer Guide". Covers:
- What the conversation model is (thread tracking, participant-aware routing, Conversation records)
- Current status: read switch (`ConversationReadSwitch`) is default OFF
- How to enable: `messaging.conversation_read_switch: true` in `scion-settings.yaml` (runtime flag)
- Divergence monitoring via `GET /api/v1/admin/messaging/divergence` with field descriptions
- Recommendation to enable only after zero mismatches over sustained traffic

### 2. Updated CLI Message Management Section
- Added "Sending Messages" subsection showing `@<agent-name>` conversation reference usage
- Added availability caveat note: `conv:<id>` and `#<thread>` are gated (DEF-5), `@<email>` is agent-only
- Added "Additional Subcommands" table: `scion broadcast` and `scion keys`

### 3. New Developer Guide Subsection: Conversation References (§6)
- Table documenting all four conversation reference forms with availability status
- `@<agent-name>`: Available
- `@<email>`: Agent-only (requires SCION_AGENT_NAME)
- `conv:<uuid>`: Not available (CLI-gated)
- `#<thread-name>`: Not available (CLI-gated)
- Caution admonition reinforcing availability constraints

### 4. New Developer Guide Subsection: Deprecated Flags (§7)
- Table of all deprecated flags with their replacements:
  - `--broadcast`/`--all` → `scion broadcast`
  - `--raw` → `scion keys`
  - `--in`/`--at` → `scion schedule message`
  - `--notify` → `scion notifications subscribe`
  - `--channel`/`--thread-id` → `@<agent-name>`
  - `--cc` → `--to`
  - `--plain` → no replacement, will be removed
- Note that all deprecated flags still work with stderr warnings

## Availability Caveats Enforced
- Conversation read switch documented as default-OFF with explicit enable instructions
- `conv:<id>` and `#<thread>` documented as NOT available in the CLI
- `@<email>` documented as agent-container-only
- `@<agent-name>` documented as the only fully available conversation reference form
