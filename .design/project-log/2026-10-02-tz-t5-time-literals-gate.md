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
  - `ent-bind-formatted`: no raw SQL naming an ent table, and no ent dialect predicate
    (`EQ/LT/...`, `Field*`, `ExprP`/`Expr`, `Builder.Arg/Args` inside `P(func(b *Builder){...})`),
    binds a formatted time string. The ent dialect package is matched by import path, so the
    `entsql` alias used across `entadapter` is covered.
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
- `webchannel_store.go` restored to its pre-task-3 version: 9 findings. They are the
  TouchThread/RecordChannel time binds, the four `conversations` inserts that bind formatted
  text, and the attachment, edit and delete writers that lack `.UTC()`.
- The `SearchChatMessages` cursor revert binds a split client string, which no syntactic rule can
  see. Task 3's regression test covers it, as design §2.1.7 says for non-literal fixes.

## Review round 1

- The ent predicate rule had matched only the identifier `sql`, but the stores import the
  package as `entsql`, so on real code it never fired. It now resolves the import path, and it
  also covers `Field*`, `ExprP`/`Expr` and `Builder.Arg/Args`. Probe: binding a formatted
  `now` into `entsql.LTE` in `ListDueSchedules` now fails the gate (tried locally, then
  reverted).
- Before, a local SQL variable resolved to its first `:=` literal. Now it is resolved at the
  call: the latest assignment that must have run, assignments in branches, and `+=`
  fragments. The fixture `sqlvars.go` covers `+=` building, reassignment in both directions,
  self-concatenation, a branch, and an unresolvable reassignment.
- `var t time.Time` with no value now counts as UTC, because the zero value is UTC. The two
  redundant `.UTC()` calls on `lastSyncedAt` in hubsync are dropped.
- None of the fixed rules finds anything new in the tree; the allowlist is still empty. The
  revert count above was corrected from 13 to 9, which matches the old checker too.

## Review round 2

- A value-less `var t time.Time` loses its UTC status when it is also written through its
  address (`row.Scan(&t)`, `json.Unmarshal(b, &t)`) or by a decoder method
  (`t.UnmarshalText(...)`). Before this, the R1-3 change had hidden those. The hubsync
  `lastSyncedAt` cleanup stays valid, because that variable is only assigned `parsed.UTC()`.
- Aliases of `time.<Layout>` are now layouts: package or local `const` and `var`, `:=`, and
  copies of these. The header now matches what the checker does.
- SQL variables respect shadowing. A declaration in an if/for/switch init, a range variable, a
  function parameter or a nested block applies only inside its own scope. Assignments to the
  inner variable no longer leak into calls on the outer one.
- The header says Builder.Arg/Args is checked on function and closure parameters. A fixture
  covers the plain-function case.
- New fixtures: `layouts.go` and `scanned.go`, plus scoping cases in `sqlvars.go`. Each new
  positive case is missed by the round-1 checker, and each new negative case is a false positive
  in it. The tree is still clean, and the allowlist is still empty.

## Tests

- `hack/checktimeliterals/main_test.go`: fixtures under `testdata/src` carry `// want <rule>`
  annotations on positive and negative cases. The tests also cover allowlist parsing, stale
  entries and every exit code. `./hack/check-time-literals.sh --self-test` runs them.
- Touched packages were tested under TZ=UTC, Asia/Tokyo and Asia/Kathmandu (see the PR body).

## Known limits / follow-ups

- The checker is syntactic. It does not see layouts held in parameters or `%v`/`String()`
  formatting. Its SQL resolution does not model loop back-edges, `strings.Builder` or SQL
  returned by helpers. All of these are documented in the checker header.
