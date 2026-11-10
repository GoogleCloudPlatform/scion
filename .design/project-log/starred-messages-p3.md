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
  deletions refetch the loaded messages and keep the reader's place. "Jump to message in conversation"
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
- **A human DM message is stored with `uuid.Nil` as its project** (`store.CreateMessage` rejects an
  empty project); test fixtures pass `uuid.Nil.String()`, as production does.
- **Around pages page by the `(created, id)` tuple in both halves**, not by strict instants, so a
  starred message sharing the anchor's instant is found by its ID and paging older from the page
  loses nothing. Ordinary history's around still uses instant bounds.
- **`URL.Query()` drops a pair it cannot decode**, so routing on it served `?starred=%zz` as
  unfiltered history; the history handler routes on the raw query key (`rawQueryHasKey`).
- **A filter change keeps the conversation key**, so key-only guards miss it: the send-completion
  backfill checks the thread generation (`fetchId`), every ordinary backfill returns in the
  starred-only center, and the exit target is bound to its conversation key.
- **The starred-only refresh refetches around the reader** (or its nearest loaded neighbours) when the
  reader is further up than the largest page, or the loaded list is already an around page (a second
  refresh, such as an unstar followed by the hub's own stars event); otherwise the newest page, which
  clears the around state.
- **Experiment off:** a `?messages=starred` conversation URL shows a notice that all messages are
  shown, then drops the filter from the URL in place.

## Verification notes

- The pkg/hub test package is split into groups by `hack/` tooling; find the group that holds a new test at the exact commit
  before running it.
- The mocked Playwright harness (`web/e2e/chat-mobile/star-center.pw.ts`) covers the browser cases;
  the real-hub case in `web/e2e/chat-stars.spec.ts` covers the filtered view and the jump end to end.
- Chromium does not start under `ulimit -v`; local Playwright runs used a 1 s session RSS watcher
  with a kill limit instead (peak about 600 MB).

## Follow-ups

- Planned for a follow-up change: add `?x=1;starred=true` to the experiment-off 404 list in
  `TestChatStarredHistory_GateAndValidation` (the routing already sends it to the starred variant, which is a 404 while the
  experiment is off).
- Also planned for that follow-up: a web unit case where a refresh in the middle of the starred-only list leaves it empty: the empty state
  renders, there is no Jump to latest, and Show all opens the full conversation with no anchor.
