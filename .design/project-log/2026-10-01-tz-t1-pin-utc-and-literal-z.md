# tz task #1: Pin server processes to UTC, embed tzdata, fix literal-`Z` sites

**Date:** 2026-10-01
**Branch:** `scion/tz-t1`
**Fork issue:** ptone/scion#2494 (part of ptone/scion#2457)

## Problem

The hub/broker process had no timezone policy: it ran in whatever `TZ` the
host happened to have. On a non-UTC host this leaked local offsets into logs,
cron parsing, and — worse — a handful of call sites formatted a **local wall
clock with a literal trailing `Z`**, which doesn't just mislabel the offset,
it reports the **wrong instant**. See `design.md` §0/§2.1 and
`impl-issues.md` task #1 for the full design context (tz-refactor, Option A
as decided).

## Changes

1. **`pkg/util.PinProcessUTC()`** (new, `pkg/util/tz.go`): sets
   `time.Local = time.UTC`. Documented as a process-entry-point-only call —
   never in a CLI command or a `PersistentPreRun`.
2. **Wired into every server/broker and offline store-writing entry point**,
   as the first statement of each command's run function (not via
   `PersistentPreRun`, so the CLI's local-zone behavior is untouched):
   - `cmd/server_foreground.go`: `runServerStart` (covers `scion server
     start`/`--foreground` and `scion runtime-broker start --foreground`,
     which both route through this function).
   - `cmd/server_migrate.go`: `runServerMigrate`.
   - `cmd/server_migrate_storage.go`: `runMigrateStorage`.
   - `cmd/server_dm_migration.go`: `runServerDMMigration`.
   - `cmd/server_backfill.go`: `runServerBackfill`.
   - `cmd/server_recover_authz.go`: `runRecoverAuthz`.
3. **`import _ "time/tzdata"`** added to `cmd/scion/main.go` and
   `cmd/sciontool/main.go` so `time.LoadLocation` works in a scratch image
   with no `/usr/share/zoneinfo` (verified manually, see Testing below).
4. **Literal-`Z`/missing-`.UTC()` fixes** (the "wrong instant" bug class):
   - `pkg/hub/events.go:491,502,534` — agent event fields
     (`lastActivityEvent`, `startedAt`, `created`) now call `.UTC()` before
     `.Format("2006-01-02T15:04:05Z07:00")`.
   - `pkg/hub/events.go:725` — the chat SSE `createdAt` now uses
     `.UTC().Format(time.RFC3339Nano)` instead of
     `.Format("2006-01-02T15:04:05.000Z")` with no `.UTC()`. This also drops
     the fixed `.000Z` suffix in favor of a variable number of fraction
     digits (still valid RFC 3339, still parses the same).
   - `pkg/hub/handlers_github_app_webhook.go:808` — GitHub token
     `expiresAt`.
   - `pkg/hub/admin_invites.go:177` — invite audit-log `expires_at`.
   - Both of the last two now go through a new tiny helper,
     `formatUTCTimestamp(t, layout)` (`pkg/hub/timeformat.go`), added so the
     "format a wire timestamp" call sites can't drop `.UTC()` again without
     it being visible in one place, and so the fix is directly unit
     testable without standing up the full GitHub App / invite handler
     machinery.

## Testing

- `pkg/util/tz_test.go`: `TestPinProcessUTC` — flips `time.Local` to
  `Asia/Tokyo`, calls `PinProcessUTC()`, asserts `time.Local == time.UTC` and
  that a subsequent `time.Now()` reports the UTC location.
- `cmd/server_foreground_pinutc_test.go`:
  `TestRunServerStart_PinsUTCBeforeAnyOtherWork` — a seam test (per the
  issue's test plan: "use a seam, not a real server"). Added two seam vars,
  `pinProcessUTCFn` and `initServerLoggingFn`, and asserts the pin fires
  before logging init, short-circuiting right after so the test never opens
  a store, binds a port, or blocks.
- `pkg/hub/events_utc_test.go`:
  `TestChannelEventPublisher_AgentEventTimestampsAreInstantCorrect` and
  `TestChannelEventPublisher_UserMessageCreatedAtIsInstantCorrect` — build
  agent/message values with a `time.Time` in a fixed +9h zone (deterministic
  stand-in for `TZ=Asia/Tokyo`/`TZ=Asia/Kathmandu`, no tzdata lookup needed),
  publish them, parse the formatted event field back, and assert **instant
  equality** with the original value (not just a `Z` suffix) — this is
  exactly the defect class the literal-`Z` bug produced.
- `pkg/hub/timeformat_test.go`: `TestFormatUTCTimestamp` — table test with
  one row for the GitHub token expiry layout and one for the invite
  audit-log layout, same fixed-zone input, same instant-equality assertion.
- Manual check (not committed, ephemeral): built a throwaway program
  importing `_ "time/tzdata"`, pointed `ZONEINFO` at a nonexistent path to
  simulate a scratch image with no system tzdata, and confirmed
  `time.LoadLocation("America/New_York")` still succeeds.
- Ran the above plus `go build -buildvcs=false -p 2 ./...` and
  `go vet ./pkg/hub/...`, `go test -p 2 ./cmd/... ./pkg/util/... ./pkg/hub/...`
  under both `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu` (dev-common.md's
  both-TZ rule, since these are the packages where a timestamp crosses the
  wire boundary in this change).
- `make fmt`; `GOFLAGS="-buildvcs=false -p=2" make ci`.

See the PR for the exact command output and `gh pr checks`.

## Deliberately out of scope (per the issue)

- `pkg/hub/events.go:637,669,811,833` and other sites that already call
  `.UTC()` before formatting with a fixed `.000Z`/`15:04:05.000` literal —
  these are correct instants today (not this bug class) and are not in
  task #1's scope list.
- `make time-literals` (the regression gate) is task #5 (U2d), landing after
  U1 and U2 merge.
- The SQLite store boundary (DSN `_timezone=UTC`, ent hook, predicates) is
  task #2 (U2a).

## Follow-ups noticed, not fixed here

- None beyond what's already tracked in `impl-issues.md` tasks #2-#23.
