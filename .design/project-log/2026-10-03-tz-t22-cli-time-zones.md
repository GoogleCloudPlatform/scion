# CLI times always show a zone; global --tz/--utc

**Date:** 2026-10-03
**Branch:** scion/tz-t22 (tz-refactor task 22)

## Problem

CLI output formatted times in several overlapping ways: `time.RFC3339`,
zoneless literals such as `"2006-01-02 15:04"`, and four separate relative
helpers (`formatRelativeTime`, `formatTimeAgo`, `formatLastSeen`,
`formatScheduleTime`). Some wall-clock times had no zone, so a reader could
not place them. Some relative helpers showed future times as "just now".

## Solution

- New package `pkg/clitime` with one absolute helper, `Format(t, style)`, and
  one relative helper, `Relative(t)`.
  - The styles `Full`, `Minute`, `Clock` and `Date` all use 24-hour layouts
    that end in `MST`.
  - `Relative` handles both directions ("5m ago", "in 5m").
  - `Ago` is `Relative` for past-only instants (heartbeats, activity,
    created/joined/fired). A future value caused by hub/laptop clock skew
    reads "just now", as the removed helpers did, instead of "in 5m".
  - Token expiry prints `(in 23h)` / `(EXPIRED 2h ago)` through the same
    helpers instead of raw Go durations.
- Global persistent flags `--tz <IANA>` and `--utc`, resolved in root
  `PersistentPreRunE` through `clitime.ResolveZone`.
  - The two flags are mutually exclusive, and an invalid zone is an error.
  - Precedence: flags first, then the process local zone. There is no env var.
  - Both are flags, so `cmd/cli_mode.go` is unchanged.
- Every human-facing time in `cmd/` and `pkg/agent/list.go` now goes through
  clitime, and the old helpers and their tests are gone.
- JSON output is untouched: it still marshals `time.Time` or passes the
  API's UTC strings through unchanged.
- Gate: `hack/check-cli-time-zones.sh`, a go/ast checker in
  `hack/checkclitimezones`.
  - It rejects zoneless layout literals and the zoneless `time` constants in
    the CLI scope.
  - Wired as `make cli-time-zones`, included in `check-custom`, and run as a
    separate CI step.

## Decisions

- Date-only columns also show the zone (`2006-01-02 MST`), because the day
  depends on the zone. This also means the gate needs no allowlist.
- Relative wording is now compact everywhere. For example, the agent list
  shows "5m ago" instead of "5 minutes ago".
- A pending schedule time that is already due still reads "now", as before
  and as in the web scheduler views. Every other schedule time uses
  `clitime.Relative`, so a future next run reads "in X" instead of the old,
  incorrect "just now". `clitime.Now()` exposes the injected clock so this
  rule and `Relative` agree in tests.
- Review follow-up: cobra's `MarkFlagsMutuallyExclusive("tz", "utc")` on the root now rejects `--tz` with `--utc`, even after a subcommand; `clitime.ResolveZone` lets `utc` win because cobra checks flag groups only after `PersistentPreRunE`.

## Tests

- `pkg/clitime` unit tests across UTC, JST, EDT, +0545, +14 and SST.
- `cmd/clitime_golden_test.go` runs the real root command with the process
  zone pinned to Asia/Tokyo. It checks `messages` and `hub secret list` with
  the local zone, with `--tz America/New_York` and with `--utc`. It also
  checks JSON passthrough, the error for an invalid `--tz`, and `--tz` used
  together with `--utc`.
- Gate self-test with annotated fixtures.
- The touched packages pass under TZ=UTC, Asia/Tokyo and Asia/Kathmandu.

## Follow-ups noticed (not done)

- `hub secret list` ignores the global `--format json`; only its own
  `--json` flag works.
- `cmd/sciontool` prints RFC3339 in whatever zone the process uses. It is a
  separate binary and outside this scope.
