# PR #305 Review Feedback — First Fix Round

**Date:** 2026-06-05  
**PR:** #305 — feat(hub): multi-node broker dispatch  
**Branch:** pr/broker-dispatch  
**Commit:** c5f8b3c

## Summary

Addressed all 6 review comments from gemini-code-assist on PR #305.

### HIGH Priority Fixes

1. **server_migrate.go — nil-checked deferred close**: Changed `defer src.Close()` to a nil-checked closure so the source DB can be manually closed and set to nil before `dropSQLiteFile`, preventing Windows sharing violations.

2. **server_migrate.go — close before drop**: Added explicit `src.Close()` + `src = nil` before the `dropSQLiteFile` call in the `migrateDropSource` path.

3. **server_foreground.go — stale closure capture**: Moved `mgr := hubSrv.GetControlChannelManager()` inside the `ownsLocally` closure. Previously it was captured once at closure creation time, so if the manager was nil at that point but initialized later, `ownsLocally` would permanently return false.

### MEDIUM Priority Fixes

4. **server_migrate.go — file:// prefix handling**: Added a `file://` case before the `file:` case in `parseSQLiteSourceDSN` so that `file:///tmp/hub.db` correctly resolves to `/tmp/hub.db` instead of `//tmp/hub.db`.

5. **server_migrate_test.go — triple-slash test**: Added a test case verifying `file:///tmp/hub.db` is parsed correctly.

6. **server_test.go — subtest name sanitization**: Used `strings.ReplaceAll(t.Name(), "/", "_")` in `newTestStore` to prevent SQLite from interpreting subtest slashes as directory paths.

## Verification

- `gofmt` clean on all changed files
- `go vet ./cmd/` passes
- All relevant tests pass including the new `file_url_with_triple_slashes` test case
