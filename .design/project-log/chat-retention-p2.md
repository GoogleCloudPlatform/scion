# Chat message retention, Phase 2

**Date:** 2026-10-31
**Branch:** scion/nc-retention-p2
**Tracking:** ptone/scion#6831 (follow-up to ptone/scion#6596)

## What was built

Phase 2 of hub-wide chat message retention (design §9 Phase 2): the
messages of deleted conversations and topics are purged a grace period
after the deletion, and attachment bytes are deleted directly.

- **Setting** `chat_retention.deleted_conversation_grace_days` (default 30,
  1..36500), in opsettings, the section schema and the admin GET/PUT. A
  change needs `expected_revision`, like `enabled` and the window, and is
  in the audit summary. JSON null resets it to 30.
- **Gating** (§7.1): the sweep now runs whenever the experiment is on and
  `enabled` is true. With no window it runs only the deleted-conversation
  purge. The settings panel says so, and its warning names the grace.
- **Phase B in the sweeper.** `grace_cutoff = now - grace` while active.
  Candidates: conversations with `deleted_at < grace_cutoff`, then web chat
  topics with `deleted_at < grace_cutoff`, each read in ID pages of 100
  from new partial `deleted_at` indexes (`conversation_deleted_at` in ent,
  `idx_webchat_topic_deleted` in both web chat DDLs). Without them each
  list would scan the table. Each candidate's messages are selected in
  chunks by `conversation_id`. A topic's legacy rows are selected by
  `thread_id = topic id AND conversation_id IS NULL`, so a linked topic's
  pre-stamp rows go too. Pending and deferred messages are skipped. The
  purge transaction, pacing, chunk adaptation, budget and deadline bound
  are the same as Phase A. The run row counts these messages under
  `deleted_conv`, and the policy records the grace and the grace cutoff.
- **Phase order and budget split.** Phase B runs first. When a window is
  set it gets at most half of the tick, and Phase A gets the rest. Phase B
  has no cursor, so each tick starts again from the first candidate. A
  finished candidate costs one index probe.
- **Attachment bytes** (§6.3). After each purge commits, every attachment
  the purge left with no link is handled in this order: re-check the link,
  delete the bytes through the AttachmentStore, then delete the
  `webchat_attachment` row only while it is still unlinked. If the byte
  delete fails, the row stays and the legacy blob GC removes the bytes
  later. Neither failure fails the tick. Artifact-backed attachments have
  no `webchat_attachment` row, so only their link is removed. The run
  counts `attachment_rows` and `attachment_bytes`. Attachment and project
  IDs must be UUIDs before they reach the blob store. Without an
  AttachmentStore, rows are left for the GC, as in Phase 1.
- **Web.** The Chat retention panel has the grace field and the
  explanation. A thread whose read watermark was purged (the watermark is
  not loaded and the history carries the retention marker) shows the New
  messages divider at the top (§8.5). The logic is in
  `chat-unread-divider.ts`.
- **Phase 1 follow-up.** The acceptance harness has an explicit
  boundary-tie row at the cutoff with its own assert, and it requires
  `rows > 0`.

## Decisions

- Phase B runs before Phase A and gets half of the budget when a window is
  set. Running it after Phase A would let a multi-day Phase A backlog delay
  the purge of deleted conversations for days.
- The store counts every deleted message under `main`. The sweeper moves
  Phase B's count to `deleted_conv`, so the purge contract is unchanged.
  A purge whose cursor has no phase stores no cursor.
- In the run counts, `attachments` still counts link rows, as in Phase 1.
  The new `attachment_rows` counts deleted metadata rows, and
  `attachment_bytes` sums their recorded sizes.
- On SQLite, `webchat_topic.deleted_at` is RFC 3339 text and is compared as
  a string. Sub-second ordering can be wrong, which a grace measured in
  days does not notice.

## Review round 1 fixes

- A deleted-conversation failure no longer holds back the main walk. A
  failing candidate is logged and skipped, so the others still go. The
  main walk runs after any Phase B error unless the tick's context is
  done, and the run keeps the first error.
- The relink race after the byte delete is logged, with the attachment
  ID, and counted in the tick result.
- On SQLite the deleted-candidate queries use `+id` for their id terms, so
  the planner cannot walk the primary key for `ORDER BY id LIMIT`. Only
  the partial `deleted_at` index serves them. An `EXPLAIN QUERY PLAN` test
  pins this; on Postgres the plan is logged.
- The panel warning leaves out the window sentence while the window field
  holds an invalid value, and shows no warning when the grace is invalid
  too.

## Known gaps

- Race, also present in the legacy GC: a message that links an attachment
  between the link re-check and the byte delete loses the bytes, while the
  row is kept by the conditional delete.
- Per-chunk run-count flush (Phase 1 follow-up) is still open.
