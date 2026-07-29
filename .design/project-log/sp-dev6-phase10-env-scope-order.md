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
