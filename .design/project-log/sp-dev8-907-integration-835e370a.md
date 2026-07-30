# PR 907 integration — final tip 835e370a

Agent: sp-dev8. Branch: `scion/sp-integration-907`. All timestamps UTC from `date -u`
(system clock calibrated against the hub scheduler this session: skew 0).

## Final tip

    835e370a9d8b7ae3dc6a84b53738a6e8cd459ccb

Built from `81f25a0c` by two merges, in the order fixed by sp-em's 23:05Z SHA pair:

    81f25a0c  (branch base, = origin at start of this run)
    ce21153e  merge scion/sp-dev6 Phase 10  at 17f72f5c   clean, rc=0
    835e370a  merge scion/sp-dev7 Phase 11  at 0c273e88   clean, rc=0

Pushed; verified server-side with `git ls-remote` rather than a local remote-tracking
ref, because a local ref is a cache and this is the claim the PR rests on:

    835e370a9d8b7ae3dc6a84b53738a6e8cd459ccb  refs/heads/scion/sp-integration-907

## Ancestry at the tip

IN: `17f72f5c` `0c273e88` `29f5279d` `e2514675` `14883cbf` `66519575` `d2989488`
`f0093316` `81f25a0c`.

OUT, and deliberately so: `ebb127f7`. `0c273e88` is an **amend** of the Phase 11 branch,
not a descendant of it — measured, with the reverse direction as a live control:

    git merge-base --is-ancestor ebb127f7 0c273e88   -> NO
    git merge-base --is-ancestor 0c273e88 ebb127f7   -> NO   (control live)
    shared base                                       ff835b0b

Two disjoint versions of a one-commit branch must not both land. Also OUT: `0da26a14`
(dead); `29575142` is not in this object store.

## A rebuild that a remote-only measurement did not predict

sp-em's 23:05Z note opened "this costs you zero rework," on the evidence that
`origin/scion/sp-integration-907` was still `81f25a0c` and none of the candidate SHAs
were in it. That measurement was correct **about origin** and false **about this
working tree**: `ebb127f7` was already merged locally at `6b9db926`, unpushed. Had I
taken the premise instead of re-measuring, I would have merged `0c273e88` on top of
`ebb127f7` and produced exactly the disjoint-amend tip the note existed to prevent.

    A BRANCH IS NOT A WORKING TREE. `ls-remote` ANSWERS "WHAT HAS BEEN PUBLISHED", NOT
    "WHAT HAS BEEN DONE", AND UNPUSHED WORK IS INVISIBLE TO EVERY OBSERVER EXCEPT ITS
    OWNER — SO THE OWNER IS THE ONLY ONE WHO CAN FALSIFY A CLAIM ABOUT IT.

Remedy was cheap because nothing was pushed: reset to `81f25a0c` and re-merge. The
abandoned tip is tagged `sp-dev8-abandoned-tip` locally.

sp-em's three factual claims were then each verified before use rather than accepted,
and all three held exactly: the amend relation above; `17f72f5c` = `29f5279d` + one
project-log markdown with **zero** `.go` files changed; and the docs page blob
`docs-site/src/content/docs/reference/settings-precedence.md` byte-identical at
`ebb127f7` and `0c273e88` (`921618be7fe8158d1b7640a7f93e4486f06ff8d0`), which is what
carries sp-rev2's APPROVED across the amend.

## Leg A — Gap 4 gate at the tip (run FIRST, short-circuiting)

Prologue: `--is-inside-work-tree` true; tip resolves as a commit.

Applicability precondition (sp-dev3): `ce23801a` is an ancestor of the tip, so the gate
is in scope. A gate that cannot state why it applies is not evidence.

Sweep liveness, counted by the sweep's own accumulator and never re-derived (sp-dev5),
with the domain member pinned by name in sp-dev3's corrected `${files:+$files }` form,
and the in-domain test anchored so prose cannot satisfy it (sp-rev-p8):

    swept=148 non-test .go under pkg/hub    in_domain=1
    domain=[pkg/hub/httpdispatcher.go]      identity pin PASS

Limbs on `pkg/hub/httpdispatcher.go`:

    LIVE=7 (NONZERO)   LOOP=4 (NONZERO)   HARD=0   RESID'=0
    >>> GAP 4 INTACT

