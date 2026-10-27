# Starred messages, phase 4: Starred conversation rail

**Date:** 2026-10-25
**Issue:** ptone/scion#6168 (epic ptone/scion#5797)
**Experiment:** web.chat_starred_messages (unchanged; one new route and operation)

## What shipped

- **Hub:** `GET /api/v1/chat/starred-conversations` (optional `projectId`, `sort=activity|alpha`, `limit` 1-100,
  default 30, `cursor`). Each item is one conversation the caller can read now in which the caller has at least one
  live star: `conversationKey`, `conversationKind`, `conversationName`, `lastActivityAt`, and the project ID/name
  for threads and agent DMs (never for a DM between users). No excerpt, count or other user's state. Off is a 404;
  the query is strict (unknown or repeated parameters, a bad sort, limit, project ID or cursor are 400); a project
  the caller cannot read is a 404.
- **Store:** `MessageStarStore.ListStarredConversationCandidates` in both dialects groups the caller's stars by
  conversation key before the page is cut. A star counts when its message exists, is a message of the conversation
  under the one selector the read switch chooses, as the other star paths apply it (switch off:
  `messages.thread_id` is the key; on: `messages.conversation_id` is the key's conversation, i.e. the topic's
  non-empty `conversation_id`, or the live native conversation whose `external_ref` is the DM key), is in the
  history channel and is not soft-deleted, and (for a thread) its topic is live. The order is keyset-paged on values the query computes: activity as fixed-width UTC
  text (newest first, none last), or the name folded for ASCII letters only (the topic name, or the DM peer's label
  computed in SQL like `starUserLabel`/`starAgentLabel`, trimmed of the same Unicode whitespace as
  `strings.TrimSpace`), then the key. PostgreSQL folds and compares under `COLLATE "C"`, so both dialects give the
  same order whatever the database's locale.
- **Authorization:** `starredConversationAuthorizer` applies `resolveStarScope`'s rules per candidate, batched: one
  project lookup (`ListProjectSummaries`), one agent lookup and one user lookup per batch, one
  `ComputeCapabilitiesForActions` for the threads' projects, the token's self-scope check (membership cached per
  project) and agent:read per agent. Request-local caches only. A parity test compares it with `resolveStarScope`.
- **Scan:** batches of 100 candidates, at most 1,000 per response; the sealed cursor (`chat.starred_conversations.v1`,
  bound to the project scope, the sort and the identity) resumes after the last scanned candidate, so a page can be
  short, or empty with a cursor.
- **Shared hub file, `pkg/hub/authorized_list.go`:** `listCursorSealer` gains `SealPosition`/`OpenPosition` for
  the signing ring, and `Seal`/`Open` (unchanged on the wire) now share one private `seal`/`open` with them, so a
  ring change cannot miss either pair. A position seals with the ring's active generation in the form the ring
  emits (`p1.` with AAD `scion-list-position-v1:`, or `p2.<kid>.` with `scion-list-position-v2:<kid>:`), opens
  only while its generation verifies, and is not checked for shape (the caller does). The rail needs it: its
  position (a sort value and a conversation key) does not fit `Open`'s `created,id,binding` check, so every rail
  cursor sealed with `Seal` failed to open. A position cursor never opens as a list cursor, and the reverse
  (`list_position_seal_test.go`, legacy and ring modes: rotation, retirement, unknown or swapped generations,
  closed issuance).
- **Catalog:** operation `chat.star.conversations`, route tables, bearer matrix exclusion (a filtered list, like
  `chat.star.state`).
- **Web:** Starred is the third option of the rail's filter control (a labelled group with pressed state). Rows come
  from the endpoint (`StarredRailController` in `chat-starred-rail.ts`), grouped into a DMs section (agent DMs too)
  and project headings, sorted with the rail's comparators; Custom shows Recent. Load more conversations keeps the
  first visible row in place. Loading, retryable error, continuation and the empty collection are distinct. A row
  opens the starred-only center (`?messages=starred`); picking a filter applies or clears an open conversation's
  filter in a new history entry; leaving the starred-only center for full context (jump, Show all, a permalink)
  returns the rail to All. Only the filter is persisted (its existing key); a stored Starred is ignored while the
  experiment is off.

## Things worth knowing

- **The rail's filter follows the page only on the page's changes.** `syncStarredFilter` reacts to a change of
  `selectedKey` or `centerFilter`, never to the user's own pick, so a click on Starred is not undone before the page
  applies the filter.
- **The rail applies the read switch's selector in SQL instead of resolving it per conversation**: the handler
  passes the switch (`ByConversationID`), and the query matches the key's conversation the way the resolvers find
  it (the topic's `conversation_id`; `GetConversationByExternalRef("native", key)`, live rows only). A star counts in
  the rail exactly when it counts in the star list, the state route and the center; a thread with no conversation
  under the switch (the resolver's 409) has no stars there and no rail row.
- **The first run of the rail's hub tests found what review and the mocked browser harness could not:** the cursor above (every
  "Load more conversations" answered 400), and two test-setup bugs. A project-boundary token can be minted only for
  a project its holder belongs to (the token tests now give the holder a role in project B, so the boundary alone
  refuses project A's rows), and topic names are unique per project regardless of case (the name-tie test puts the
  tied names in two projects).
- **A stored DM key that is not canonical** (a legacy star under swapped slots) yields no rail row (`starDMPeer`
  refuses it as `resolveStarDM` does), and the state route leaves it out of a batch instead of answering 400.
- **The star routes accept a DM only under its canonical key** (`messages.DMConversationKey`: sorted slots,
  canonical IDs); the same DM with its slots swapped is a 400. A stored DM key is therefore always the external ref
  of the DM's native conversation, which the read switch and the rail's switch-on branch look up. The web client
  already sent canonical keys.
- **The switch-on DM branch repeats the partial unique index's predicate** (`external_ref <> '' AND deleted_at IS
  NULL`) so both dialects look the conversation up on that index; a SQLite plan test pins "no SCAN c".
- **Only a pick in the filter control is persisted.** A change the page drives (a full-context permalink, a jump,
  Show all) or the experiment drives (the browser flag off, or the hub answering 404) moves the rail to All and
  stops the reconcile poll without touching the stored preference.
- **SQLite activity text** is RFC3339Nano for current writers; the query pads it to nine digits so a whole second
  sorts before its fractions. Another layout (a legacy row) sorts as having no activity.
- **`CreateTopic` stores no activity**; a topic's `last_activity_at` comes from its messages.
- **A refresh reloads as many rows as are loaded** (in pages of up to 100), and stops at a short page, so a hidden
  scan continues only on an explicit Load more.

## Verification notes

- The pkg/hub tests run in parts (`hack/hubshard`); locate the new tests at the exact commit.
- Mocked browser cases: `web/e2e/chat-mobile/star-rail.pw.ts` (hub mock in `star-rail-mock.ts`).

## Follow-ups

None. The three code review follow-ups (the rail's canonical DM check, the state route's leave-out of a
non-canonical stored key, and the canonical-key test's doc comment) are done.
