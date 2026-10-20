# webperf C9a: chat thread row-model memoization (#5312)

**Date:** 2026-10-19
**Issue:** ptone/scion#5312 (epic #5301)
**Branch:** scion/webperf-c9a (stacked on C11, #5317)

## Problem

Finding #9: every row-model rebuild in `chat-thread.ts` (`renderMessages`,
up to 500 messages) ran `formatChatDate` (Intl) and `new Date()` per message,
a linear `members.find` two or three times per row (`isSenderAgent`,
`getSenderDisplayName`), and re-sorted the inter-agent messages on each
render, although they were already sorted when fetched.

## What was already there

Main already memoized the row model (#1766): `renderMessages()` runs inside
`guard([messageRowsVersion, seenExpired])`, and `willUpdate` bumps
`messageRowsVersion` for every changed property except a deny-list
(`typingUsers`, `agents`, `pinnedToBottom`, the send-error expand flags), and
for bare `requestUpdate()` calls, because row metadata maps
(`v2MessageExtMap`, `v2AttachmentMap`, ...) are mutated in place. Typing
events therefore already skipped the rebuild. C9a builds on this counter
rather than adding a second memo.

## What changed (chat-thread.ts only)

- Deny-list extended with `ROW_INDEPENDENT_UI_KEYS`: the context menu
  (message, position, sheet), the file preview and the composer reply/edit
  bars. Checked: `renderMessages` and its helpers read none of them, and none
  of their writers mutates row metadata in place without also assigning
  `messages`.
- `rowTime(msg)`: epoch ms + date label per message, in a `WeakMap` keyed by
  the Message object, validated against `createdAt`, and dropped whenever
  `effectiveTimeZone()` differs from the zone it was built in (the zone
  controller's `requestUpdate()` triggers the rebuild).
- `senderInfo(msg)`: name + isAgent per message in a `WeakMap`, validated
  against `sender`/`senderId`/`type` (so an in-place mutation re-derives) and
  replaced when `members` changes. The member lookup is an id map plus an
  email map; when both match, the lower array index wins, which is exactly
  what the old `members.find` returned.
- Both are filled at ingest (`mergeMessages`, `fetchInteragentExchanges`);
  render falls back to deriving lazily.
- `renderMessages` uses `this.interagentMessages` directly (sorted at ingest).

## Tests

`chat-performance.test.ts`, "chat thread row model memoization":
`typing event does not rebuild row model` (real `chat-typing-received` on the
state manager, 0 builder calls); a new message, an edit and a member rename
each rebuild exactly once; UI-only state rebuilds 0 times; date labels and
sender names after ingest; first-match member semantics; replaced and
in-place-mutated messages re-derive; a display-zone change relabels dates.
Each cache invalidation was checked by hand against a mutant that removes it
(each mutant fails at least one test).

## Learnings

- Read the existing render path before designing a memo: the findings
  snapshot predated #1766, which already had the guard.
- A cache keyed by object identity is only safe if you also validate the
  fields it derives from: edits mutate message objects in place.
- Date labels depend on the user's display zone, not only on the timestamp.
