# DEF-12: CLI Command — scion server backfill

**Date:** 2026-08-27
**Author:** dev-backfill-cmd
**Branch:** scion/ca-msg-em6-def12

## Summary

Implemented the `scion server backfill` CLI subcommand that retroactively
assigns historical messages to conversations based on thread, sender, and
recipient metadata.

## Deliverables

### cmd/server_backfill.go

- **Command:** `scion server backfill` added as a subcommand of `serverCmd`
- **Flags:**
  - `--execute` — apply changes (default: dry-run mode)
  - `--project <id>` — scope to a single project (default: all projects)
  - `--batch-size <n>` — messages per batch (default: 100)
  - `--checkpoint <msg-id>` — resume from a previous run
  - `--db <dsn>` — database DSN override (auto-detects sqlite vs postgres)
- **Database resolution:** `--db` flag > config file (via `LoadGlobalConfig`)
- **Multi-project support:** when `--project` is omitted, iterates all projects
  via `ListProjects` and aggregates results
- **Report:** human-readable summary printed to stdout showing mode, project
  scope, messages processed/attributed/inferred/skipped, conversations created,
  hazard counts, errors, and last checkpoint

### cmd/server_backfill_test.go

| Test | AC | Verifies |
|------|-----|----------|
| `TestBackfillDryRunMutatesNothing` | AC-12-2 | Dry-run processes messages but leaves conversation_id empty |
| `TestBackfillExecuteAndIdempotent` | AC-12-3 | Execute stamps messages; second run skips all (idempotent) |
| `TestBackfillResumeViaCheckpoint` | AC-12-4 | Checkpoint-based resume processes only new messages |
| `TestBackfillMalformedThreadID` | AC-12-5 | Malformed `dm:invalid:bad:format` ThreadID produces errors |
| `TestBackfillMergeResult` | — | Result aggregation helper correctness |

All tests use in-memory SQLite via the existing `newTestStore(t)` pattern.

## Design Decisions

1. **Dry-run default:** the command defaults to dry-run mode (no `--execute`)
   to prevent accidental data modification, matching the safety-first approach
   of the existing backfill service.

2. **Store opening:** follows the same pattern as `server_foreground.go`'s
   `initStore()` — opens via `entc.OpenSQLite`/`OpenPostgres`, runs
   `AutoMigrate`, wraps in `entadapter.NewCompositeStore`. The `--db` flag
   auto-detects driver from DSN prefix.

3. **Testable core:** extracted `runBackfillWithStore(ctx, store, config)` as
   the testable entry point, bypassing cobra command/flag parsing.

4. **Result aggregation:** `mergeBackfillResult` accumulates counts across
   multiple projects for the multi-project case.
