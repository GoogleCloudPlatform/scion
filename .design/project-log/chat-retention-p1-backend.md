# Chat message retention, Phase 1 (backend)

**Date:** 2026-10-29
**Branch:** scion/nc-retention-p1
**Tracking:** ptone/scion#6596

## What was built

Phase 1 of hub-wide chat message retention, backend half: one admin-set age
window purges old messages and every row that depends on them, in small
paced transactions. The web half (settings panel, history divider, muted
quote) is a separate change.

- **Experiment** `hub.chat_retention` (server + web layers, alpha, default
  off). The hub checks it in the settings handler, the sweeper and the
  history marker.
- **Settings section** `chat_retention` (DB-only, like `messaging`):
  `enabled`, `messages_retention_days` (absent = keep forever, else 7..36500)
  and `sweep` tuning (`chunk_size` 25..1000, default 200; `pause_ms` default
  500; `tick_budget_seconds` default 30; `tick_interval_minutes` default 10).
  A stored document that does not resolve fails closed to "retention off".
  `deleted_conversation_grace_days` (Phase 2) and
  `failed_message_retention_days` (Phase 3) are not part of it.
- **Admin API** `GET/PUT /api/v1/admin/chat-retention`: hub admin, session
  only (`IRREVERSIBLE_CASCADE`), 404 while the experiment is off. A change to
  `enabled` or `messages_retention_days` needs `expected_revision`; every
  save is a compare-and-set on the read revision. The generic settings save
  path wrote no audit record, so this endpoint writes a mutation audit record
  (`hub_setting_update`, target `chat_retention`, before/after summary)
  before the write, and refuses the save if it cannot.
- **ent entities** `ChatRetentionRun` (`chat_retention_runs`) and
  `ChatRetentionCursor` (`chat_retention_cursors`), created by AutoMigrate on
  SQLite and Postgres. The cursor uses typed columns (`cursor_created`,
  `cursor_id`, `reset_at`, `last_applied_cutoff`) rather than the JSON blob
  in the design sketch.
- **Store** `store.ChatRetentionStore`, implemented in
  `entadapter.ChatRetentionStore` and reached through
  `CompositeStore.ChatRetention()`. The selection query and the purge are
  raw SQL over the `*sql.DB` the ent client and the web chat tables share.
  One transaction deletes `webchat_message_star`, `webchat_mention`,
  `webchat_message_ext`, `webchat_message_attachment` (attachment IDs
  captured), `message_addressees` and `messages`, each with
  `DELETE ... RETURNING` for per-project counts, and then upserts the
  cursor. Unlinked `webchat_attachment` rows are reported and left in place
  for the existing attachment GC.
- **Sweeper** `pkg/hub/chat_retention.go`: `RecurringSingleton`
  "chat-retention" under `LockChatRetention` (0x5C10002E), checked every
  minute, and ticking once `tick_interval_minutes` have passed since the
  last tick. It takes one policy snapshot per tick and purges only when
  experiment && enabled && a window is set. Then:
  - the cursor goes back to the start once every 24 h;
  - each chunk runs under a 5 s timeout;
  - the chunk size halves above 250 ms (floor 25) and grows 25% below
    50 ms (cap `chunk_size`), and resets every tick;
  - the sweeper sleeps `pause_ms` after each chunk and stops at the tick
    budget;
  - `last_applied_cutoff` is set when the walk returns a short chunk;
  - one sweep run row accumulates for 24 h, with the policy and per-project
    counts but never message IDs or content;
  - run rows older than 365 days are pruned.

  No COUNT or whole-table scan anywhere.
- **History marker**: `retention: {"purgedBefore": RFC 3339}` on the oldest
  page when retention is active, a cutoff is applied and the conversation
  started before it. The start is the topic's creation time, or the
  conversation row's for a DM. A legacy DM without a conversation gets no
  marker. The policy and the cutoff are cached reads (cutoff TTL 1 min);
  only a DM listed by conversation reads one row by primary key.
- **Reply previews**: a `reply_to_id` whose target is gone returns
  `{"messageId", "senderName": "", "content": "[no longer available]",
  "unavailable": true}`.

## Open questions resolved

1. **Shared connection:** yes. `CompositeStore.DB()` is the ent client's
   pool, and the web chat store is built on it. A store test shows that a
   failure late in the purge rolls back every delete
   (`TestChatRetentionStore_PurgeIsOneTransaction`).
2. **FK on `message_addressees.message_id`:** none on either dialect. The ent
   schema has no edge, and no raw DDL adds one. The explicit delete in the
   purge is what removes addressee rows. Asserted on SQLite in the store
   suite and on Postgres in the store and hub Postgres tests.
3. **`dispatch_state` residual filter:** cheap. The walk is a two-sided range
   on the `created` index from the persisted cursor, so pending and deferred
   rows below the cursor are not visited again until the daily reset. The
   filter cost is bounded by those rows plus one chunk. No
   `(dispatch_state, created)` index is needed.

## Verification

- SQLite: `go test` for `pkg/config/opsettings`, `pkg/experiments`,
  `pkg/store` and `pkg/store/entadapter`, plus vet. `pkg/hub/authzop` was
  vetted and tested, and its catalog report regenerated.
- pkg/hub tests, `go build ./...`, the Windows build and the Postgres
  variants are proved by the companion CI run (all hub shards, SQLite and
  Postgres).
- The acceptance harness `TestChatRetention_AcceptanceSQLite` runs with
  5,000 rows by default. Set `CHAT_RETENTION_ACCEPTANCE_ROWS=100000` for the
  full run, which asserts a maximum transaction time under 250 ms on
  file-backed SQLite in WAL mode. The full run is still pending.

## Follow-ups

- Add `TestChatRetention_Postgres_CascadeAndReplicaRace` to
  `HUB_INTEGRATION_POSTGRES_TESTS` in the Makefile so the Postgres CI job
  runs it.
- Phases 2 to 4 as in the design: deleted conversations and attachment
  bytes, the failed-message purge migration, operator surface and docs.
