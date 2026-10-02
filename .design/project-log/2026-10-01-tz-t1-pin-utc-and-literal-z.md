# tz-refactor task 1: Pin server processes to UTC, embed tzdata, fix literal-`Z` sites

**Date:** 2026-10-01 (round 1 review fixes: 2026-10-02)
**Branch:** `scion/tz-t1`
**Fork issue:** ptone/scion#2494 (part of ptone/scion#2457)

## Problem

The hub/broker process had no timezone policy: it ran in whatever `TZ` the
host happened to have. On a non-UTC host this leaked local offsets into logs,
cron parsing, and — worse — a handful of call sites formatted a **local wall
clock with a literal trailing `Z`**, which doesn't just mislabel the offset,
it reports the **wrong instant**. See the tz-refactor design (ptone/scion#2457)
§0/§2.1 for the full design context (Option A as decided).

## Changes

1. **`pkg/util.PinProcessUTC()`** (new, `pkg/util/tz.go`): sets
   `time.Local = time.UTC`. Documented as a process-entry-point-only call —
   never in a CLI command or a `PersistentPreRun`.
2. **Wired into every server/broker and offline store-writing entry point**,
   as the first statement of each command's run function, via a single
   package-level seam in `cmd`, `pinProcessUTC = util.PinProcessUTC`
   (`cmd/server_foreground.go`). Not via `PersistentPreRun`, so the CLI's
   local-zone behavior is untouched:
   - `cmd/server_foreground.go`: `runServerStart` (covers `scion server
     start`/`--foreground` and `scion runtime-broker start --foreground`,
     which both route through this function).
   - `cmd/server_migrate.go`: `runServerMigrate`.
   - `cmd/server_migrate_storage.go`: `runMigrateStorage`.
   - `cmd/server_dm_migration.go`: `runServerDMMigration`.
   - `cmd/server_backfill.go`: `runServerBackfill`.
   - `cmd/server_recover_authz.go`: `runRecoverAuthz`.
   - `cmd/hub_secret_migrate.go`: `runSecretMigrate`.
   - `cmd/hub_secret_migrate_names.go`: `runSecretMigrateNames`.
     (The last two were added in review round 1, R1-4 — see below.)
3. **`import _ "time/tzdata"`** added to `cmd/scion/main.go` and
   `cmd/sciontool/main.go` so `time.LoadLocation` works in a scratch image
   with no `/usr/share/zoneinfo`. Verified with `go list -deps` and
   `go tool nm` (see Testing).
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
     `expiresAt`: `token.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")`.
   - `pkg/hub/admin_invites.go:177` — invite audit-log `expires_at`:
     `invite.ExpiresAt.UTC().Format(time.RFC3339)`.
   - These last two are inline at the call site (review round 1 removed an
     intermediate `formatUTCTimestamp` helper — see below).

## Review round 1

Review (`gs://scion-xproject-exchange/tz-refactor/out/t1/review-1.md`) found
1 High, 2 Medium, 4 Low and 1 Nit. All are fixed; full disposition is in
`review-1-response.md` (same GCS prefix). Summary of the code changes:

- **R1-3 (High), R1-2 (Medium): one seam, an AST placement test, no more
  `cmd` data race.** The previous round used two ad hoc seam vars
  (`pinProcessUTCFn`, `initServerLoggingFn`) and a single ordering test
  (`TestRunServerStart_PinsUTCBeforeAnyOtherWork`) that only proved the pin
  ran before logging init in `runServerStart`. Calling the real
  `util.PinProcessUTC()` from the three run functions with their own direct
  `cmd` tests (`server_dm_migration_test.go`, `server_backfill_test.go`,
  `server_recover_authz_test.go`) raced goroutines leaked by other `cmd`
  tests in the same binary, and silently turned the rest of that test binary
  UTC regardless of `TZ`, masking 30 Kathmandu-baseline failures.
  Fixed by:
  - replacing both old seams with one, `pinProcessUTC = util.PinProcessUTC`
    (`cmd/server_foreground.go`), called by all eight run functions listed
    above;
  - `cmd/main_test.go`: a new `TestMain` for the `cmd` package that sets
    `pinProcessUTC = func() {}` for the whole test binary, so no `cmd` test
    ever runs the real pin;
  - `cmd/pin_process_utc_test.go`: a static (AST) test,
    `TestPinProcessUTC_CallSitesAreExactlyTheAllowList`, replacing the
    deleted ordering test. It parses every non-test `cmd/` source file and
    asserts: `util.PinProcessUTC` is referenced exactly once (the seam's own
    initializer); every `pinProcessUTC()` call is the first statement
    (`Body.List[0]`) of one of the eight allow-listed functions; and no call
    sits inside any closure (`*ast.FuncLit`), which would catch a
    `PersistentPreRun(E)` or `RunE` literal.
  - Deleted `cmd/server_foreground_pinutc_test.go`
    (`TestRunServerStart_PinsUTCBeforeAnyOtherWork`) and the
    `initServerLoggingFn` seam; `runServerStart` now calls
    `initServerLogging` directly.
  - Verified: `TZ=Asia/Tokyo go test -race -count=1 ./cmd/` — no `DATA RACE`
    (was 1, reproducible, before the fix). `TZ=Asia/Kathmandu go test
    -count=1 ./cmd/` — 166 distinct failing tests, and that set is
    byte-identical to a baseline run on the current upstream main tip
    (`GoogleCloudPlatform/scion@b44fcf25`, built in a detached worktree),
    confirming no new failures and no masking. (166 vs. the review's "about
    163 on `e572e72f`" reflects upstream main moving — tz-refactor tasks 10
    and 14 merged in between — not a regression; see Testing.)
- **R1-4 (Low, ruling: YES): pin the two secret-migrate commands.**
  `runSecretMigrate` (`cmd/hub_secret_migrate.go`) and
  `runSecretMigrateNames` (`cmd/hub_secret_migrate_names.go`) open the hub
  database directly and write rows (ent `Default(time.Now)`/`UpdateDefault`),
  so tz-em ruled they are offline hub-store writers like the other five.
  `pinProcessUTC()` is now their first statement, and both are in the
  `pin_process_utc_test.go` allow-list.
- **R1-1 (Medium): test the real call sites, not a helper.** Deleted
  `pkg/hub/timeformat.go` (`formatUTCTimestamp`) and its test: reverting
  either of the two call sites it wrapped left the old test green, because
  it only tested the helper, not the sites. The two sites
  (`handlers_github_app_webhook.go:808`, `admin_invites.go:177`) are now
  inline `.UTC().Format(...)` calls, as the task list specified. Added
  `pkg/hub/admin_invites_audit_utc_test.go`
  (`TestAdminInvitesCreate_AuditLogExpiresAtIsUTC`), which drives
  `handleAdminInvites` end to end with a `recordingAuditLogger` and asserts
  the captured `expires_at` is `Z`-suffixed and instant-equal to the created
  invite's `ExpiresAt`. For the GitHub App token site: no fake GitHub App
  client exists anywhere in `pkg/hub` tests today, and per the review's
  instruction we did not build one for this. That site's regression
  protection for now is the `make time-literals` gate (tz-refactor task 5,
  U2d, design §2.1.7, which already lists this site).
- **R1-5 (Low): exercise more than one offset.** Both
  `pkg/hub/events_utc_test.go` tests now loop
  (`locationsUnderTest`/`t.Run`) over three locations: a named +9h zone
  (JST-style), a nameless +5:45 zone (Kathmandu-style, sub-hour, no letter
  abbreviation), and `time.Local` itself — the last one is what actually
  varies under the required `TZ=Asia/Tokyo`/`TZ=Asia/Kathmandu` runs, since
  the fixed-zone-only version before this fix was identical under both.
- **R1-6 (Low): comment accuracy.** Reworded the `events_utc_test.go` doc
  comment: the `:491,502,534` sites produced a correct instant with a local
  offset (a §2.2 wire-contract violation), not a wrong instant; only `:725`
  produced a wrong instant. Dropped the `dev-common.md` file-name citation
  (not in the repo); this log and the test comments now cite
  ptone/scion#2494 / ptone/scion#2457 instead of the scratch file names
  (`impl-issues.md`, `design.md`) used in the first pass.
- **R1-7 (Low): tzdata evidence that actually discriminates.** The original
  "manual check" (`ZONEINFO` pointed at a nonexistent path) also passes
  without the `time/tzdata` import, because `LoadLocation` falls back to the
  system zoneinfo directories first on a host that has them. Replaced with
  `go list -deps ./cmd/scion ./cmd/sciontool | grep -x time/tzdata` (present
  for both) and `go tool nm` on the built binaries (3 `time/tzdata` symbols
  in each) — see Testing.
- **R1-8 (Nit): bare `#1` in a commit subject.** The branch was rebased onto
  upstream main for this round anyway (see below); the offending commit's
  subject was reworded in the same pass to say "tz-refactor task 1", not
  "tz task #1".

## Rebase onto upstream main

Rebased `scion/tz-t1` onto `GoogleCloudPlatform/scion` main
(`b44fcf25`, pulling in tz-refactor task 10, `GoogleCloudPlatform/scion#2241`,
and task 14, `GoogleCloudPlatform/scion#2218`). No conflicts: neither task
touches the files this branch changes. Only this branch's two commits remain
ahead of `upstream/main` after the rebase.

## Testing

- `pkg/util/tz_test.go`: `TestPinProcessUTC` — unchanged, still covers
  `PinProcessUTC` itself.
- `cmd/pin_process_utc_test.go`:
  `TestPinProcessUTC_CallSitesAreExactlyTheAllowList` — the AST placement
  test described above (R1-2/R1-3).
- `cmd/main_test.go`: `TestMain` disables the real pin for the `cmd` test
  binary (R1-3).
- `pkg/hub/events_utc_test.go`:
  `TestChannelEventPublisher_AgentEventTimestampsAreInstantCorrect` and
  `TestChannelEventPublisher_UserMessageCreatedAtIsInstantCorrect` — now
  table-driven over three locations (R1-5), asserting instant equality
  (not just a `Z` suffix).
- `pkg/hub/admin_invites_audit_utc_test.go`:
  `TestAdminInvitesCreate_AuditLogExpiresAtIsUTC` — site-level test added
  for R1-1.
- `go build -buildvcs=false -p 2 ./...`: clean.
- `go vet -buildvcs=false ./cmd/... ./pkg/hub/... ./pkg/util/...`: clean.
- `gofmt -l cmd pkg/hub pkg/util`: clean.
- `GOGC=40 golangci-lint run --new-from-rev=upstream/main --concurrency=1`
  on `./cmd/...`, `./pkg/hub/...`, `./pkg/util/...`: 0 issues (run
  separately per the broker-01 one-heavy-build-at-a-time throttle).
- `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu go test -buildvcs=false -p 2
  -count=1 ./pkg/util/...`: ok / ok.
- `TZ=Asia/Tokyo go test -buildvcs=false -race -count=1 ./cmd/`: FAILs
  `TestContract_BudgetExhaustion_MonotonicProgress` and
  `TestReincarnateHandoffTemplate_WorksAnywhere` (pre-existing, fail
  identically with no race on upstream main) — **no `DATA RACE`** (was 1,
  reproducible, before the R1-3 fix).
- `TZ=Asia/Kathmandu go test -buildvcs=false -count=1 ./cmd/`: 166 distinct
  failing tests. Byte-identical failing-test-name set to the same command
  run against `GoogleCloudPlatform/scion@b44fcf25` (upstream main tip, in a
  detached worktree) — confirms no new failures and no masking.
- `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu go test -buildvcs=false -p 2
  -count=1 -run 'TestChannelEventPublisher_|TestAdminInvitesCreate_' -v
  ./pkg/hub/`: the events tests pass under both TZs, all three locations.
  The two `TestAdminInvitesCreate_*` tests (the pre-existing one and the new
  R1-1 one) both pass under Tokyo. Under Kathmandu, **both fail with exactly
  the known pre-existing baseline error** (`empty agent role backfill: sql:
  Scan error on column index 6, name "create_time": unsupported Scan,
  storing driver.Value type string into type *time.Time`) — reproduced
  identically against `GoogleCloudPlatform/scion@b44fcf25` for the
  pre-existing test. This is the `createTestStore`-backed-test baseline
  documented in `dev-common.md` (tz-refactor task 2 / U2a territory, not
  this task's bug), not a regression from this branch.
- tzdata: `go list -deps ./cmd/scion ./cmd/sciontool | grep -x time/tzdata`
  — present for both. `go tool nm` on binaries built from this head shows 3
  `time/tzdata` symbols in each (R1-7).
- `make ci`/`make ci-full` were not run locally (broker-01 throttle, per
  the brief's override); fork CI runs the full suite.
- **Fork CI (`gh pr checks 2522 -R ptone/scion`):** `Mergeability Gate`,
  `golangci-lint`, `pkg/hub SQLite Tests`, `T1 Launch Store PostgreSQL
  Tests`, `shellcheck`, `Lint 405 Allow header (reporting only)` and
  `single-node-vm deploy.sh test harness` all pass. `Build & Test` and
  `Full Test Suite (reporting only)` fail, both on the same single
  failure: `TestReResolveModelAlias/nil_cfg_falls_back_to_built-in_table_when_harness_is_known`
  in `pkg/agent` (`reResolveModelAlias() model = "claude-sonnet-5-5", want
  "claude-sonnet-5"`), reproduced identically on a re-run of the same CI
  job. This is **not caused by this branch**: `pkg/agent`,
  `harnesses/claude/config.yaml` and everything `reResolveModelAlias`
  touches are untouched by this change; the test passes locally every way
  tried (isolated, full `pkg/agent` package, with and without `-tags
  no_sqlite`) against both this branch and a pristine
  `GoogleCloudPlatform/scion@b44fcf25` checkout; and the identical failure
  (same test, same wrong value) is independently reproducing right now on
  at least three other, unrelated, concurrently-running fork PRs/branches
  (`fix/skill-resolve-fail-fast`, `scion/agent-keys-4-1-tests`,
  `kr-t1-p1b1`) that don't touch this code either. This looks like a
  fork-wide CI environment issue (CI-only, not reproducible locally),
  flagged to tz-em; not fixed here (out of scope, shared infrastructure).

## Deliberately out of scope (per the issue)

- `pkg/hub/events.go:637,669,811,833` and other sites that already call
  `.UTC()` before formatting with a fixed `.000Z`/`15:04:05.000` literal —
  these are correct instants today (not this bug class) and are not in
  this task's scope list.
- `make time-literals` (the regression gate) is tz-refactor task 5 (U2d),
  landing after U1 and U2 merge. It is also now the stated regression
  protection for the GitHub App token expiry site (R1-1).
- The SQLite store boundary (DSN `_timezone=UTC`, ent hook, predicates) is
  tz-refactor task 2 (U2a).

## Follow-ups noticed, not fixed here

- None beyond what's already tracked in the tz-refactor task list
  (tasks 2-23).
