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
  conversation key before the page is cut. A star counts when its message exists, carries the star's key as its
  native routing key (`messages.thread_id`, which a promotion moves with the star), is in the history channel and
  is not soft-deleted, and (for a thread) its topic is live. The order is keyset-paged on values the query computes:
  activity as fixed-width UTC text (newest first, none last), or the case-folded name (the topic name, or the DM
  peer's label computed in SQL like `starUserLabel`/`starAgentLabel`), then the key. PostgreSQL compares under
  `COLLATE "C"` so both dialects use byte order.
- **Authorization:** `starredConversationAuthorizer` applies `resolveStarScope`'s rules per candidate, batched: one
  project lookup (`ListProjectSummaries`), one agent lookup and one user lookup per batch, one
  `ComputeCapabilitiesForActions` for the threads' projects, the token's self-scope check (membership cached per
  project) and agent:read per agent. Request-local caches only. A parity test compares it with `resolveStarScope`.
- **Scan:** batches of 100 candidates, at most 1,000 per response; the sealed cursor (`chat.starred_conversations.v1`,
  bound to the project scope, the sort and the identity) resumes after the last scanned candidate, so a page can be
  short, or empty with a cursor.
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
- **The rail lists a conversation by its native key and does not resolve the read-switch selector** (one query per
  conversation); the center does, so a drifted conversation shows its error there.
- **SQLite activity text** is RFC3339Nano for current writers; the query pads it to nine digits so a whole second
  sorts before its fractions. Another layout (a legacy row) sorts as having no activity.
- **`CreateTopic` stores no activity**; a topic's `last_activity_at` comes from its messages.
- **A refresh reloads as many rows as are loaded** (in pages of up to 100), and stops at a short page, so a hidden
  scan continues only on an explicit Load more.

## Verification notes

- The hub tests run per shard; locate them with `hack/hubshard` at the exact commit. Build-lane runs follow the
  project's lane process (dev-time Go runs need a lane START).
- Mocked browser cases: `web/e2e/chat-mobile/star-rail.pw.ts` (hub mock in `star-rail-mock.ts`). Chromium ran under
  a 1 s session RSS watcher with a kill limit (peak about 600 MB).
