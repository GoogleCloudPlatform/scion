# Starred messages, phase 2: DMs and lifecycle invariants

**Date:** 2026-10-24
**Issue:** ptone/scion#6121 (epic ptone/scion#5797)
**Experiment:** web.chat_starred_messages (unchanged; no new routes or operations)

## What shipped

- **DMs:** every star route serves agent DMs and DMs between users. The shared read-target resolver
  decides the history scope; a DM needs the caller in a user slot of the key (exact participation).
  Stars of an agent DM are stored under the agent's project (none if the agent or project is
  gone), human DM stars under no project, whatever project the message row carries. Tokens follow
  the existing DM rule (`chatTokenMayUse`): the agent's project inside a project boundary plus
  `agent:read`, a hub boundary for a human DM. A malformed DM key is a 400.
- **Labels:** one label path in `handlers_chat_stars.go` for sender and DM names, from the batched
  user and agent lookups: display name or `User`, agent name/slug or `Agent`, `Deleted user` /
  `Deleted agent` for a looked-up ID with no record, `Someone` for an unknown sender. A failed
  lookup is a 500. There was no existing neutral label; the architect fixed this contract in the
  design during the phase.
- **Deletion:** both dialects share one deletion transaction for `SetMessageDeleted` and
  `MarkMessageDeletedAndClearStars` (extension, mentions, stars, owners). PostgreSQL locks the
  `messages` row first, so an add either commits before (and is deleted) or waits and finds the
  message deleted. The chat delete handler publishes the content-free stars invalidation to each
  owner's own subject after the commit (only while the experiment is on); the browser drops the
  message from the thread's star state and the header list on `chat-message-deleted`.
- **Promotion:** both `PromoteDM` implementations update star key/project in the same transaction
  by joining to `messages WHERE thread_id = <new topic>` after the re-key. The new topic is the
  only conversation with that thread ID, so the join is exactly the moved rows, for both the
  conversation-ID and legacy thread-ID arms. The handler tells the DM's users to refetch.
- **Cleanup:** topic deletion and project deletion (after the deletion service commits) and user
  deletion (next to the scheduled-message cleanup) remove matching stars on a detached bounded
  context (`finalizeContext`); `DeleteDM` removes them in the store like scheduled messages.
- **Upkeep:** `startMessageStarUpkeep` in `Server.Start` (joined by `CleanupResources`): every
  10 minutes, 500 rows in `(user_id, message_id)` order, 5 s, one pass at a time per replica,
  resuming from the last row and wrapping. It first repairs the stored key/project of a star whose
  live message sits in another live topic (an older binary's promotion), then purges stars whose
  user, message, stored project or topic is gone, or whose message/topic is soft-deleted. It reads
  no permissions and no user status. Runs with native chat on, experiment on or off.

## Things worth knowing

- **Repair before purge.** A topic star whose stored project no longer exists is repaired to the
  topic's current project, not purged; only a DM star (whose project is the agent's) is purged on a
  missing project. The lifecycle contract test pins both.
- **PostgreSQL UUID casts in maintenance SQL.** Star columns are TEXT and the Ent keys are uuid;
  the upkeep and deletion lock cast through a regex guard (`pgUUIDText`) so one malformed stored
  value cannot fail a whole statement.
- **Browser coverage of promotion is a mocked-hub case** (`web/e2e/chat-mobile/star-promotion.pw.ts`),
  by project decision: the real-hub suite cannot create an agent record, which promotion needs. The
  hub promotion path is covered by the Go store and handler tests (both dialects, both message
  populations, rollback, single connection, access loss and regain). Human DM retrieval runs on the
  real hub (`web/e2e/chat-stars.spec.ts`).
- **Attachment-only messages.** A native send with attachments and no text cannot be persisted
  today: the message store refuses an empty body (ptone/scion#6146). The star excerpt shows
  "Attachment" for an empty stored body.

## Follow-ups (from the phase 1 list)

1. Done: seeded maintenance description notes the `starred_at` exception.
2. Done: neutral labels (see above).
3. Done: `pgDropWebchatTables` drops `webchat_message_star` and `webchat_scheduled_message`.
4. Already resolved on main: the e2e spec comments no longer contain the reviewed wording.
5. Done: at phone width the menu helper asserts exactly one open action sheet.
6. Done: the history regression test asserts byte-identical 404 bodies for missing and unreadable
   threads and no `canPost` on any refused read.
7. and 8. belong to later work (8 is the phase 5 menu fix).
