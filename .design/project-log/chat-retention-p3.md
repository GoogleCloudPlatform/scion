# Chat message retention, Phase 3 (failed-message purge)

**Date:** 2026-10-31
**Branch:** scion/nc-retention-p3
**Tracking:** ptone/scion#6832

## What was built

Design §7.4 / §9 Phase 3: the hourly failed-message purge now uses the
Phase 1 retention cascade instead of one unbounded `DELETE`. It is not
gated by `hub.chat_retention` and keeps its 7-day default.

- **Setting** `messaging.failed_message_retention_days` (default 7, minimum
  1; a stored value below 1 reads as the default). It is in the messaging
  section schema, `OperationalSettings.FailedMessageRetentionDays()` and
  `GET/PUT /api/v1/admin/messaging` (null resets it), and in the user
  messaging page. It replaces
  `Config.FailedMessageRetentionDays`, which nothing ever set. The handler
  reads it on every run, so a change takes effect without a restart.
- **Store** `ChatRetentionStore.SelectFailedMessages` is a capped scan
  over the partial `message_failed_created_id` index (`(created, id) WHERE
  dispatch_state = 'failed'`). That index came from the upstream change
  (#4427) that P3's merge supersedes, and is kept exactly as upstream wrote
  it. The scan reads at most `scanLimit` failed rows below the cutoff from
  a `(created, id)` keyset. `dispatch_state = 'failed'` is a literal, so
  SQLite and a Postgres generic plan can use the partial index. The
  recipient check (`recipient LIKE 'agent:%'`) is computed in SQL per row,
  so `LIKE` behaves on each dialect as it did in the old `DELETE`. It returns
  up to `limit` of them, the position to resume from (the last candidate
  when the limit cut the scan, else the last row read) and whether the
  range below the cutoff is exhausted. `PurgeFailedMessages` runs the same cascade
  transaction as `PurgeChatMessages` but writes no cursor, and counts the
  deleted messages under the new `ChatRetentionCounts.Failed` instead of
  `Main`.
- **Handler** `failedMessageRetentionHandler` keeps its schedule (60 min),
  its `LockFailedMessageRetention` lock and its 30 s deadline. It runs
  `chatRetentionSweeper.PurgeFailed`. Each step is one capped scan of at
  most 1000 rows (`chatRetentionFailedScanCap`) and a purge transaction of
  the failed rows the scan found, each under the 5 s chunk timeout. Pacing
  is the chat retention sweep's: `sweep.chunk_size` as the limit, with
  adaptive shrink and grow, `pause_ms` after each step, and a budget of
  `tick_budget_seconds` that also stops before the handler deadline. Each
  purge deletes the dependent rows with its messages, which fixes the
  `message_addressees` orphans the old `DELETE` left.
- **Cursor** (phase `failed` in `chat_retention_cursors`). A run that stops
  before the cutoff (budget, deadline, error) saves its position, also when
  the handler's context is done, and the next run resumes from it. A run
  that reaches the cutoff completes the pass and saves the cursor at the
  start, so the next run begins again at the oldest failed row. A row can
  turn failed long after it was created (a pending message expires after
  24 h; a dispatched or long-deferred message fails on dispatch), behind
  the position of a pass in progress; the next pass reaches it. A full pass
  reads only failed rows through the partial index, so it is cheap.
- **Run counts.** When chat retention is active (experiment and `enabled`)
  and a sweep run started in the last 24 h, the counts are added to that
  run under `failed`. The update holds the in-process tick guard and
  `LockChatRetention`. If a tick holds either, the counts are kept on the
  server's chat retention state. The chat retention handler adds them to
  the run after its next tick, while it holds the guard and the lock, or
  the next failed purge adds them with its own. The purge never opens a
  run.
- **Removed** the trailing `PurgeOrphanMentions` call and the method (its
  only caller), `MessageStore.PurgeOldMessages` (dead) and
  `MessageStore.PurgeFailedMessages`. There is no deprecation path (Alpha
  rule).
- **Docs:** the messaging key lists in `reference/api.md` and
  `reference/server-config.md`.
- **Web:** no change. The server-config page shows only the cross-project
  switch from the messaging endpoint, not the other messaging keys.

## Decisions

- Merge with upstream #4427, which had chunked the old ent `DELETE` with
  an in-memory cursor and added the failed index. P3 keeps the index and
  replaces the rest with the cascade path. `PurgeFailedMessagesChunk`,
  `FailedMessagePurgeChunk`, `store.MessagePosition` and the server's
  `failedPurge` state are removed. Upstream's tests were ported to the new
  path, or dropped where superseded; the dispositions are in the review
  report.
- The scan cap stays (1000 failed rows per statement) and the cursor stays,
  but only to resume an unfinished pass. With the index, a scan reads only
  failed rows, so the cap matters only when many failed "user:" rows sit in
  the range. An earlier revision kept the cursor at least 24 h behind the
  cutoff and never reset it; that missed rows that turned failed behind it
  more than 24 h after creation (a dispatched or long-deferred message that
  fails days later) while chat retention was off. Resetting at the end of
  each pass, as upstream #4427's in-memory cursor did, reaches them.
- Kept counts live in one replica's memory. A restart before they are
  added loses them from the run (the rows are still purged), and they go to
  whichever run is open when they are added.
- Without the chat retention store (web chat tables not initialized), the
  handler logs a warning and deletes nothing. It used to fall back to the
  ent `DELETE`.

## Tests

- `pkg/store/entadapter` (SQLite by default, Postgres in the store
  Postgres job): `TestChatRetentionStore_SelectFailedMessages` and
  `TestChatRetentionStore_PurgeFailedMessages` cover the old row set
  (failed, agent recipient, strictly older), ties, the limit and the
  cursor; `TestChatRetentionStore_SelectFailedMessagesScanCap` covers
  sparse failed rows over capped scans. They also check the cascade, counts under `Failed`, that no
  cursor is written, and idempotence. These replace the old `MessageStore`
  purge tests.
- `TestFailedMessageScan_UsesFailedIndex` checks the SQLite plan of the
  exact statement `SelectFailedMessages` issues. From the start and from a
  cursor, the plan names `message_failed_created_id` and has no temp
  B-tree sort. `TestMessagesFailedCreatedIDIndex_AutoMigrate` (upstream,
  unchanged) pins the index on both dialects.
- `pkg/hub`: unit tests of the paced walk (capped scans over
  sparse rows, resume from the cursor, a completed pass restarts at the
  oldest failed row so a row that fails behind the cursor is purged),
  SQLite handler tests on the real web chat tables (same rows, no orphans,
  backlog over several runs and a dispatched row that fails behind the
  cursor, window change without restart, counts added to an open sweep
  run, counts deferred
  while a tick holds the run, no store), admin messaging
  validation, schema and fallback, and member/admin authz.
- `pkg/hub` `-tags integration`:
  `TestFailedMessageRetention_Postgres_SameRowsNoOrphans` (60 rows in
  chunks of 25, then a dispatched row as old as them fails and the next
  run purges it).

## Known gaps and follow-ups

- None beyond the decisions above.
