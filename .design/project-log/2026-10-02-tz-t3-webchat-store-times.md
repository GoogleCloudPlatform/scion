# tz-refactor task 3: raw webchat store times in UTC

Closes ptone/scion#2496. Refs ptone/scion#2457. Design: tz-refactor design §2.1.4, §2.1.5.

## What changed (`pkg/hub/webchannel_store.go`, SQLite store only)

- `TouchThread` and `RecordChannel` bind `.UTC().Format(time.RFC3339Nano)`. Before, they bound a
  raw `time.Time`, which modernc stores as `time.Time.String()` text (`'… +0900 JST m=…'`). No
  reader parsed that, so `GetThreads` returned a zero `LastActivityAt`.
- The attachment, edit and delete writers add `.UTC()` (they stored local offsets).
- Every webchat time reader returns UTC: `parseSQLiteTime`, the `GetThreads` parser (now a
  `parseSQLiteTime` call), and the `GetMessageExt(s)` readers (they called `time.Parse(RFC3339Nano)`
  directly). `parseSQLiteTime` falls back to the new shared parser for legacy `String()` rows.
- New `pkg/hub/time_string_parse.go`: `parseGoTimeString`. It accepts any zone abbreviation,
  alphabetic or numeric (four-digit numeric too, e.g. `+0545 +0545`), and an optional ` m=…`
  suffix, and returns UTC. tz-refactor task 6 (`utc-timestamp-normalize`) should reuse it.
- `SearchChatMessages` parses the RFC3339Nano cursor and binds it as a UTC `time.Time` against the
  ent `messages.created` column. Compared as text, `T` > ` `, so the cursor never advanced and
  paging looped forever. A cursor with an unparseable timestamp now returns
  `ErrInvalidSearchCursor`, which the chat search handler maps to `400 invalid_cursor` (review
  round 1). The Postgres twin still returns 500 for that case: existing behaviour, a follow-up candidate.
- All four raw `INSERT INTO conversations` sites (CreateTopic, EnsureGeneralTopic, PromoteDM,
  backfillTopicConversations) bind a UTC `time.Time` for `last_activity_at`/`created_at`. The
  `webchat_topic` columns keep RFC3339Nano text.
- The Postgres twin is unchanged.

## Tests

- `time_string_parse_test.go`: one table case per zone in design §2.1.6 (Kathmandu, nameless
  `FixedZone(+3h)`, `FixedZone("XYZ",-5h)`, JST, UTC, Sao_Paulo `-03`, Lord_Howe DST `+11`,
  Kolkata `IST`, Dubai `+04`), each with and without ` m=…`; also literals and rejects.
- `webchannel_store_time_test.go` uses the production modernc driver. The older webchat tests
  use mattn `sqlite3`, which formats bound times differently and hides these bugs. It covers the
  round trips, legacy rows (RFC3339 `+09:00`, alphabetic, four-digit numeric and nameless-zone
  `String()`), `SearchChatMessages` paging on the ent-migrated schema (`createTestStore`,
  ent-written rows, `Limit: 2`, a timestamp tie, at most 10 pages), the conversations-insert text
  form at all four sites, and conversation-list order and keyset paging over mixed ent and webchat rows.
- The hand-made `messages` table helper in `handlers_chat_v2_test.go` now uses modernc and a
  `DATETIME` column bound with `time.Time`, so it stores what ent stores. With RFC3339 text it hid
  the cursor bug.
- Revert spot-checks (TZ=Asia/Tokyo): reverting each of the 13 fixes, one at a time, makes at least one new test fail.
- TZ runs: all touched tests pass under UTC and Asia/Tokyo. Under Asia/Kathmandu the
  non-`createTestStore` tests pass; the `createTestStore`-backed ones fail only with the known
  upstream baseline (`empty agent role backfill: Scan error on column create_time`), which
  tz-refactor task 2 fixes.

## Follow-ups (not done here)

- Many webchat tests still open the mattn `sqlite3` driver, which does not match production.
  Moving them to modernc would let them catch driver-format bugs like these.
