# Discord Component Interaction Dispatch & Modal Handling

**Date:** 2026-06-03
**Scope:** Phase 2, items 5, 6, 10 from discord-chat.md

## Changes

### callbacks.go — Ask-user and notification dispatch

Added three new callback prefixes to `Dispatch()`:

- **`ask:opt:<requestID>:<index>`** — Choice button click. Looks up PendingAskUser from store, validates (not expired, not already responded, index in range), extracts the choice text, delivers a StructuredMessage (TypeInstruction) to the hub targeting the agent, marks responded, and updates the original message to show the selection with buttons removed.

- **`ask:reply:<requestID>`** — "Reply" button for free-text. Opens a Discord modal via `OpenAskUserModal`. This is a special case: the broker does NOT pre-acknowledge this interaction with DeferredMessageUpdate, because the modal response must be the first interaction response.

- **`ask:dismiss:<requestID>`** — "Dismiss" button. Marks the request as responded and updates the message to "Dismissed." with buttons removed.

- **`notif:on:<agentSlug>` / `notif:off:<agentSlug>`** — Toggle notification preferences. Looks up channel link for project context, persists the preference via `SetNotificationPref`.

Added `deliverInbound` function field to `CallbackHandler` (injected by broker) for hub delivery. Updated `NewCallbackHandler` signature accordingly.

### modals.go — New file

- **`OpenAskUserModal()`** — Responds with `InteractionResponseModal` containing a single TextInput (paragraph style). CustomID format: `ask:modal:<requestID>`.

- **`HandleModalSubmit()`** — Parses `ask:modal:<requestID>` custom_id, extracts text from modal components, validates pending request, builds and delivers StructuredMessage to hub, marks responded, edits original message to show response summary with buttons removed, sends ephemeral confirmation.

- **`extractModalTextValue()`** — Walks ActionsRow → TextInput component tree to extract the submitted text.

- **`respondEphemeral()`** — Helper for sending ephemeral follow-up messages after deferred acknowledgment.

### broker.go — Wiring

- **Component interaction dispatch**: Added special case for `ask:reply:` prefix — skips the `InteractionResponseDeferredMessageUpdate` auto-acknowledgment so the callback can respond with a modal instead.

- **Modal submit dispatch**: Replaced the TODO stub with actual routing. For `ask:` prefix modals: acknowledges with deferred ephemeral message, then dispatches to `HandleModalSubmit()` in a goroutine.

- Updated `NewCallbackHandler` call to pass `b.deliverInbound`.

## Design Decisions

1. **ask:reply skips auto-acknowledge**: Discord only allows one initial response per interaction. Since `ask:reply` needs to open a modal (which IS the initial response), the broker must not pre-acknowledge. All other component interactions continue to use `DeferredMessageUpdate`.

2. **Hub delivery via function injection**: Rather than duplicating the HTTP delivery logic, `CallbackHandler` receives a `deliverInbound` func from the broker. This keeps the callback handler testable and avoids coupling to the HTTP client.

3. **Modal submit uses deferred ephemeral + follow-up**: The broker acknowledges modal submissions with `DeferredChannelMessageWithSource` (ephemeral), then `HandleModalSubmit` sends a follow-up confirmation via `respondEphemeral`. This gives us time to process the request while keeping the user informed.

## Verification

- `go build ./...` passes
- Existing tests pass (`go test ./...`)
