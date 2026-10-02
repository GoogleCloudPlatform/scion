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
- **Startup check.** On SQLite, `StartBackgroundServices` runs one `EXISTS` probe per ent time
  column before the scheduler starts. If any table holds a non-canonical value, it logs one error
  naming `utc-timestamp-normalize` and the tables (never values).
- **Executor.** It is registered under `utc-timestamp-normalize` and seeded as a migration-category
  operation. `{"params":{"dryRun":true}}` reports without writing and leaves the status pending.
- **CI.** The JSON rewrite tests (`TestUTCTimestampNormalizeJSON_`) are added to the `-run` filter
  of the existing Postgres store target. The workflow file is unchanged.

## Notes

- The scheduled-events handler and the schedule store `fire_at` binds are out of scope
  (ptone/scion#2476 owns them). The fixture test covers a stored `+0200 +0200` `fire_at`:
  `ListScheduledEvents` fails before the run and succeeds after it.
- Operator action, for the release note: back up the database, then run the operation on this
  release or later.