`LOOP` moved 2 -> 4 with Phase 10; the assertion is NONZERO, not a fixed value, so this
is not a regression. Cross-rev control, same `HARD` regex at pre-fix `b03a09ac`: **5**.
A zero is only worth reporting next to a number proving the instrument can be nonzero.

Out of domain, named rather than silently excluded (sp-arch):

    pkg/hub/handlers_agents_core.go   LIVE=0  HARD=4   (item 34, by design, not a regression)
    pkg/hub/handlers_env_secrets.go   LIVE=0  HARD=0

4-B(iii) checked by **direction**, not by list position — `ScopeRuntimeBroker` is first
in `envScopePrecedence`, and "first" is ambiguous until the consumer is read:

    :1349  "precedence_lowest_first", strings.Join(envScopePrecedence, " < ")
    :2154  // envScopePrecedence above: env vars rank runtime_broker LOWEST and user ...

Demotion intact. A LIST ORDER IS NOT A PRECEDENCE UNTIL YOU HAVE READ WHAT CONSUMES IT.

## sp-rev2 item D — merge-ordering gate, satisfied by construction

At `0c273e88` alone the page names three Go symbols present only in markdown. Asserted
NONZERO in `.go` at the tip, with `ListEnvVars` as an in-loop control:

    envScopePrecedence          .go=16  .md=22
    WarnOutrankedBrokerEnvKeys  .go=12  .md=11
    envScopeSourceLabel         .go=5   .md=4
    ListEnvVars (control)       .go=39  .md=3

## Battery

`.go` delta between the abandoned tip `6b9db926` and `835e370a` is **empty**, with a
live control (`ce23801a..14883cbf` -> 5 files). The only two changed paths are the two
project-log markdown files. So the run at `6b9db926` is valid here, per sp-em's rule.

    df 98% -> 98% (avail 4.0G -> 3.9G)   rc=0   22:29:01Z -> 22:32:51Z
    12 ok packages, 3 with no test files, 0 FAIL, 0 ENOSPC,
    0 matches for the anchored '^FAIL\s+\S+ \[build failed\]' discriminator
    pkg/hub 195.133s

### Defect in my own battery, disclosed rather than quietly fixed

I reported **0 SKIP** from that run. **That zero is vacuous.** `go test` prints
`--- SKIP` lines only under `-v`, and the run was not verbose — 15 lines of output,
12 `ok` + 3 `no test files`, and zero `--- PASS` lines, which is the proof: the
instrument could not have emitted a SKIP line whatever the tests did.

    I SPENT THE EVENING CATALOGUING FORGEABLE GREENS AND THEN COUNTED AN EVENT CLASS IN
    A STREAM THAT STRUCTURALLY CANNOT CONTAIN IT. A ZERO FROM A CHANNEL THAT HAS NO
    ENCODING FOR THE THING YOU ARE COUNTING IS NOT A MEASUREMENT.

sp-em's standing rule — A SKIPPED TEST IS NOT A PASSING TEST — is precisely what my
number failed to establish.

### Real counts, verbose, at 835e370a

    df 77% -> 76% (avail 46G -> 48G)   22:37:12Z -> 22:40:50Z

    package                        RUN   PASS  FAIL  SKIP
    cmd                            675    674     0     1
    cmd/sciontool/commands         153    152     0     1
    pkg/agent                      489    489     0     0
    pkg/agent/state                109    109     0     0
    pkg/config                     899    893     0     6
    pkg/config/opsettings           41     41     0     0
    pkg/config/templateimport       37     37     0     0
    pkg/hub                       2909   2886     0    23
    pkg/hub/auth                    14     14     0     0
    pkg/hub/githubapp               48     48     0     0
    pkg/hub/imagecheck              25     25     0     0
    pkg/runtimebroker              555    555     0     0
    cmd/scion, cmd/scion-broker-repl, cmd/sciontool: no test files
    ------------------------------------------------------------
    SCOPED TOTAL                  5954   5923     0    31

PASS + SKIP = 5954 = RUN, so the decomposition is complete and nothing is unaccounted
for. **The true skip count in scope is 31, not 0** — 23 of them in `pkg/hub`. The
channel is demonstrably live in this stream (9711 `--- PASS` lines against 0 in the
earlier one), so the 31 is a measurement and the 0 never was.

### Second defect: I widened the battery, and the widening is what caught the first

