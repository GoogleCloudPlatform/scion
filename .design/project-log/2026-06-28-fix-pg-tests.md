# fix-pg-tests

Implemented Postgres startup migration locking and repaired the failing `pkg/hub` and `internal/fixturegen` test suites.

Changes:
- Added `store.LockSchemaMigration` and wrapped Postgres `s.Migrate(ctx)` in a blocking `pg_advisory_lock`/`pg_advisory_unlock` in `cmd/server_foreground.go`.
- Updated fixture generation for current Ent schema names, deterministic raw-SQL defaults, and newly covered domain tables.
- Fixed hub tests for deterministic dev user identity, UUID-backed test records, base64 secret values, current harness capabilities, and project-scoped harness config filtering.

Verification:
- `GOWORK=off go test ./pkg/hub/... ./internal/fixturegen/... ./pkg/store/...`
- `GOWORK=off go test ./cmd`
