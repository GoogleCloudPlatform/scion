# Update snapshots: stale-partial cutoff on the injected clock (Gemini review, #4371)

**Date:** 2026-10-30
**Branch:** scion/hu-p3-gem1 (from scion/hu-p3-mf1 @ 1aadca805)

Two Gemini review comments on the Phase 3 landing PR
(GoogleCloudPlatform/scion#4371), both on
`pkg/hub/maintenance/db_snapshot.go`.

## (2) Fixed: pruneStalePartials used the wall clock
`SQLiteSnapshotter.Snapshot` passed `time.Now()` to `pruneStalePartials`
while it named the snapshot from the injected clock (`s.now`). It now reads
`s.now()` once and uses that one value for both the stale-partial cutoff and
the snapshot name, so snapshot names are unchanged. The `start := time.Now()`
duration measurement stays on the wall clock by design.

Tests set file times relative to the test clock instead of the wall clock:
- `TestSQLiteSnapshotter_StalePartialsFreedBeforeDiskCheck`: the stale
  partial is two hours older than the injected clock. A second partial,
  ten minutes old by the injected clock (a day old by the wall clock), must
  survive. That fails if `Snapshot` reads the wall clock again.
- `TestSQLiteSnapshotter_ExistingPartialFails`: the in-use partial is dated
  at the injected clock, so the test no longer depends on the wall clock
  being later than the fixed test clock.

## (1) Declined: "VACUUM INTO fails if the pre-created .partial exists"
`writeAtomically` creates the `.partial` with O_CREATE|O_EXCL and closes it
empty before `VACUUM INTO` fills it. The SQLite documentation
(https://www.sqlite.org/lang_vacuum.html) says: "The file named by the INTO
clause must not previously exist, or else it must be an empty file, or the
VACUUM INTO command will fail with an error." The driver is
modernc.org/sqlite (a translation of the SQLite C sources, so the same
VACUUM INTO), and `pkg/store/entadapter` `SnapshotSQLite` uses the same
empty-file pattern. `TestSQLiteSnapshotter_Snapshot` runs this path on a
real database and reads the rows back from the snapshot.
