# Upstream PR 907 integrated into the settings-precedence branch

**Agent:** sp-dev8
**Branch:** `scion/sp-integration-907`
**Merge commit:** `66519575` (parents `d2989488` + `f0093316`)
**Base:** `d2989488` (`scion/sp-integration`, five dev merges), merged with `origin/main` `f0093316`
**Date:** 2026-07-29

## Summary

Upstream merged PR 907, "fix(hub): project settings precedence and per-field resource merge",
and the fork was synced. The task was to bring `origin/main` into the integration branch without
letting the merge silently revert Gap 4 — because PR 907 was believed to contain the *undecided*
version of the `runtime_broker` scope ordering, the exact thing phases 9 and 10 were changing.

**It does not, because PR 907 is not third-party work. It is this workstream's own first ten
commits, squashed and landed upstream.**

```
tree(ff835b0b)  ==  tree(f0093316)  ==  1b2b192ad1da5e21be17496edc6876d92e15c989
git diff ff835b0b f0093316          ->  empty, zero files
tree(ae4b60e1)  ==  tree(b03a09ac)  ==  4c194f4efb89f932418f3d8408e267f87063b8d0
```

`ff835b0b` is the base of the integration branch. `f0093316` is `origin/main`. Their trees are
byte-identical, whole-tree — not merely "the nine touched files agree". The only commit between
the merge base and PR 907, `ae4b60e1` (#904), is an **empty commit**, which is why the two shas
diverge while the content does not.

That single fact settles the entire merge, and it converts a merge review into an equality check.

## Why every conflict resolves to OURS by construction

Relative to the merge base `b03a09ac`:

- **theirs** (`f0093316`) makes change set *X* — the ten lead commits.
- **ours** (`d2989488`) makes *X* + *Y*, because `d2989488` descends from `ff835b0b`, whose tree
  is *X*.

So "theirs" is a **content-ancestor of ours**. There is nothing upstream can contribute that the
branch does not already have, and every hunk resolves to ours without needing a judgement call.

This yields a falsifiable prediction, which is the useful part:

> **If theirs is a content-ancestor of ours, the merge tree must equal `tree(d2989488)` exactly.**

It does — `2fe33ba237a9ce994664e10183512485bce9a7fb` on both sides, `git diff d2989488 HEAD`
empty. One check subsumes every per-hunk argument, and unlike a hunk-by-hunk review it cannot be
satisfied by a resolution that is *mostly* right.

## What actually happened to the four intersecting files

| file | git said | result |
|---|---|---|
| `pkg/hub/httpdispatcher.go` | CONFLICT | ours |
| `pkg/hub/handlers_agent_create_helpers.go` | CONFLICT | ours |
| `pkg/hub/handlers_agents_core.go` | auto-merged clean | byte-identical to ours |
| `pkg/hub/server.go` | auto-merged clean | byte-identical to ours |

### The two clean auto-merges did nothing, and the benign reason is measurable

The handoff flagged these as the dangerous case — "a conflict is loud; a clean auto-merge across
a semantic overlap is silent." They are clean for the benign reason instead: **PR 907's version
of both files is byte-identical to `ff835b0b`'s**, so both sides of the merge made the *same*
change from the base and git had nothing to reconcile. Ours then adds the later Phase 8/9/10 work
on top.

```
                                    base->theirs   base->ff835b0b   ff835b0b->d2989488
handlers_agents_core.go             +20 -1         +20 -1           +95 -13
server.go                           +22 -3         +22 -3           +110
```

Columns one and two are equal because they are the same blob. Column three is the only new
content, and it is ours.

Verified positively rather than by absence of a conflict: **every line PR 907 adds to those two
files is present verbatim at the merge head — 0 lost of 20 and 0 lost of 22.**

### `httpdispatcher.go` — the conflict that matters, and it is a subsumption

PR 907's *entire* contribution to this file is **five literal-to-constant substitutions** inside
the inline per-scope blocks:

```
- store.EnvVarFilter{Scope: "project", ScopeID: agent.ProjectID}
+ store.EnvVarFilter{Scope: store.ScopeProject, ScopeID: agent.ProjectID}
```

…and the same for `"user"` and `"runtime_broker"`. Phase 9 (`ce23801a`) **deleted those blocks
entirely**, replacing them with `envScopePrecedence` plus `envScopesInPrecedenceOrder` /
`envScopeID` / `envScopeSourceLabel`.

So the merge presents a hunk where **the 907 side has content and our side is empty**, which
reads as "upstream added code, we deleted it" and invites keeping theirs. Keeping theirs would
restore broker-highest ordering, defeat the 4-B(iii) demotion, and leave Gap 4's three helper
functions in the file **unused** — with their unit tests still green, because those tests call
the helpers directly.

Taking ours does not drop PR 907's intent; it generalises it. 907 wanted typed scope constants
instead of string literals. At the merge head:

```
store.Scope{Hub,Project,User,RuntimeBroker} in httpdispatcher.go   17 occurrences
EnvVarFilter{Scope: "<literal>"}                                    0   (5 at b03a09ac)
```

The typed constants now live in the ordering list itself, which every consumer derives from.

The second hunk is the same shape: 907 hard-codes `sources[v.Key] = "project"` / `"user"`;
`buildEnvSources` derives the label from `envScopeSourceLabel(filter.Scope)`, which returns
`"hub"`, `"project"`, `"user"` — the same strings 907 uses — plus `"broker"` for
`store.ScopeRuntimeBroker`, a label 907 never reported at all.

### `handlers_agent_create_helpers.go` — strict superset in all four hunks

Ours adds `hcFromHubDefault` (the G2 hub-operational-defaults provenance) beside
`hcFromProjectAnnotation`. Of the 41 lines PR 907 adds to this file, 39 survive verbatim; the
two that do not are the two our side **widens**:

```
907:  if hcFromProjectAnnotation {                    ours: if hcFromProjectAnnotation || hcFromHubDefault {
907:  "from_project_annotation", hcFromProjectAnnotation)
ours: "from_project_annotation", hcFromProjectAnnotation,
      "from_hub_default", hcFromHubDefault)
```

## No upstream test contradicts the 4-B(iii) demotion

The three test files PR 907 adds or changes contain **zero** occurrences of `runtime_broker`,
`ScopeRuntimeBroker` or `"broker"`, case-insensitive. Positive control: the same search returns
103 in `httpdispatcher.go` at the same rev, so the search ran.

Structurally it could not have been otherwise: those three files are already in the branch
byte-identical, and phases 9 and 10 were built on top of them. A contradiction would have been
red at `d2989488` before this merge existed.

## Two instrument corrections, both of which fail toward false alarm

**1. The mandated Gap-4 predicate reports a regression on a branch where Gap 4 is intact.**

```
git grep -nE 'ListEnvVars\(ctx, store\.EnvVarFilter\{Scope' <rev> -- pkg/hub/httpdispatcher.go
```

Circulated ground truth was `ce23801a` 0 · `f0093316` 3 · `b03a09ac` 7. Measured, with the
matches printed rather than counted:

```
b03a09ac 7 · ce23801a 0 · f0093316 7 · d2989488 1 · 66519575 1
```

- `f0093316` is **7, not 3** — the regex also matches the two `ScopeHub` calls and the two in
  `buildEnvSources`. The 3 appears to have been read off the 907 diffstat, which touches three
  lines in `resolveEnvFromStorage`, rather than measured at the rev.
- The head value is **1, not 0**, and the single match is
  `httpdispatcher.go:1308  d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: scope})` — where
  `scope` is the **loop variable** in `WarnOutrankedBrokerEnvKeys`, iterating a list derived from
  `envScopePrecedence`. Gap 4 is intact and the predicate cannot see it, because it matches
  `Scope:` followed by *anything*. Its ground-truth 0 was taken at `ce23801a`, two commits before
  Phase 10c (`14883cbf`) added that call.

The corrected instrument is the `Scope:`-qualified form, which distinguishes a **hard-coded**
operand from a **loop-driven** one — the actual question:

```
git grep -nE 'Scope:[[:space:]]*(store\.ScopeX|"x")' <rev> -- pkg/hub/httpdispatcher.go
  66519575   hub 0 · project 0 · user 0 · runtime_broker 0     <- no hard-coded operands remain
  f0093316   hub 2 · project 2 · user 2 · runtime_broker 1     <- instrument demonstrably alive
  b03a09ac   EnvVarFilter{Scope: "<literal>"} = 5
  negative control (impossible value): rc=1 in both
```

The general form: **a predicate is only valid at the rev its ground truth was taken at.**
`ce23801a` stopped being the tip two commits earlier, and the predicate went stale with it.

**2. `golangci-lint` on changed *files* emits 50 phantom `undefined:` errors.**

The standing instruction is to lint your changed files. Doing so on these four produced 50
`typecheck` failures — `undefined: AuthzService`, `undefined: EventPublisher`, and so on — every
one of them a symbol defined in *another file of the same package*. Linting file arguments
type-checks only those files.

```
golangci-lint run <the four files>   ->  50 issues, all typecheck  (rc nonzero)
golangci-lint run ./pkg/hub/...      ->  0 issues                  (rc 0)
```

`go build ./...` and `go vet` are both clean. **Lint at package scope, not file scope**, or the
recipe manufactures failures that point at the branch.

## Gates

| # | gate | set it ranged over | result |
|---|---|---|---|
| 1 | `go build ./...` | module root | PASS, stderr 0 bytes warm |
| 2 | `go test -count=1` | 10 packages from `go list`, `ok` line required per package | 10/10 ok, no FAIL |
| 3 | PR 907's test files | 66 funcs derived from the 3 files; `-list` guard 66==66 | 66 RUN / 66 PASS / 0 FAIL |
| 4 | AC6 | blob `94af67d8` at 5 revs incl. head; live control differs | PASS |
| 5 | Criterion 17 | `else if profileName != ""` | 0 at head, 1 at `f0093316` |
| 6 | Gap 4 signature | inline `d.store.ListEnvVars` / `envScopePrecedence` | 3 / 13, matching `d2989488` |
| 7 | production path, `-race` | 7 tests calling `resolveEnvFromStorage`/`buildEnvSources` | 7/7, 0 data races |
| 8 | six-pair invariant | all six named pairs asserted **present**, not just green | 6/6 PASS |

Gate 7 exists because of a specific warning worth recording: **Gap 4's helper tests call the
helpers directly, so a merge that orphans the helpers leaves their unit tests green while
production no longer calls them.** Only tests routed through `resolveEnvFromStorage` discriminate.

Gate 8 checks the six scope pairs *by name and for presence*, because an absent subtest is not a
green one. `EnvSources_AgreesWithResolver` is deliberately **excluded** from the invariant: if an
inline block were restored in both functions it would agree with itself and go green while the
code was wrong.

## An operational note that cost a real scare

The gate 7 `-race` run first produced this:

```
top-level RUN 0 · top-level PASS 0 · --- FAIL 0 · no ok line · rc nonzero
```

Zero failures and zero tests, which a count-reading gate scores as clean and a human reads as a
broken branch. The cause was in the file, not the counters:

```
link: mapping output file failed: no space left on device
FAIL github.com/GoogleCloudPlatform/scion/pkg/hub [build failed]
```

The disk was 98% full. `go clean -cache` freed 3.1 GB and the gate passed 7/7. **Requiring the
`ok` line per package is what catches this; a FAIL count of zero does not.**
