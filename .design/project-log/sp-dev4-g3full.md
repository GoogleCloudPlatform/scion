# Settings Precedence, Phase 5 (2nd commit) — G3-full: retire `profiles.<name>.env` from harness-config resolution

**Agent:** sp-dev4
**Branch:** scion/sp-dev4-g3full (off `dacbedeb`)
**Commit:** 4951f5c6
**Date:** 2026-07-29

## Summary

Deleted the `profile.Env` merge from harness-config resolution — the change the
workstream calls **G3-full**. Two merge sites, both of `profile.Env`:

1. `ResolveHarnessConfig` (`pkg/config/settings_v1.go`)
2. legacy `Settings.ResolveHarness` (`pkg/config/settings.go`)

`profiles.<name>.harness_overrides.<hc>.env` **survives**. The
`V1ProfileConfig.Env` **field** survives — it is still parsed, still populated,
and still read by `ResolveRuntime` and by the broker's required-secret
inspection. Only the merge into harness-config resolution is gone.

Product-owner rationale ("pare down the number of control and injection
points") is recorded verbatim **in `settings_v1.go` at the deletion site**, not
only in the CHANGELOG, because it is the only statement of intent that exists
for the removal and a future reader at the deletion site is the one who needs
it.

## THE REVERSAL, AND THE FACT THAT I HELD RATHER THAN COMMITTED

**This is the part of this entry that matters.** It is recorded at `sp-em`'s
explicit instruction, and the instruction was right: the hold is the reason this
cost four lines instead of a revert on `main`.

Scope moved three times in about nine minutes:

| time | source | ruling |
|---|---|---|
| 20:15 | lead, relayed by `sp-em` 20:16:28 | G3-full = **both** deletions: delete `harness_overrides.<hc>.env` too |
| 20:20:15 | `sp-arch` | G3-full = the `profile.Env` merge **only**; the override **SURVIVES** |
| 20:21:41 | lead reverses **its own** 20:15 call | `sp-arch`'s ruling stands |

By the time the second ruling landed **I had already implemented the contested
half** — all four deletions were in my working tree, green, with tests.

**I did not commit.** I held, and escalated with the two rulings quoted verbatim
and timestamped, stating plainly which one I had already built and that
reverting was cheap *now* and would stop being cheap the moment it was committed.

Two things made the hold correct rather than merely cautious:

- **Each ruling named the other party as the owner.** `sp-em`'s relay cited a
  lead ruling; `sp-arch`'s ruling deferred dispatch scope to `sp-em`. A reader
  of *either one alone* would have been certain. That is not two agents
  disagreeing about a fact — it is a **gap in the authority chain**, and the
  only safe move against an authority gap is to stop, because every additional
  step taken inside it has to be undone.
- **Asymmetric reversibility.** The smaller deletion first is additive. Shipping
  both and restoring one later is a *revert of a documented breaking change* —
  it means publishing "removed" for a path that still works, then unpublishing
  it. Cost of holding: minutes. Cost of guessing wrong: a false breaking-change
  notice in a shipped CHANGELOG.

Standing rule adopted into `COMMON-CONTEXT.md` §0 as a result (`sp-dev3`'s
wording):

> **A scope that has already been contradicted once is contested until one named
> owner confirms it, no matter who relays the next answer.**

And the operational half `sp-em` took: **a relayed ruling must name the ruler,
the timestamp, and what it supersedes.** The 20:16 relay named the ruler and the
time and *not* what it superseded — which is exactly why `sp-arch` could rule
against it without either of them knowing.

Final disposition (`RULING-g3full-SUPERSEDING.md` §1): **keep 1 and 2, revert 3
and 4.** Reverted before commit. The tree never carried the contested deletions
into history.

## I refuted the ruling's reason (2), against my own interest

`sp-arch`'s ruling rested on two reasons. Reason (2) said deleting the override
would close the migration path for the removed profile env.

**I measured that reason (2) is wrong**, and said so while holding the opposite
implementation in my tree. The migration path is `harness_configs.<hc>.env` — a
different, **top-level** key that survives either way and is the *base* argument
to the merge. It has nothing to do with `harness_overrides`.

I stated at the same time that this **does not change the ruling**, because
reason (1) — reversibility — carries it alone. Reason (2) is struck as a reason;
the ruling stands.

Recording it because the point generalises: **a correct conclusion resting on a
refuted premise is the same defect as a correct test resting on an accidental
fixture.** Both are green for a reason that will not survive the next edit.

## The control shape — the load-bearing part of the tests

G3-full deletes the **middle** rank of a three-rank order:

```
harness_overrides.<hc>.env  >  profiles.<p>.env  >  harness_configs.<hc>.env
```

Profile env was passed as the **override** argument to `mergeMaps`, so it
**outranked** `harness_configs.<hc>.env` on any shared key.

Two consequences that cost the workstream real time:

**1. The obvious test is the wrong test.** "Harness-config env now takes
precedence over profile env" is *not* what this change establishes. That claim
was false before the change, and afterwards it is not merely **vacuous** — it is
**still false**, because the surviving override still outranks the base. Any
test of that shape fails today and would pass after the deletion *for the wrong
reason*.

**2. A middle-rank deletion is invisible whenever a higher rank is populated.**
The fixture reads as coverage and measures nothing.

The first correction to (2) was: *leave every higher rung unpopulated.* That
rule was **right in its conclusion and wrong in its mechanism**, which
`sp-rev-p5` caught and `sp-arch` adopted verbatim. The masking argument is a
**scalar-ladder** argument, and `Env` is a **per-key map** — `mergeMaps` overlays
key by key, so the top rung masks the middle rung *only for the keys it itself
sets*. `sp-rev-p5` measured it: an overlapping key masked exactly as predicted,
while a profile-only key was **fully observable with the top rung populated**.

General form, which covers both cases without needing a taxonomy:

> **THE DISCRIMINATING FIXTURE IS THE ONE WHERE THE DELETED RUNG IS THE SOLE
> SOURCE OF SOME OBSERVABLE.**

Scalar rung → masking is total → leave every higher rung unpopulated. Map or
list rung → masking is per key → the deleted rung must set at least one key **no
higher rung sets**, and higher rungs *may* be populated.

Why this matters past this commit: the old form **generates false positives**. A
reviewer holding it sees a populated top rung, declares the control vacuous, and
deletes a test that works perfectly.

`TestResolveHarnessConfig_ProfileEnvNotMerged` therefore sets **all three**
rungs and asserts per key:

- `PROFILE_ONLY_KEY` — profile only, no higher rung sets it → **disappears**
- `OVERLAP_KEY` — set by the override → **unchanged**, proving the override survived
- `SHARED_KEY` — resolves to the harness-config value, proving the base was left undisturbed
- profile `Volumes` — **existence control**, so the absence assertions cannot pass
  because the profile was never found at all

One fixture pins both halves of criterion 17 and exercises the real-world
migration state, which the unset-override version could not.

**Red confirmed, not inferred.** Reverting the deletion turns `PROFILE_ONLY_KEY`
red *with the override populated* — run and pasted, with a `=== RUN` line
checked, because a build failure and an assertion failure print the same word.

## Tests migrated, not weakened

This commit is **not pure-additive in test files**, which trips the append-only
gate. Named here so it is adjudicated rather than discovered:

- `TestProvisionAgentEnvMerging`, `TestStartInjectsProfileEnvForAuth` — fixtures
  moved from `profiles.<p>.env` to `harness_configs.<hc>.env`, **every assertion
  byte-identical**. These are the executable form of the migration note.
- `TestResolveAuthEnvOverlay_ProfileEnvStillArrivesViaHarnessConfig` — renamed
  and **inverted** to `...ProfileEnvNoLongerArrivesViaHarnessConfig`. It asserted
  the retired behaviour. Given an `HC_ONLY` existence control so the new absence
  assertion cannot pass vacuously.
- `TestEnvMerging` — **one key**, `H2`, `"P2"` → `"V2"`. The legacy mirror of the
  ladder; `H2` is the only key positioned to observe the deletion. Flagged to
  `sp-em` as a breaking change recorded, **not** an assertion relaxed to reach
  green. `sp-em` did not have this test in view when ruling Decision 2, so it was
  raised as adjudicable rather than quietly edited.

`TestLegacyAndVersionedResolution_SameResult` forces both resolvers to delete the
**same** rung, so the legacy deletion was required for *agreement*, not for
reach — the legacy method has zero non-test callers.

## Two findings raised rather than silently resolved

**`run.go` scope.** Decision 4 (20:13:32) explicitly approved rewriting a stale
comment in `pkg/agent/run.go`; §6 of the superseding ruling then said *"do not
touch `run.go`. Your commit is `pkg/config` only."* Rather than pick one, I
measured which parts were load-bearing: `pkg/config`-only ships **three red
tests** (`TestProvisionAgentEnvMerging`, `TestStartInjectsProfileEnvForAuth`,
`TestResolveAuthEnvOverlay_ProfileEnvStillArrivesViaHarnessConfig`), so the
literal reading is not executable; but the `run.go` edit alone is **comment-only
with zero code lines**, so §6 *is* satisfiable at zero cost to green. `run.go`
is dropped from the commit and its drafted text handed to `sp-dev7` as a patch
for the Phase 11 doc row. The two `pkg/agent` **test** files stay, because
Decision 1 (lead-confirmed) requires them by name.

**Criterion 17a — and my own count was wrong, in the direction that flatters my
instrument.** I measured the v0 `Settings` **type** as constructed in production
at **5** sites (`koanf.go:179`, `koanf.go:202`, `settings_v1.go:1796`,
`settings_v1.go:1799`, `cmd/server_foreground.go:198`). `sp-rev2` independently
measured **86**. I have not reconciled the gap and I am not going to paper over
it: my pattern was narrower than the question, **for the third time today**, and
the fact that my five were all real is exactly what made the number look
finished. *Five true positives do not bound the false negatives.*

The conclusion is unaffected, and that is the interesting part. `sp-arch` has
since ruled that the gate it originally specified — *"does anything construct a
v0 `Settings` in production"* — **was the wrong gate**, and that used as
written it argues **for** a CHANGELOG entry that must not ship. `v1` is the
**file schema**; `v0` is **what the program holds in memory**. So the type is
loudly live and it does not matter: the deciding question is **method**
reachability, and `ResolveHarness` has **zero** non-test callers, positive-
controlled against `ResolveHarnessConfig` at **5** — same grep shape, so the
zero is a measurement rather than a silence. **A live type with a dead method on
it.**

Both my number and `sp-rev2`'s answer a question that turned out not to decide
anything. Mine was additionally too small.

**Consequence I am raising and not resolving:** 17a as re-gated says the v0
deletion is **not user-visible** and must get **no CHANGELOG entry** — a
breaking-change notice for an unreachable path is a false notice inverted. **Row
3 of the superseding ruling gives it one.** Under the standing rule, a scope
contradicted once is contested until one named owner confirms it, so I have
neither dropped nor kept Row 3 on my own authority. Flagged to `sp-em` with both
citations.

Also noted for Phase 11: Row 3's phrase *"the pre-v1 harness resolver"* uses
"pre-v1" for the **type**, which `sp-arch` has just reserved for the **schema**
only. Row 3's other use — *"settings files still on the pre-v1 format"* — is
schema and is fine.

## Methodological findings worth keeping

**The `§0.3.1` enumeration defect.** `design.md` claimed three merge sites; there
are **five**. Its sweep keyed on the identifier `rtConfig`, which by construction
cannot find a merge into a harness struct (the variable is `result`) or a
non-merge read. Routed to `sp-arch` unbundled. *The search space was chosen from
the hypothesis, not the subject.*

**My own enumeration had the same defect.** I found the five sites with a literal
`profile\.Env` pattern, which by construction cannot find a site where the
profile is bound to another name (`p.Env`). At `dacbedeb` no such site exists —
**so the answer was right and the instrument was wrong.** Worth more than the
answer.

**Drift vs error in citations.** `sp-dev3` reported `run.go:472` as a live
CLI-mode channel. Measured across four revs, each guarded with
`git cat-file -e <rev>^{commit}` and a must-be-absent negative control: present
at `9364aa6a` and `ff835b0b`, **absent at `dacbedeb` and HEAD** — deleted by my
own earlier Phase 5 commit and extracted into `resolveAuthEnvOverlay`.
*Everything was true except the rev.* Retraction accepted by `sp-dev3`, `sp-arch`
(who re-measured independently and un-amended `design.md`) and `sp-em`.

The general shape: **a stale line number resolves to real code that is
confidently the wrong THING; a stale working tree resolves to real code that is
confidently the wrong VERSION. Both fail by resolving successfully** — which is
why a line number is a citation and needs a rev. I only caught this one because
`sp-dev3` had stated its rev.

`sp-arch` sharpened this against the obvious reading: `sp-dev3` **did** state its
rev and still made the error, so **stating the rev made the mistake auditable,
not less likely** — and we had been treating those as one property. The cheap
missing step sits on the author side and it is now mine to carry: **when you
report a finding against someone else's commit, measure at THEIR parent, not at
yours.** The relay side has its own repair (re-pin on relay), which `sp-em`
claimed. Generalisation worth more than the incident: **the working tree is the
one rev you never have to type, so it is the one that never gets audited.**
Every "I grepped the repo" claim today — *including the correct ones, including
mine* — was made against whatever tree its author happened to be sitting on.

## Consequence for the merge, owned by `sp-em`, not by me

The else-branch deletion exists on the **`dacbedeb` lineage only** — `a13ff174`
and `9364aa6a` still carry it. So *"is the CLI-mode profile-env read gone?"* is
currently **branch-dependent**, and a bad merge resolution would silently
reinstate a retired injection point. Criterion 17 must therefore run on the
**integrated** branch, never on a dev tip.

## Verification

- `go build ./...` — clean
- `go test ./pkg/config ./pkg/agent ./pkg/runtimebroker -count=1` — all `ok`
- `go test ./pkg/hub -count=1` — `ok` (178s). **Never run `./pkg/hub -race`; it
  hangs indefinitely.** Targeted `-race -run` is safe only when anchored to full
  test names.
- Residual greps: `mergeMaps(result.Env, profile.Env)` = **0**;
  `mergeMaps(result.Env, override.Env)` = **2** (restored in both resolvers)
- `harnesses/` untouched by this commit — AC6 blob
  `94af67d8db5f50ed338f1ef65e0d126dbc0e5a77` not re-run, per instruction that it
  is settled and measured across all seven tips
- `internal/fixturegen` `TestFixtureCoverage` is red on `main` (#625) — pre-existing,
  excluded, not caused by this commit
