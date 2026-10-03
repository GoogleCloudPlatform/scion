# tz-refactor task 6: utc-timestamp-normalize maintenance migration

**Date:** 2026-10-02
**Branch:** scion/tz-t6 (stacked on tz-refactor task 3)
**Issue:** ptone/scion#2499 (part of ptone/scion#2457; see also ptone/scion#2473)

## What changed

- **Shared parser moved.** The stored-timestamp parser moved from `pkg/hub` to the leaf package
  `pkg/store/storedtime`, so that the webchat readers in `pkg/hub` and the normalizer in
  `pkg/store/entadapter` share one implementation without an import cycle. The move is a separate
  commit with no behaviour change. `parseSQLiteTime` keeps its name, signature and
  zero-on-failure contract. A follow-up commit drops the input text from parse errors.
- **Normalizer.** `entadapter.NormalizeUTCTimestamps` works on SQLite and Postgres.
  - On SQLite it rewrites ent time columns (enumerated from `migrate.Tables`) to
    `t.UTC().String()`, and `webchat_*` `*_at` columns (found with `pragma_table_info`) to
    RFC3339Nano `Z`.
  - On both backends it rewrites JSON-embedded times (`access_policies.conditions`
    validFrom/validUntil, `agents.exposed_ports[].exposedAt`) to RFC3339Nano UTC. Postgres
    scalar columns are `timestamptz` and need no rewrite.
  - Values are read with `CAST(col AS TEXT)`, so rows that ent cannot scan (a four-digit numeric
    zone abbreviation such as `+0545 +0545`, or a nameless FixedZone) are still reached.
  - It works per table in rowid batches, one transaction per batch, with compare-and-set updates
    (`WHERE rowid = ? AND CAST(col AS TEXT) = ?`). A concurrent live write is never overwritten,
    and an interrupted run resumes by running again.
  - Unparseable values are left unchanged and logged by table, column and rowid only.
  - The hub SQLite connection pool has one connection, so each batch is read and closed before
    its transaction opens.
- **One canonical predicate.** A single SQL GLOB predicate selects the rows to rewrite and drives
  the startup probe, so the two cannot disagree. A fraction is canonical when it is digits ending
  in a non-zero digit, which is what Go writes. A first version needed two digits and flagged
  `.5`; the hub fixture caught it, and a direct predicate test now checks against Go's output.
- **Boot-time repair of unreadable tables.** On SQLite, `initStore` probes every ent time column
  for the four-digit numeric abbreviation (one `SELECT EXISTS` per column) on every boot. If a
  table holds such a value, the repair takes a snapshot of the database next to its file
  (`VACUUM INTO <db>.pre-utc-timestamp-normalize-<UTC time>.bak`), logs its path, and runs the
  same normalizer limited to those tables. It refuses to write if the snapshot fails. The
  snapshot is taken once: a later attempt reuses the existing file and logs that, so failed boots
  do not use more disk. A leftover temporary snapshot file means a snapshot is in progress or was
  interrupted; the repair refuses to write and reports that separately from a disk-space failure,
  and it removes only a file it created. When the rewrite completes but values that do not parse
  keep a table unreadable, it logs a manual-correction message (by table, column and rowid)
  instead of retrying. It runs
  **before** `migrateStore` because `Store.Migrate` reads agents, hub settings and user access
  tokens through ent and fails, fatally, on such rows: the test shows an unrepaired copy failing
  `Migrate` with a scan error on `user_access_tokens.created`. So the repair uses raw SQL only and
  skips tables and columns that an older schema lacks. The marker (`utc_timestamp_repair` in the
  `_migrations` hub setting) is written after `migrateStore`, only when a repair completed and a
  re-probe finds nothing, and it only records completion. The repair never fails boot, recovers
  from panics, caps per-value log lines at `maxBootLogErrors`, and has a 30-minute budget.
  Postgres is skipped.
- **Startup check.** On SQLite, `StartBackgroundServices` starts a background check with a
  5-minute timeout, so its full scan per column does not delay start. It logs one error naming
  `utc-timestamp-normalize` and the tables (never values) when a table has values the operation
  will rewrite, with a separate `tables_unreadable` attribute, and a warning pointing at the run
  log when the only leftovers are unparseable. If such a table is also unreadable, that line is an
  error and lists it in `tables_unreadable`. The check function is a package variable, so a test
  blocks it and shows that start returns while it runs.
- **Executor.** It is registered under `utc-timestamp-normalize` and seeded as a migration-category
  operation. `{"params":{"dryRun":true}}` reports without writing and leaves the status pending.
  The key is exempt from the completed-migration guard, so it can run again after rows written
  later (for example by an older binary) make the startup check fire again. The dialect comes
  from the store (`CompositeStore.Dialect()`), and no probe statement is run against the database.
- **CI.** The JSON rewrite tests (`TestUTCTimestampNormalizeJSON_`) are added to the `-run` filter
  of the existing Postgres store target. The workflow file is unchanged.

## Notes

- The scheduled-events handler and the schedule store `fire_at` binds are out of scope
  (ptone/scion#2476 owns them). The fixture test covers a stored `+0200 +0200` `fire_at`:
  `ListScheduledEvents` fails before the run and succeeds after it.
- Operator action, for the release note: unreadable tables are repaired automatically at the
  first start of this release, after a snapshot next to the database file, taken once and reused
  on later attempts, that needs about the database size in free space. The snapshot is a full copy
  of the database, including secrets: store it like the database and delete it once the repair is
  verified. Other non-canonical values need the operation; back up
  the database, then run it on this release or later.
- Design decision (review round 1): one-time migration utilities do not go in the CLI, so there is
  no offline subcommand. The boot repair runs ahead of `migrateStore`, not inside
  `runBootDataMigrations` as first proposed, because that hook runs after `Migrate` has already
  failed on the affected hubs. Splitting `Store.Migrate` was considered and rejected.
- Review round 2: the snapshot is taken once and reused on retries; values that do not parse get
  a manual-correction message instead of "will retry"; the startup check escalates unreadable
  tables whose leftovers do not parse; the snapshot code deletes only files it created; and the
  background startup check has a test seam.
