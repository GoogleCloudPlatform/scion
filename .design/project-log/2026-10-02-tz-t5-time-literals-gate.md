# tz-refactor task 5: `make time-literals` regression gate

Closes ptone/scion#2498. Refs ptone/scion#2457. Design: tz-refactor design §2.1.7 (AC14).
Stacked on tz-refactor task 3 (GoogleCloudPlatform/scion#2289).

## What changed

- **Gate.** `hack/check-time-literals.sh` (target `make time-literals`, part of `check-custom`
  and so of `make ci`; its own step in `.github/workflows/ci.yml`). It builds and runs
  `hack/checktimeliterals`, a go/ast checker (no new dependencies, about 0.5 s over 461 files).
  It covers `pkg/hub` (including `githubapp`), `pkg/store`, `pkg/runtimebroker`, `pkg/hubsync`,
  `pkg/sciontool/hub` and `pkg/runtime/cloudrun`, and has three rules:
  - `format-utc`: `R.Format(layout)` / `R.AppendFormat(b, layout)` needs a receiver that is
    provably UTC. That means `.UTC()` or `.AsTime()` (protobuf returns UTC), Add/Truncate/Round
    of such a value, or a local variable whose every assignment is one of those.
  - `ent-bind-formatted`: no raw SQL naming an ent table, and no ent `sql.EQ/LT/...` predicate,
    binds a formatted time string.
  - `webchat-bind-time`: no raw SQL naming a `webchat_*` table binds a `time.Time` in the SQLite
    store. `*_postgres.go` is exempt, because its webchat columns are `TIMESTAMPTZ`.
  - Binds are read from direct arguments and from `args...` slices built with `append` or a
    `[]any{}` literal.
- **Allowlist.** `hack/time-literals-allowlist.txt`. Each entry is
  `file | function | finding | justification`, with no line numbers. A stale entry fails the
  gate. The list is currently empty, because every site was fixable.
- **Sites.** 30 `format-utc` findings on the stacked base, all fixed with `.UTC()`. This is
  instant-preserving; only the rendered offset changes. They are wire fields, opaque cursors
  (their decoders `time.Parse` and compare instants, so old cursors still work), log fields,
  Cloud Logging filters (`logquery.go`, `cloudrun/logs.go`), broker `deletedAt` query
  parameters, hubsync state, the GitHub token expiry file, and `metrics_dashboard.go`
  `queryDailyTimeSeries`/`queryGroupedTimeSeries`. Those two now bucket on UTC days, matching the
  "(UTC)" labels that tz-refactor task 7 added. Their third site (`AsTime()`) was already UTC.
- **Verified, not touched:** task 1's `events.go` sites (PublishAgentStatus x2,
  PublishAgentCreated, PublishUserMessage), `mintGitHubAppToken` and
  `handleAdminInvitesCreate`. Task 3's webchat writers and binds.
- Nothing was flagged in the files owned by ptone/scion#2476.

## Revert check (AC14), done by hand

These checks were run against the gate binary:
- Each task-1 `.UTC()` removed in turn (events.go x4 including the literal-`Z` layout, the
  GitHub webhook, admin invites): fails, one finding each.
- `webchannel_store.go` restored to its pre-task-3 version: 13 findings. They are the
  TouchThread/RecordChannel time binds, the four `conversations` inserts that bind formatted
  text, and the attachment, edit and delete writers that lack `.UTC()`.
- The `SearchChatMessages` cursor revert binds a split client string, which no syntactic rule can
  see. Task 3's regression test covers it, as design §2.1.7 says for non-literal fixes.

## Tests

- `hack/checktimeliterals/main_test.go`: fixtures under `testdata/src` carry `// want <rule>`
  annotations on positive and negative cases. The tests also cover allowlist parsing, stale
  entries and every exit code. `./hack/check-time-literals.sh --self-test` runs them.
- Touched packages were tested under TZ=UTC, Asia/Tokyo and Asia/Kathmandu (see the PR body).

## Known limits / follow-ups

- The checker is syntactic. It does not see layouts held in parameters, `%v`/`String()`
  formatting, or SQL built from non-literal fragments; these are documented in the checker
  header.
