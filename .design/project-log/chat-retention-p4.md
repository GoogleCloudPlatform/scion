# Chat message retention, Phase 4 (operator surface and docs)

**Date:** 2026-11-02
**Branch:** scion/nc-retention-p4
**Tracking:** ptone/scion#6833

## What was built

Phase 4 of hub-wide chat message retention (design §9 Phase 4; §6.5, §7.2,
§8.3), on top of Phases 1 to 3.

- **`chat-retention-preview`** (routine maintenance operation,
  `pkg/hub/chat_retention_ops.go`). It walks the deleted-conversation phase
  and then the main window in the sweep's order, chunk size (adapted to the
  selection time) and pause, and deletes nothing. The deleted-conversation
  phase is paged through the new store method
  `SelectChatRetentionConversationPage` ((created, id) keyset within one
  conversation or legacy topic), because the purge's own selection only
  reads the first rows of what is left. The main walk starts at the
  beginning of the created index, not at the sweep's cursor, and skips the
  messages the first phase counted (by conversation, or for a legacy topic
  by thread_id with no conversation; candidates now carry `ThreadID`), so on
  an unchanged database the counts equal a following run's. The policy is
  the saved section as if enabled; the optional `messages_retention_days`
  (a JSON string, 7..36500) replaces the window. The result is a
  `chat_retention_runs` row (`trigger=preview`, `dry_run=true`, actor,
  policy, counts under `main` and `deleted_conv`), written at start and at
  end so the history shows a running preview.
- **`chat-retention-run`**: the sweep once, without the tick budget (the
  pause stays; a 24 h cap through the maintenance run timeout, with the
  walk stopping early enough for the final writes). It waits for the
  in-process tick guard and `LockChatRetention`, polling every 2 s, then
  takes the policy snapshot, and records a `trigger=manual` row with the
  actor. `RunTick` was split so both share `sweepAndRecord` (phases, cursor
  and last applied cutoff, run status, prune); a manual run gives each
  phase the whole time instead of halving it. Refused before a run is
  created with `409 chat_retention_disabled` while the sweep is not active.
- **Maintenance wiring.** Both keys are seeded as routine operations, have
  a parameter allowlist and a run timeout, and are hidden from the list and
  answer `404 Operation not found` on every route while `hub.chat_retention`
  is off (`maintenanceOperationExperiments`). `executeOperation` passes the
  caller's user ID to executors that implement `maintenanceActorSetter`.
  The preview refuses non-string parameters (`parseMigrationParams` drops
  them silently, which would have previewed the saved window).
- **Admin API.** `GET /api/v1/admin/chat-retention` adds `runs` (10 most
  recent rows, totals and project count only), `last_preview` and
  `purged_since_preview` (messages deleted by non-dry runs that finished
  after the preview started), read through the new
  `ListChatRetentionRuns(filter)`. The estimate errs towards a smaller
  backlog, since a sweep run open since before the preview counts whole.
- **Around jump.** Every anchor that cannot be shown (malformed, missing,
  purged, another conversation) answers `404 message_unavailable`
  ("Message not found"); the cases are not told apart.
- **Web.** The Chat retention panel lists the runs, the last preview and a
  backlog estimate (while enabled), starts a preview of the window in the
  form and polls its run until it ends, then reloads only the history (the
  form keeps its edits). A save that enables the sweep, sets a first
  window or shortens it opens a confirmation dialog with the last preview
  and a note when that preview was for another window. While enabled with
  neither field valid, the panel says so and disables Save. The thread
  shows "That message is no longer available." when an explicit jump gets
  `message_unavailable`; a scroll-position restore stays silent.
- **Docs.** `reference/admin-settings.md` (preview and run, run history,
  failed-message retention, the around response, the large-hub runbook and
  the SQLite file size note), `reference/api.md` and the maintenance list in
  `hosted/single-node/hub-server.md`.

## Follow-ups folded in

- ptone/scion#7088: the failed-message purge now deletes the bytes and then
  the row of each attachment it leaves unlinked (`deleteAttachments` returns
  its counts so both purges use it), and its handler wires the attachment
  store. The store test asserts the unlinked row survives the store purge.
- Panel: the both-fields-invalid no-save state.
- `walkDeleted` keeps a candidate's earlier error when a candidate list
  fails afterwards.
- Comment re-wraps; blank line in `pkg/store/chat_retention.go`.

## Tests

- Unit (fake store): preview equals a manual sweep per project and phase,
  deletes nothing and keeps the pause; hypothetical and absent windows;
  errors and cancellation; the window parameter; the first-error order;
  failed purge attachment bytes; run JSON.
- SQLite (real web chat tables): preview equals run with actor and dry-run
  rows; the manual run waits for a held tick guard and is refused while
  disabled; the operations over HTTP (404 and hidden while the experiment is
  off, 403 for a member, invalid and non-string window refused, run history
  in the settings GET, no project IDs in it); around `message_unavailable`.
- Postgres (integration tag): preview equals purge.
- Store (both dialects): the conversation page walk and the run listing.
- Web: summary, backlog and confirmation helpers; history table; the
  confirmation flow (cancel, confirm, shorten, lengthen without asking);
  starting a preview; the invalid-form state; the toast.

## Notes for later work

- A manual run or preview saves its row at start and end only. A crash
  leaves it `running`; nothing resets chat retention run rows the way
  `AbortRunningMaintenanceOps` resets maintenance runs.
- The preview's conversation pages sort within one conversation (no index
  orders a conversation's messages by (created, id) across channels), so a
  very large deleted conversation costs more per page than the purge does.
- The panel polls a started preview every 2 s, for up to 30 minutes.