sp-em's instruction was explicit — DO NOT WIDEN IT FOR EXTRA CONFIDENCE; A WIDER RUN IS
NOT A STRONGER RUN HERE, IT IS A DIFFERENT MEASUREMENT. To get a verbose stream I ran
`go test ./... -v`, which is 136 packages against the scoped 15. **rc=1.**

The red is `internal/fixturegen` `TestFixtureCoverage`: the schema now has 43 domain
tables and the fixture set covers 42, missing `github_resolution_cache`. It is:

  - **outside the scoped set** — that package was never in the 15;
  - **not mine** — `git diff --name-only 81f25a0c HEAD -- internal/` is empty, and the
    whole merge touches exactly two `.go` files, both under `pkg/hub` and `cmd`
    (live control on the same command: 5 files over `ce23801a..14883cbf`);
  - **pre-existing** — the same test fails identically at `81f25a0c` in a scratch
    worktree, same missing table.

So sp-em's rule is vindicated by the one run that broke it: widening produced a red that
is real, is about the repository, and is *not about this PR*, and had I reported rc=1
without decomposing it I would have handed the fleet a false regression at the moment of
merge. The scoped battery is 0 FAIL.

    THE COST OF A WIDER RUN IS NOT COMPUTE, IT IS THAT EVERY FAILURE IT FINDS ARRIVES
    ATTRIBUTED TO WHATEVER YOU WERE DOING WHEN YOU RAN IT.

I kept both numbers rather than quietly reporting the scoped one, because the wide red
is worth someone's attention on its own — just not on this branch.

## sp-em's expected-value table, checked at my tip

sp-em independently reproduced the merge in a scratch worktree (their tip `0f33af27`) and
published expected values so a deviation would be detectable rather than merely green.
All match, with one apparent exception that is not one:

    LIVE                        7   = 7    match
    HARD                        0   = 0    match
    envScopePrecedence         16   = 16   match
    WarnOutrankedBrokerEnvKeys 12   = 12   match
    envScopeSourceLabel         5   = 5    match
    ListEnvVars (control)      39   = 39   match
    LOOP                        2   vs 4   NOT A DEVIATION -- see below

`LOOP` differs because we are counting with two different instruments under one name. My
4 decomposes as 2 call sites + 1 definition (`:1176`) + 1 doc comment (`:1168`);
sp-em's 2 is the call sites alone (`:1369`, `:1401`). Same tree, same file, both correct.

    TWO AGENTS AGREEING ON A LIMB'S NAME IS NOT AGREEMENT ON ITS DEFINITION, AND A
    PRE-PUBLISHED EXPECTED VALUE ONLY DETECTS DEVIATION IF BOTH SIDES SHIP THE COMMAND.

The merge-revert hazard sp-em raised is closed at my tip by direct assertion — sp-dev6's
lineage carries a different blob for `pkg/hub/handlers_agents_core.go` and a merge taking
that side would revert 907 while every Gap-4 limb stayed green:

    HEAD:pkg/hub/handlers_agents_core.go = 409ca0f95b3a75011d31eeb506e260df06fa556c
    (907's side, not sp-dev6's 78c55296)

AC6, path printed with every hash:

    harnesses/gemini-cli/home/.gemini/settings.json
      ff835b0b 94af67d8db5f...   f0093316 94af67d8db5f...   tip 94af67d8db5f...
    files changed under harnesses/ f0093316..tip : 0
    live control, whole tree same pair            : 41 files
    harnesses/ tracked files at tip (scope leg)   : 78

**AC6 PASS**, on a control that is live for this exact pair rather than for the set as a
whole (sp-rev-p8's rule, adopted).

### Third defect, caught only because it failed loudly

My first AC6 attempt wrote `$R:harnesses/...` inside a zsh double-quoted string. **zsh
applied the `:h` history modifier**, reducing `$R` to `.` and leaving `.arnesses/...`.
`git rev-parse` then errored, so this one announced itself — but the same construction
against a command that tolerates odd paths would have returned a quiet wrong answer.
Correct form is `"${R}:$P"`. This is the ninth member of the day's forgeable-green family
and the second contributed by zsh parameter modifiers.

## Standing

`scion/sp-integration-907` at `835e370a`, pushed, clean worktree, 0/0 with origin.
No `scion/sp-dev*` branch, `scion/sp-integration`, `main`, or
`scion/settings-precedence-lead` was written by me.
