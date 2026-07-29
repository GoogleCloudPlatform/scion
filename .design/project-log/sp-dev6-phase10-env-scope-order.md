# Phase 10 — Gap 4: `runtime_broker` env scope demoted to lowest precedence

**Agent:** sp-dev6
**Branch:** `scion/sp-dev6` → target: `scion/settings-precedence-lead`
**Commits:** `574e33d8` (phase 10a, no behaviour change), `b172ca41` (phase 10b, breaking),
`10c` (boot wiring — see below)
**Base:** `ce23801a` (my Phase 9)
**Date:** 2026-07-29

## Summary

Hub env vars scoped to a runtime broker used to override every other scope. They are now
overridden by every other scope.

```
before:  hub < project < user < runtime_broker
after:   runtime_broker < hub < project < user
```

`runtime_broker` being highest was never a decision. It was an artefact of the order in which
four near-identical blocks happened to appear inside `resolveEnvFromStorage`, and it survived
because nothing stated the order in one place and no test pinned any pair of scopes against
each other. The product ruling (Q2 → variant 4-B, sub-decision → target (iii)) is that
broker-scoped env is the most infrastructural and least specific of the four scopes, so it
should be the weakest default rather than an override nobody can escape. The scope may be
removed entirely in a future release.

This is a **breaking change with no migration available.** The hub cannot distinguish a
broker-scoped value that an operator pinned deliberately from one set by accident, so it must
not try to fix them. The only warning that can be offered is to name the affected keys at boot,
which this phase adds.

## Why the behaviour change was one line

Phase 9 (`ce23801a`) extracted the ordering into a single list, `envScopePrecedence`, and made
`resolveEnvFromStorage` and `buildEnvSources` range over a shared helper derived from it. So
phase 10b's actual behaviour edit is moving one entry in that slice. The resolver, the
provenance reporter and the new startup warning all read the same list and could not be left
behind — which was the entire point of doing the extraction before the answer arrived rather
than after.

## What changed

### `pkg/hub/httpdispatcher.go`

- `envScopePrecedence` — `store.ScopeRuntimeBroker` moved from last to first.
- Its doc comment rewritten. Three things it now says that it did not before:
  - **This is the STORAGE-scope ladder only.** Templates, harness overrides, profiles and
    project annotations sit between these scopes and the final config and are resolved
    elsewhere. Four names in a row are not a complete ordering, and the comment says where it
    stops.
  - **Where agent config sits, precisely.** `buildCreateRequest` seeds `ResolvedEnv` from
    `AppliedConfig.Env` and storage then fills only keys config left **absent or empty**. A
    non-empty config value outranks all four scopes; an empty one is a passthrough marker that
    deliberately yields to storage. That rung is not a plain inequality and a total order
    cannot express it.
  - **Why broker is last**, in terms that do not invert if read casually.
- `envScopesOutranking(order, scope)` — new. Who beats whom, derived from the ladder.
- `envScopeCollisions(order, target, vars)` — new, pure. The keys defined at `target` that are
  also defined at a scope outranking it.
- `WarnOutrankedBrokerEnvKeys(ctx)` — new, exported. One query per scope; logs each shadowed
  key with its broker IDs and the scopes that now outrank it.
