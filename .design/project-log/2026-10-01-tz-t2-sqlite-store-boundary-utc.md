# tz-refactor task 2 (U2a): SQLite store boundary in UTC

**Date:** 2026-10-01
**Branch:** `scion/tz-t2`
**Fork issue:** ptone/scion#2495 (part of ptone/scion#2457)

## Problem

Nothing forced SQLite `time.Time` binds or reads through a UTC-canonical
path. Under a non-UTC `time.Local`, modernc.org/sqlite renders a bound
`time.Time` with `Time.String()` in its own zone (e.g. `'... +0900 JST
m=+0.09...'`), which: (a) does not round-trip through ent's own `TEXT`
columns cleanly when mixed with canonical UTC rows (wrong sort order), and
(b) a bare `time.Now()` bind carries a monotonic-clock suffix (`m=...`)
that is never canonical. See design `tz-refactor/design.md` §2.1.2.

## Scope of this change (narrowed per tz-em ruling, 2026-10-01)

PR ptone/scion#2470 (agent-store `.UTC()` at write/bind sites, including
`MarkStale*`/`MarkStalled*`) and PR ptone/scion#2476 (ScheduleStore binds,
`ListDueSchedules`, `PurgeOldScheduledEvents`) are open and not stalled, so
this change does **not** touch `pkg/store/entadapter/agent_store.go` or
`pkg/store/entadapter/schedule_store.go`. Separately, fork issue
ptone/scion#2553 owns the message-store items (purge thresholds, keyset
cursor), so `pkg/store/entadapter/message_store.go` is untouched here too.

What's left after both carve-outs, and what this change implements:

- **SQLite DSN option.** `pkg/ent/entc/client.go`: `OpenSQLite` and
  `OpenSQLiteReadOnly` now rewrite the caller's DSN to force
  `_timezone=UTC` (`withUTCTimezone`), preserving any other options already
  present and replacing an operator-supplied `_timezone` (modernc only
  honours the first value for a repeated key). This makes every SQLite bind
  and scan canonical UTC regardless of `time.Local`, including legacy rows
  written with a non-UTC zone suffix.
