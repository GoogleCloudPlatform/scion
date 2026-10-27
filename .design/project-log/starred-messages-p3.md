# Starred messages, phase 3: starred-only center and jump back to full context

**Date:** 2026-10-24
**Issue:** ptone/scion#6140 (epic ptone/scion#5797)
**Experiment:** web.chat_starred_messages (unchanged)

## What shipped

- **Hub:** `GET /api/v1/chat/conversations/{key}/messages?starred=true` answers the ordinary history
  response with only the caller's live starred messages of the conversation, newest first by
  `(created, id)`. The experiment is checked before anything else (off is a 404, never unfiltered
  history); only the literal `true` is accepted; the query is strict (unknown or repeated parameters,
  a bad limit, cursor plus around are 400). The conversation is resolved like the other star reads
  (`resolveStarScope`: the shared read target, DM participation, `inbox:read` for a token), so a token
  that can read ordinary topic history with `project:read` alone still gets 403 here. The cursor is
  sealed and bound to the conversation, the caller and the filter (`chat.history.starred.v1`); an
  ordinary history cursor and a starred one are not interchangeable in either direction. `around`
  must itself be a live starred message of the conversation. `totalCount` is the conversation's live
  starred messages. A never-used DM is an empty page.
- **Store:** `MessageStarStore.ListStarredHistory` in both dialects returns ordered `(created, id)`
  positions; the handler loads the messages through `GetMessagesByIDs`, so they are mapped exactly as
  ordinary history maps them. On SQLite the comparisons and order use `messages.created` with the
  legacy monotonic suffix stripped (the entadapter's `timeColumnExpr` form).
- **Shared enrichment:** `chatHistoryPage` (attachments, artifacts, extensions with the deleted-text
  strip, reply previews of this conversation) now serves both history variants.
- **Catalog:** operation `chat.conversation.history.starred` (variant `starred` on the history
  route), with its bearer-matrix query override and companion selector.
- **Web:** `?messages=starred` on a conversation route (thread, readable slug route, DM) opens the
  starred-only center: standalone rows, a "Starred messages only" banner with Show all messages in
  place of the composer, its own loading and empty states. No read watermark, unread divider,
  typing indicator, inter-agent exchanges or live appends there; star invalidations, edits and
  deletions refetch the window and keep the reader's place. "Jump to message in conversation"
  resolves the star, then opens the full conversation at the message in a new history entry (Back
  returns to the filtered view and its position); Reply, Edit, a reply preview and a header entry
  open the full conversation first.

## Things worth knowing

- **The thread reloads on a filter change as on a conversation switch** (`updated()` resets on
  `messageFilter` alone), so `fetchId` makes every request generation `(conversationKey, filter)`.
- **`replaceRoute` keeps the query and fragment**, so slug canonicalization keeps the filter by
  itself. The risk is the other way: a filter on a non-conversation route (a space) would be carried
  along, so the page drops it there (`syncMessageFilterFromUrl`). `/chat/space/{id}` looks like the
  readable two-segment thread route; `isChatConversationPath` excludes it first, as the router does.
- **`pushChatPath` compared pathnames only**; with the filter the query matters (the filtered and the
  full view are separate entries), so it now compares path plus query.
- **Back to a filtered URL re-renders the page** (the router builds a new page instance), so the
  filtered position is kept in memory per conversation (`rememberFilteredScrollAnchor`) and restore
  anchors carry the filter they were taken under; a full-history position is never applied to the
  filtered list or the reverse.
- **Edit/Delete eligibility uses "no agent reply after"**, computed from the loaded list. The filtered
  list omits messages, so the filtered menu has no Delete and Edit opens full context first, where
  the check runs against the real neighbours.
- **Lit calls `requestUpdate` on every `@state` assignment**, also an unchanged one, and a page test
  pins "no update when nothing changed": assign only on change.
- **New test files using the SQLite fixtures need `//go:build !no_sqlite`** like their siblings,
  or the no_sqlite build breaks.

## Verification notes

- The hub tests run per shard; locate the new tests with `hack/hubshard` at the exact commit.
- The mocked Playwright harness (`web/e2e/chat-mobile/star-center.pw.ts`) covers the browser cases;
  the real-hub case in `web/e2e/chat-stars.spec.ts` covers the filtered view and the jump end to end.
- Chromium does not start under `ulimit -v`; local Playwright runs used a 1 s session RSS watcher
  with a kill limit instead (peak about 600 MB).