- `resolveSecrets` doc comment — added the missing `hub` rung and a warning that **secrets rank
  these same four scopes differently on purpose** (issue #624).

### `cmd/hub_env.go`

Scope list reordered, ladder corrected to `broker -> hub -> project -> user -> agent config`,
the empty-string passthrough marker documented, and a "Changed in this release" note.

### `pkg/hub/httpdispatcher_envscope_test.go`

Criterion 18 as seven rows, plus the collision and warning tests. Detail below.

## The startup shadow warning

`WarnOutrankedBrokerEnvKeys` derives who-outranks-whom from `envScopePrecedence` instead of
hard-coding it. Two consequences worth recording:

1. Under the **old** ladder it is inert by construction — nothing outranks `runtime_broker`, so
   it returns before issuing a single query. Under the **new** ladder every scope outranks it.
   The same one-line edit that causes the flip turns on the warning about the flip. They cannot
   be shipped apart.
2. It **over-reports deliberately.** It matches on key alone: it does not compare values, and it
   does not check that the broker and the higher-scoped entity share any agent. A false positive
   costs a line of boot log; a false negative costs an operator a pinned value they never learn
   about. There is a test asserting the over-reporting is intentional so nobody "optimises" it.

### Wiring it (phase 10c)

Through `b172ca41` the warning had **no caller**: present, tested, and silent in production. That
is the workstream's own central pattern arriving in the wiring layer — *it looks done in the diff
and it does nothing at runtime* — so it was flagged rather than reported as delivered.
`cmd/server_foreground.go` is outside this phase's file set, so permission was requested from
`sp-em` rather than taken; it was granted with three conditions, answered below.

**It runs synchronously, and that is deliberate.** The obvious form —
`go func() { dispatcher.WarnOutrankedBrokerEnvKeys(ctx) }()` — reproduces the exact defect the
check exists to report, one level down. If `ctx` were cancelled between step 14 and the point the
queries ran, no warning would ever be emitted and the diff would still look complete. A handful of
small SELECTs on a path that runs once per process does not justify that.

**What `ctx` is at the call site.** It is the process-lifetime context: `context.WithCancel(
context.Background())` at step 7, cancelled in exactly three places — `defer cancel()` on return
from `runServerForeground`, the SIGINT handler, and the `errCh` branch of the final `select`. It is
not request-scoped and it is not replaced anywhere between step 7 and step 14. The only way it is
already cancelled at the call site is a signal arriving mid-boot, i.e. the process is going down
anyway.

**The failure branch says DID NOT RUN, not "failed".** From the operator's side those are the same
event: no list of shadowed keys was produced. Logging `"failed to check"` would read as a checked
failure — as though the check ran and hit a snag — when what actually happened is that the check
did not happen and the absence of warnings carries no information. The log says so in those words,
and a test pins the wording for both a store error and `context.Canceled`.

**Log library.** `log/slog` is already imported and already used for warnings in this file
(step 12). No new logging dependency, and no `log.Printf`/`slog` mix introduced at the call site —
the adjacent `log.Printf("Agent dispatcher configured")` is pre-existing and untouched.

**What pins the wiring, and what does not.** `runServerForeground` is not callable from a test, so
the call site was extracted into `warnShadowedBrokerEnv(ctx, w brokerEnvShadowWarner)`, which is
unit-testable against a fake:

- `TestWarnShadowedBrokerEnv_InvokesTheCheck` — the helper actually calls the method, once, with
  the context it was handed.
- `TestWarnShadowedBrokerEnv_FailureSaysDidNotRun` — both error shapes produce a DID NOT RUN log.
- `TestWarnShadowedBrokerEnv_SuccessLogsNothing` — the negative control. Without it, a helper that
  logged DID NOT RUN unconditionally would satisfy the two tests above.
- `var _ brokerEnvShadowWarner = (*hub.HTTPAgentDispatcher)(nil)` — renaming the method or changing
  its signature breaks the build rather than quietly leaving boot calling nothing.

None of that pins that **step 14 calls the helper.** Delete that one line and every test above
still passes, which is the failure being guarded against. So there is one more, labelled for
exactly what it is: `TestBootPathCallsWarnShadowedBrokerEnv` is a **drift guard over source text,
not a correctness check.** It greps the boot file for the call. It does not cover that step 14
executes, that `enableHub` is true, that the dispatcher passed is the live one, or that the store
behind it is reachable. It fails only when the line disappears — which is the one thing nothing
else in the suite can see.

Both guards were mutation-tested rather than assumed: deleting the call site reddens the drift
guard alone; making the helper stop calling the method reddens the three unit tests and correctly
leaves `SuccessLogsNothing` green.

## Criterion 18 — the discrimination table

Four scopes, six unordered pairs, plus the all-four case. Every pair seeds **only the two scopes
it names**, so each is discriminated directly rather than held up by transitivity through the
rest of the ladder.

| # | pair | winner | discriminated | moved in 10b |
|---|---|---|---|---|
| 1 | hub vs project | project | directly | no |
| 2 | hub vs user | user | directly | no |
| 3 | project vs user | user | directly | no |
| 4 | hub vs broker | **hub** | directly | **yes** |
| 5 | project vs broker | **project** | directly | **yes** |
| 6 | user vs broker | **user** | directly | **yes** |
| 7 | all four | user | winner only, **not order** | **yes** |

**No pair holds only by transitivity.** That was worth checking rather than assuming: with a
two-scope fixture the winner is whichever of the two appears later in `envScopePrecedence`, so
each row fails on its own if that pair swaps.

Row 7 is the one that needs a label. `sp-rev2` demolished the previous version of criterion 18
by mutation: "a key defined in all four scopes resolves to the scope the doc comment names"
survived **both** swapping user with project **and** deleting user from the ladder entirely,
because the all-four case pins the *winner* and leaves everything below the top scope
unconstrained. Row 7 is kept for the winner and explicitly disclaimed for the order.

Row 7 also says "user wins **among the four storage scopes**" and nothing more. Agent config,
which carries request and `--config` env, outranks all four.

## Red-before / green-after

Test expectations were changed to the (iii) ladder **before** the source edit, and the suite run
in that state:

```
exit=1, guard PASS (14 wanted / 14 selected)
--- FAIL: TestEnvScopesInPrecedenceOrder_ListsAllFourScopes
--- FAIL: TestWarnOutrankedBrokerEnvKeys_LogsShadowedKeys
--- FAIL: TestResolveEnvFromStorage_ScopePrecedence
--- FAIL: TestResolveEnvFromStorage_PairwisePrecedence
    --- FAIL: .../hub_beats_broker
    --- FAIL: .../project_beats_broker
    --- FAIL: .../user_beats_broker
```

Exactly the three inverting pairs, the all-four case, the helper's own order, and the warning
(inert under the old ladder, so it named nothing). **The three non-inverting pairs stayed green
throughout**, which is what makes the three reds mean something rather than being a suite that
went red for any reason. After the one-line reorder: 14/14 PASS, 0 FAIL.

## Two design defects found and routed on discovery

Both were sent **directly to `sp-arch`, copying `sp-em`**, before writing a line of the code that
would have carried them. Both were accepted.

1. **§3.4 asserted a relation as settled that was open.** The doc said "certain under all three
   readings: `runtime_broker` loses to project and to user." Option (ii) is
   `hub < project < runtime_broker < user` — under it broker **beats** project, and that option's
   own stated rationale two lines above says so. The intersection of the three options was one
   relation, not two. This mattered because criterion 18 was being written from that sentence at
   the time, and would have pinned a direction the ruling did not support.
2. **The wider ladder relayed for the reference doc had its top two rungs inverted.** It put user
   scope env above request/`--config`; the code has config seeded first and storage filling only
   what it left absent or empty. I asserted only the half I could verify — that
   `AppliedConfig.Env` outranks all four storage scopes — and declined the half I did not own
   (that request env reaches `AppliedConfig.Env`). `sp-arch` verified that half and closed the
   chain.

The rule both came out of, and the reason the doc comment above is shaped the way it is:
**never state a partial ladder without saying where it stops.** A descending list that stops
early is true in every relation it states and false in what it implies by stopping.

## Deviations

- **Edited the `resolveSecrets` doc comment**, which is in my file but outside the env region.
  Behaviour-free. Reason: demoting broker for env makes that comment actively dangerous, because
  env now ranks broker *lowest* and secrets rank it *highest* in the same file, and the next
  reader to notice will reach for consistency and make one of them a lie. Disclosed to `sp-em`.
- **Two commits rather than one**, so that the breaking change is a diff a reviewer can read on
  its own, with the test expectations moving inside it.

## Known-adjacent, not fixed

- Env and secrets rank `user`/`project` in opposite directions (issue #624). Untouched; this
  phase widens the divergence on the broker axis too, and warns about it in the comment.
- `runtime_broker < hub` is **not** a uniform rule across subsystems. It holds for env vars and
  for limits, but the broker's own `settings.yaml` contributes to Resources at three ranks, two
  of them above hub (`sp-rev-p8`'s measurement). These are two different precedence systems and
  the reference doc must not join them.

## Phase 10d — review fixes from `sp-rev-p10` (CHANGES REQUESTED on `ce23801a..14883cbf`)

Four edits, three of them text and one a regex. No behaviour change: the ladder,
the resolver, the reporter and the boot wiring are all untouched. New commit on
top of `14883cbf` — `14883cbf` is merged into `scion/sp-integration @ d2989488`
and must not move.

**F-1 — `httpdispatcher.go`, the inertness paragraph on `WarnOutrankedBrokerEnvKeys`.**
The comment said the check is inert "which is the case for the ladder currently
in `envScopePrecedence`". That was true when 10a wrote it and 10b inverted the
ladder underneath it without updating the prose. Under the shipped ladder
`runtime_broker` is weakest, `envScopesOutranking` returns hub/project/user, and
the check is LIVE. Rewritten to say so, and to say what *would* make it inert.
This is the comment a maintainer reads before deciding whether the 10c call site
is load-bearing, and it was telling them it was a no-op.

**F-2 — `cmd/server_foreground_brokerenv_test.go`, the boot drift guard.**
`strings.Contains` is satisfied by a commented-out call site, so the guard's
label "it only fails when the line disappears" was an overstatement in the
direction that matters. Replaced with an anchored `(?m)` pattern requiring
statement position, and the declared-gaps list now records what the guard still
cannot see (a call present at statement position but unreachable). Demonstrated
by mutation rather than asserted — see the table below.

**F-3 (sentence only) — `httpdispatcher.go:1092`.** The claim that changing the
order here "is the ONLY edit required to change it everywhere" is false:
`Server.buildEnvGatherResponse` in `handlers_agents_core.go` answers the same
provenance question from its own hardcoded chain, defaults the reported scope to
`hub`, and never consults `runtime_broker`, so a broker-only key is reported
there as `hub`. Verified read-only before writing the claim down (`runtime_broker`
occurrences in that function: 0; `envScopePrecedence` references: 0). Per
`sp-em`'s scope ruling the *sentence* is fixed and the *gap* is not — that
reporter is a tracked follow-up and this phase does not touch its file.

**N-1 — `httpdispatcher.go:1072`.** "runtime_broker is deliberately LAST" sat
above a lowest-first slice in which `runtime_broker` is element 0. The word was
true of precedence and false of the literal. Replaced with the spelled-out
sequence, per design §3.4's "a literal sequence, not a word".

### F-2 mutation evidence

A fix to a mutation-detection gap is only demonstrated by the mutation, so all
four arms were run, serially, with `df` checked before and after (96% both ends,
no threshold crossed):

| arm | mutation | result |
|---|---|---|
| baseline | none | 4/4 green, `rc=0` — also the positive control that the new pattern matches at all |
| **M7** | prefix the call site with `// ` | **RED**, `rc=1`, exactly one failure |
| M4 | delete the line | RED, `rc=1`, exactly one failure |
| restore | none | 4/4 green, file byte-identical to `HEAD` |

M7 previously passed; that is the whole point of the change. The failure was
read as a *body*, not a count — the assertion text is
`boot path no longer calls warnShadowedBrokerEnv(ctx, dispatcher) at statement
position — deleted, or commented out`, which distinguishes the guard firing from
a build failure. `anchored-FAILs=1` alone would not have.

Envscope suite re-run under `-race`: 14/14, `ok 92.082s`. Build and vet clean.
File set is exactly two files; `handlers_agents_core.go` is untouched.

## Phase 10e — the second unscoped claim (`29f5279d`)

`e2514675` fixed finding F-3 at the `envScopePrecedence` doc block and left the same claim standing
at `envScopesOutranking`, ~60 lines below: *"changes who outranks whom everywhere at once — the same
property that makes the resolver and the provenance reporter unable to drift apart."* The file
therefore shipped a scoped and an unscoped statement of one property, with the unscoped one attached
to the function that computes the answer.

`buildEnvGatherResponse` is a provenance reporter, does not read `envScopePrecedence`, and can
drift. Verified read-only before the claim was written: `runtime_broker` occurrences inside it are
0, `envScopePrecedence` references are 0, and `envScopeSourceLabel` maps `store.ScopeRuntimeBroker`
to `"broker"` — so the two reporters disagree about the same key today.

`29f5279d` scopes the comment to the three consumers in this file and states explicitly that it
covers no reporter which does not read the list. Comments only, one file, fast-forward from
`e2514675` (which is merged and did not move).

**Method notes worth keeping.**

- *Comments-only was proven, not asserted*, with a live negative control: the filter returns 0 on
  this diff and **84** on `ce23801a..14883cbf`. A filter must be bounded on both sides by its own
  universe — one returning 0 and one returning everything are both broken and neither announces
  itself.
- *A line-oriented grep cannot see a wrapped phrase.* The phrase spanned lines 1189–1190, so a
  phrase count was **equal** at both revs and read as "nothing changed". The count was arithmetically
  right about a quantity nobody meant. Normalise whitespace, or match a fragment short enough not to
  wrap.
- *The SKIP fence broke on first use, fail-closed.* Widening the RUN pattern to add a SKIP limb lost
  the end-of-line anchor, so subtest `=== RUN` lines counted against a source-derived WANT (45 vs 14,
  6 vs 4). Count top-level RUN only, anchored; require `SKIP == 0`; require the `ok` line. Add one
  limb per edit and control it before adding the next.
- *A green under disk pressure needs the run count.* `t.TempDir` succeeding and a later write hitting
  ENOSPC under a `t.Skipf` handler yields a green manufactured by disk exhaustion. Both suites here
  are structurally immune: `t.Skip` count 0 and `t.TempDir` count 0.
- *Gap-4 counts are scoped.* 7/0 before and 0/2 after are claims about `pkg/hub/httpdispatcher.go`.
  Package-wide the hardcoded limb is **4**, all in `handlers_agents_core.go`, unchanged across
  `b03a09ac`, `f0093316` and `d2989488`. A three-limb gate run without that pathspec red-lights the
  correct tree.