- **Ent mutation hook.** `entc.UTCTimeHook` (exported; see "Review round
  1 fixes" below for why) calls `.UTC()` on every
  `time.Time` field value present in a mutation (explicit sets and
  schema `Default`/`UpdateDefault` values, since ent populates defaults on
  the mutation before hooks run). Registered via `client.Use(...)` in
  `OpenSQLite`, `OpenSQLiteReadOnly` and `openPostgres`. On Postgres
  (`field.Time` → `timestamptz`) this changes nothing persisted; its only
  effect is that a non-UTC input is not echoed back non-UTC in the
  create/update response.
- **Predicates.** No predicate sites were in scope after the two
  carve-outs above — the only predicate sites named in `impl-issues.md`
  task 2 are the agent-store thresholds (owned by ptone/scion#2470) and the
  message-store purge/cursor sites (owned by ptone/scion#2553). The DSN option alone
  already makes a threshold bound in any location compare correctly on
  SQLite (verified by test, see below); `.UTC()` at a bind site is
  belt-and-braces for Postgres parity with pre-existing explicit binds, not
  something this task needed to add anywhere.
- `pkg/store/enttest` (`pkg/store/enttest/enttest.go`,
  `enttest_postgres.go`) already routes every test client through
  `entc.OpenSQLite`/`entc.OpenPostgres`, so it is covered for free — no
  edits needed there. The generated `pkg/ent/enttest` package is unused
  anywhere in this repo and was left untouched.

## Tests added (`pkg/ent/entc/utc_timezone_test.go`)

- `TestWithUTCTimezone` — DSN rewrite: bare path, `:memory:`, `file:` with
  no query, `file:` with an existing query, operator-supplied `_timezone`
  replaced.
- `TestOpenSQLite_BindsCanonicalUTC` — a `Default(time.Now)` field and an
  explicit non-UTC-located `SetLastLogin` both land as `'... +0000 UTC'`
  text with no `m=...` suffix.
- `TestOpenSQLite_LegacyRowReadBack` — a raw-SQL-inserted legacy
  `Time.String()` row (JST, with and without a monotonic suffix) reads back
  through ent as the correct UTC instant (AC2).
- `TestOpenSQLite_ThresholdBindMatchesCanonicalCount` — the 5-vs-3 fixture
  from design §2.1.2: a `last_login < threshold` predicate bound with a
  time.Time located in Asia/Kathmandu (four-digit numeric offset, `+0545`)
  matches the correct 3 of 5 rows.
- `TestHubSetting_HookCoversCreateAndUpdate` — hook fires on both the
  create and update path (`UpdateDefault(time.Now)`).

## Test evidence

- `go build -buildvcs=false -p 2 ./...` — clean.
- `go test -p 2 -count=1 ./pkg/ent/...` — pass, default TZ, `TZ=Asia/Tokyo`,
  and `TZ=Asia/Kathmandu`.
- `go test -p 2 -count=1 ./pkg/store/entadapter/... ./pkg/store/enttest/...`
  — pass under default TZ, `TZ=Asia/Tokyo`, and `TZ=Asia/Kathmandu` (this
  is the KNOWN BASELINE package from dev-common.md; it now passes under
  Kathmandu without needing PR ptone/scion#2470 or PR ptone/scion#2476).
- `golangci-lint run --new-from-rev=upstream/main --concurrency=1
  ./pkg/ent/entc/...` — 0 issues.
- `gofmt -l` on changed files — clean.
- `go test -p 2 -count=1 -timeout 60m ./pkg/hub/...` (the ~275-test
  `createTestStore`-backed baseline from dev-common.md): **fully green
  under both `TZ=Asia/Kathmandu` (1040.6s) and `TZ=Asia/Tokyo` (1029.0s)**,
  zero failures — not a partial fix. `pkg/hub/auth`, `authzop`,
  `githubapp`, `imagecheck`, `permissions` all pass too under both zones;
  `brokersettings` has no test files. No remaining failure traces to an
  agent-store site owned by PR ptone/scion#2470: `_timezone=UTC`
  canonicalises every SQLite bind regardless of whether the predicate
  site calls `.UTC()` explicitly (design §2.1.2), so the agent-store/
  schedule-store thresholds are already correct on SQLite even before
  PR ptone/scion#2470 or PR ptone/scion#2476 land. Those two PRs still matter for **Postgres**, where
  there's no DSN-level equivalent — correctness there depends on their
  explicit bind-site `.UTC()` plus this PR's hook (which only normalises
  what's echoed back, since Postgres `timestamptz` is already
  instant-correct regardless of the bound value's `Location`). A
  default-TZ full `pkg/hub` run was skipped per tz-em (fork CI covers
  default TZ).

## Fixed: two test harnesses bypassed the store boundary entirely

While validating the Kathmandu acceptance criterion, found that
`pkg/hub/ge_exchange_signin_policy_test.go`'s `newSignInPolicyHarness`
and `pkg/hub/ge_exchange_test.go`'s `newPersistentTestExchangeService`
each built their own `*ent.Client` via a raw `sql.Open("sqlite", dsn)` +
`ent.NewClient(ent.Driver(entsql.OpenDB(...)))`, instead of going through
`entc.OpenSQLite`. Neither this PR's DSN option nor its mutation hook
could reach them, so under `TZ=Asia/Kathmandu` a bare `time.Now()`
default (`ExternalIdentity.CreatedAt`, `User.Created`, etc.) stored a
numeric-zone-abbreviation wall clock that ent then failed to `Scan` back
— 15 failing tests (`TestExternalBearer_*`, `TestGEExchange_*`,
`TestGoogleIdentityResolver_*`), with the same error shape as the known
agent-store/schedule-store baseline (`unsupported Scan, storing
driver.Value type string into type *time.Time`) but on different
columns/tables (`external_identities.created_at`, `users.created`,
`groups.created`) and via a live `Create()` call during the test rather
than the startup migration backfill — so a distinct bug, not the
baseline, and per dev-common.md's "any Kathmandu failure that is NOT
this baseline error is yours to fix," in scope here.

First fix (commit `fb3fe12`) routed both harnesses through
`entc.OpenSQLite`. Their hand-rolled `_journal_mode=WAL&_busy_timeout=5000`
DSN query params were never modernc-recognised keys (modernc only reads
`_dqs`, `_error_rc`, `_pragma`, `_time_format`, `_time_integer_format`,
`_timezone`, `_txlock`, `_inttotime`, `_texttotime`), so they were
already no-ops.

That broke fork CI's "Build & Test" job (`make test-fast`, i.e. `go test
-tags no_sqlite`; caught via `gh run view 36942107974 --job
110635763894 --log-failed`): `ge_exchange_signin_policy_test.go` carries
a `//go:build !no_sqlite` constraint, so it's unaffected, but
`ge_exchange_test.go` has no build tag and is compiled under
`no_sqlite` too. Several unrelated `webchat_*` test files in the same
package import `mattn/go-sqlite3` without a build-tag guard, registering
the cgo `"sqlite3"` driver even when modernc is excluded. So
`ge_exchange_test.go`'s own runtime check (`sqliteDriverName()`, which
just looks for *any* registered SQLite driver) found `"sqlite3"` and let
the test proceed instead of skipping, straight into `entc.OpenSQLite`'s
hardcoded `sql.Open("sqlite", ...)` — `"sqlite"` (modernc) isn't linked
under `no_sqlite`, so: `sql: unknown driver "sqlite" (forgotten
import?)`.

Corrected (commit `35c9516`): `ge_exchange_test.go`'s
`newPersistentTestExchangeService` reverts to the flexible
`sql.Open(driverName, dsn)` that adapts to whichever driver the build
actually links, and instead registers the hook directly —
`client.Use(entc.UTCTimeHook)` — which required exporting
`utcTimeHook` as `entc.UTCTimeHook`. The hook alone is sufficient for
this fix: it converts a mutation's `time.Time` to UTC *before* the SQL
driver ever formats it for binding, so the resulting text is canonical
regardless of the connection's own timezone handling (modernc's
`formatTime` only adjusts a bound value's location when `_timezone` is
set — `conn.go:207-211`,`219-221` — so an already-`.UTC()`-converted
value formats as canonical `"... +0000 UTC"` either way).
`ge_exchange_signin_policy_test.go` is untouched by this correction;
its build tag guarantees modernc is always linked when it compiles, so
`entc.OpenSQLite` was never wrong there.

Verified: `go build -buildvcs=false -p 2 ./...`; `go test -tags
no_sqlite -p 2 -count=1 ./pkg/hub/...` (reproduces and then passes the
CI failure, including `TestGEExchange_ConcurrentFirstLinkage_PersistentStore`
specifically, confirmed actually running via `-v`, not silently
skipped); all 15 originally-failing tests still pass individually under
`TZ=Asia/Kathmandu`; `gofmt -l` clean on both changed files;
`golangci-lint run --new-from-rev=upstream/main --concurrency=1
./pkg/ent/entc/... ./pkg/hub/...` — 0 issues. Confirmed no other
`pkg/hub` test file wraps a raw `sql.Open` in an `ent.Client` — grepped
every other `sql.Open(` site in `pkg/hub/*_test.go` (about 20 files);
all are the raw `webchat_*` stores, task 3 (U2b) territory, untouched.

## Decided: no change at the other predicate sites (tz-lead ruling, 2026-10-01)

Several other entadapter stores bind unconverted `time.Time` thresholds
into `LT`/`LTE`/`GT`/`GTE` predicates (`brokerdispatch_store.go`,
`mutation_audit_store.go`, `decision_audit_store.go`,
`notification_store.go`, `credential_store.go`, `composite.go`'s
`DeletedAtLT`, `chat_link_store.go`, `github_resolution_store.go`). I
flagged these to tz-em; tz-lead's ruling is that no `.UTC()` is needed at
these sites: range-predicate binds are correct without it on both
backends — SQLite canonicalises every bound `time.Time` through the DSN
`_timezone=UTC` option added by this PR (design §2.1.2; verified by
`TestOpenSQLite_ThresholdBindMatchesCanonicalCount`), and Postgres
`timestamptz` compares instants regardless of the bound value's
`Location`. The predicate `.UTC()` calls in `design.md` are
defense-in-depth for the two named stores only, not a general
requirement. This is recorded as a decision (see the PR's "Decided: no
change" section) so task 5's `make time-literals` gate treats these
sites as already correct.

## Review round 1 fixes (`gs://scion-xproject-exchange/tz-refactor/out/t2/review-1.md`)

Verdict: CHANGES REQUESTED (0 Critical/High, 1 Medium, 3 Low, 2 Nit). ACs
confirmed met; every new test confirmed to fail without its fix (reviewer
ran the mutations). Full response filed at
`gs://scion-xproject-exchange/tz-refactor/out/t2/review-1-response.md`;
summary here:

- **R1-1 (Medium):** `withUTCTimezone` discarded the caller's whole DSN
  query on a `url.ParseQuery` error (`values = url.Values{}`), which
  could silently drop `mode=memory`/`mode=ro` and masked the error
  modernc's own `applyQueryParams` would otherwise surface at open time.
  Fixed: on a parse error, return `dsn` unchanged. Added
  `TestWithUTCTimezone_MalformedQueryIsNotMasked` (two fixtures: invalid
  percent-encoding, a `;`-containing `_pragma` value).
- **R1-2 (Low):** `entc.UTCTimeHook`'s doc now lists what it does not
  cover — predicate arguments, `OnConflict().Update(...)`, raw SQL,
  JSON-embedded times — and which backend (SQLite vs. Postgres) each gap
  matters for.
- **R1-3 (Low) / R1-6 (Nit), taken together:** `ge_exchange_test.go`'s
  `newPersistentTestExchangeService` left predicate binds
  uncanonicalised on SQLite (the hook only reaches mutation field
  values, not predicate arguments), and the `35c9516`/`613e488` "revert"
  had also dropped the original `_journal_mode=WAL&_busy_timeout=5000`
  DSN query params, which was more than a revert and wrongly justified
  for the mattn `sqlite3` driver (which *does* read those keys, even
  though the resulting behaviour happened to be unchanged). Fixed
  together: restored the original query params, and added
  `sqliteTimezoneDSNOption(driverName)`, appending `_timezone=UTC` for
  modernc's `"sqlite"` or `_loc=UTC` for mattn's `"sqlite3"`.
- **R1-4 (Low):** every bare `#N` and `task #N` (same GitHub-autolink
  problem) was fixed throughout the PR body and this project log.
  Reworded the two flagged commits' messages and rebuilt the branch on
  top of them via cherry-pick + `commit --amend -m` (not `rebase -i`,
  per the sandbox's git rules), then pushed with `--force-with-lease` as
  tz-em instructed. Every SHA on the branch changed as a result; old → new
  for the commits referenced elsewhere in this log: `7e57894` → `588f4ec`,
  `fb3fe12` → `326ab83`, `f4b0bd8` → `4d7e0f9`, `35c9516` → `613e488`,
  `262f4c0` → `50c19aa`.
- **R1-5 (Nit):** this log's `utcTimeHook` reference (above) corrected
  to `entc.UTCTimeHook`; the PR body's inaccurate claim that "modernc's
  `_timezone` option only ever adjusts a value that isn't already
  UTC-located" corrected to describe what `formatTime` actually does
  (only calls `t.In(loc)` when `_timezone` is configured; otherwise
  formats the value as-is, so an already-`.UTC()` value is still
  canonical).

Verified post-fix: `go build -buildvcs=false -p 2 ./...`; `go test -p 2
-count=1 ./pkg/ent/...` and `go test -tags no_sqlite -p 2 -count=1
./pkg/hub/...`, each under default TZ, `TZ=Asia/Tokyo` and
`TZ=Asia/Kathmandu` — all green; `golangci-lint
run --new-from-rev=upstream/main --concurrency=1 ./pkg/ent/entc/...
./pkg/hub/...` — 0 issues; `gofmt -l` clean. Full `pkg/hub` suite
(default tags) re-run under `TZ=Asia/Kathmandu` after the R1 fixes:
still zero failures.
