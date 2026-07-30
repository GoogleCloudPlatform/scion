# Phase 6-7-8: Thread Command Orchestration, Binding, and Hub Probe

**Date:** 2026-07-30
**Branch:** scion/ca-mgr-scion-thread
**Author:** ca-dev-thread-p6p7

## Summary

Replaced the HandleThread validation-only stub with the complete orchestration
lifecycle: concurrent fan-out for agent + thread creation, full error
compensation matrix, binding + kickoff, and hub capability probe placeholder.

## Changes

### `extras/scion-discord/internal/discord/commands.go`

#### New: `isForumChannelType` helper
Standalone function that detects forum (type 15) and media (type 16) channels
using a `*discordgo.Session`. Unlike `DiscordBroker.isForumChannel`, this
helper does not require broker state and can be used from `CommandHandler`.

#### Phase 6: Concurrent fan-out
After all validation passes (steps 0.1–0.6, unchanged from Phase 5):

- **Goroutine A (CreateAgent):** Uses a 5-minute `context.WithTimeout` against
  the long-timeout hub client. Sends `X-Scion-On-Behalf-Of: user:<email>` for
  delegated identity attribution.
- **Goroutine B (Create thread):** Branches on `isForumChannelType`:
  - Forums: `ForumThreadStartComplex` with `ThreadStart{Name, AutoArchiveDuration: 10080}`
    and a status `MessageSend`.
  - Text channels: `ThreadStart` + `ChannelMessageSend` into the new thread.

Both goroutines join via `sync.WaitGroup`, then the four-outcome error
compensation matrix handles every combination:

| Agent | Thread | Action |
|-------|--------|--------|
| OK    | OK     | Continue to binding |
| FAIL  | OK     | Edit status message with error; ephemeral reply |
| OK    | FAIL   | Ephemeral reply naming the created agent slug |
| FAIL  | FAIL   | Single ephemeral error |

#### Phase 7: Binding + Kickoff (both-OK path)
1. `SetThreadDefault(parentChannelID, threadID, agentSlug)` — binds thread routing
2. `SetConversationContext` — pre-seeds outbound route so agent replies land in
   the new thread
3. `deliverInbound` kickoff message asking the agent to introduce itself
4. Edit status message: "Creating..." → "✅ Ready — agent `slug` (template: t)"
5. Ephemeral followup with jump link: `https://discord.com/channels/<guild>/<thread>`
6. Bind failure handled as success-with-caveat: tells user to run `/scion default`

#### Phase 8: Hub capability probe
Added as a TODO comment. The full probe (checking whether the hub supports
`X-Scion-On-Behalf-Of` before creating anything) is deferred. The comment
documents the approach: verify the created agent's OwnerID matches the
expected user after creation.

## Testing

- `go build ./...` — passes
- `go test ./... -count=1 -timeout 120s` — all tests pass
- No new test doubles needed; existing mock structures satisfy the interface

## Design Decisions

- Used `Msg` field (not `Body`) for `StructuredMessage` kickoff — matches the
  actual struct definition in `pkg/messages/types.go`.
- Forum thread starter message ID equals the thread's channel ID (per discordgo
  `ForumThreadStartComplex` semantics).
- Kickoff message delivery failure is non-fatal — the agent is bound and
  reachable; the user can message it directly.
- Template label in status message defaults to "default" when no template is
  specified, matching the hub's fallback behavior.
