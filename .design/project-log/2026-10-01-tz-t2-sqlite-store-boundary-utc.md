# tz-refactor task #2 (U2a): SQLite store boundary in UTC

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
- **Ent mutation hook.** `utcTimeHook` calls `.UTC()` on every
  `time.Time` field value present in a mutation (explicit sets and
  schema `Default`/`UpdateDefault` values, since ent populates defaults on
  the mutation before hooks run). Registered via `client.Use(...)` in
  `OpenSQLite`, `OpenSQLiteReadOnly` and `openPostgres`. On Postgres
  (`field.Time` → `timestamptz`) this changes nothing persisted; its only
  effect is that a non-UTC input is not echoed back non-UTC in the
  create/update response.
- **Predicates.** No predicate sites were in scope after the two
  carve-outs above — the only predicate sites named in `impl-issues.md`
  task #2 are the agent-store thresholds (owned by #2470) and the
  message-store purge/cursor sites (owned by #2553). The DSN option alone
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
  Kathmandu without needing PR ptone/scion#2470/#2476).
- `golangci-lint run --new-from-rev=upstream/main --concurrency=1
  ./pkg/ent/entc/...` — 0 issues.
- `gofmt -l` on changed files — clean.
- `go test -p 2 -count=1 ./pkg/hub/... ` under `TZ=Asia/Kathmandu`
  (createTestStore-backed tests, the ~275-test baseline from
  dev-common.md): see report to tz-em for the full breakdown (which of
  those now pass vs. which still hit the agent-store/schedule-store
  thresholds owned by #2470/#2476).

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
change" section) so task #5's `make time-literals` gate treats these
sites as already correct.
