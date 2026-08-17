# wsguard — a guard against destructive git operations on a shared workspace

```sh
export PATH="$PWD/hack/wsguard/bin:$PATH"   # install
hack/wsguard/selftest.sh --prove-it         # verify (see "Firing the control")
```

## What it protects against

Scion runs agents in three workspace modes. In `shared-plain`, every agent in a
project shares **one** working directory; in `worktree-per-agent`, the working
trees are private but the underlying clone — history, refs, and the single-slot
files in `.git` — is not. Both modes make ordinary git commands destructive to
people who are not running them.

The hazard has two heads, and a guard that covers one is not a guard.

**(a) The destructive command itself.** `checkout`, `switch`, `restore`,
`reset`, `clean -f`, `stash`, `branch -D`. On 2026-08-17 an agent on this
project ran `git checkout --` on a shared working tree and destroyed its own
uncommitted work. It disclosed the mistake itself, and its framing is the design
principle here:

> *"the rule earns its keep by being reported, not by being unbroken."*

**(b) `FETCH_HEAD` and the other single-slot refs are shared globals.** Two
agents fetching concurrently into one clone overwrite each other's `FETCH_HEAD`,
so `git fetch <url> <ref> && git log FETCH_HEAD` can read a ref that a different
agent fetched. This head is the more dangerous of the two, because it is
invisible: it does not error, it returns a confident wrong answer.

## Mechanism: a `PATH` shim, and why not the alternatives

| Candidate | Verdict | Why |
|---|---|---|
| **`PATH` shim named `git`** | **chosen** | The only mechanism that sees the command *before* it runs, for every form of it, including the pathspec forms that touch no ref. Bypassable on purpose (see below). |
| `core.hooksPath` hook set | rejected — **and this rejection is measured, not argued: `hack/wsguard/hook-probe.sh`** | **Git has no `pre-checkout` hook.** `post-checkout` fires after the working tree has already been overwritten. `reference-transaction` can abort a ref update, so it sees branch switching and `reset` — but `git checkout -- <path>`, `git restore <path>` and `git clean -fd` touch no ref at all, and `FETCH_HEAD` is not written through the ref backend either. A hook set would have been a control that passes vacuously against `git checkout -- .`, the exact command that caused the casualty. See **Measuring the hook rejection** below — that paragraph was originally reasoning from git's documented hook set, and is now a measurement. |
| `git config alias` shadow | rejected | Git does not let an alias shadow a built-in subcommand. `alias.checkout` is ignored whenever `git checkout` resolves to the built-in, which is always. The mechanism silently does nothing, which is the worst available failure mode. |
| A CI check | rejected **as the mechanism**, adopted **as the verification** | CI runs after the push. The work this guard protects is uncommitted, so by the time CI could speak, there is nothing left to protect. But CI is exactly the right place to prove the guard still works: `make wsguard` runs the selftest, so a regression in the guard is caught by the same pipeline as any other regression. |
| Documentation alone | rejected | This project has ample evidence that prose rules are not obeyed under pressure — including in the incident above, where the rule existed and was written down. Documentation is shipped alongside the shim, not instead of it. |
| A read-only mount / filesystem ACL | rejected | It would break the legitimate 95% of writes. The problem is not that agents write to the shared tree; it is that a handful of git subcommands write *over other agents*. |

### Measuring the hook rejection

The paragraph above decides the whole design, and in the first version of this
README it was **argued from git's documentation rather than measured** — it was
published as the design's weakest claim and the first thing a reviewer should
attack. `hack/wsguard/hook-probe.sh` now attacks it.

The probe installs a real `core.hooksPath` set in a throwaway repository, with
each hook logging **its argv and its stdin**, and runs both arms. Measured on
git 2.54.0, `/usr/local/bin/git`, `files` ref backend:

| Arm | Command | `reference-transaction` | Reading |
|---|---|---|---|
| 1 — positive control | `git checkout -q -b probe-branch` | **fires**, payload `refs/heads/probe-branch` | the hook set is really installed, and stdin capture really works |
| 2 | `git checkout -- tracked.txt` | **silent** (only `post-index-change`, `post-checkout` — both after the fact) | the command that caused the casualty is invisible to it |
| 3 | `git restore tracked.txt` | **silent** | same |
| 4 | `git clean -fd` | **no hook fires at all** | not merely unabortable — unobservable |
| 5 | `git fetch <donor> main` | fires 3×, **payload empty**, nothing names `FETCH_HEAD` | hazard (b)'s write side is invisible to it |
| 6 — contrast | `git reset -q --hard HEAD~1` | **fires**, payloads name `ORIG_HEAD`, `HEAD`, `refs/heads/main` | a hook set would have covered `reset`, and *only* the ref-moving forms |

#### The aperture is derived from git, not chosen

The first version of this probe installed **nine hook names I picked**. That put
a membership test I authored underneath the headline result: a hook git invokes
that is not on my list produces silence, and silence is the shape of the result
being reported. An aperture chosen for display convenience becomes a membership
test without saying so.

The hook set is now enumerated from git itself:

1. **an over-inclusive candidate corpus** — every maximal `[a-z-]` run in the
   git binary, 6–32 characters, not dash-terminated (`tr` for the extraction,
   bash `case` for the shape, so no regex dialect can drop a name);
2. **an oracle, which is git** — `git hook run <name>` answers `unknown hook
   event` for a name git does not know and `cannot find a hook named` for one it
   does. The oracle is controlled first: it must give *different* answers for
   `pre-commit` and for an invented name, or it discriminates nothing;
3. **a denominator control against an independent source** — the `.sample` hooks
   git ships in its own templates directory. The derived set must be a superset
   of all 14. Asserting merely that it is non-empty would be asserting against
   zero.

Result: **5779 candidates → 24 native hook events**, all 14 shipped samples
present. And the differential, run rather than argued: across all six arms,
**3** distinct hooks fired and **0** of them were outside the original nine. The
narrow aperture happened to be adequate — but it had no way of knowing that and
no way of reporting it, which is the defect, not the count.

`pre-checkout` and `pre-reset` come back `unknown hook event`. The README's
opening sentence, *"git has no `pre-checkout` hook"*, is that line of output.

Arm 6 is why the rejection matters. A hook set is not useless — it is *partial*,
and it would have looked whole. The part it misses is the part that took
`gd-p1-dev`'s work.

**Arm 5 carries a finding of its own, and it is a finding against the probe's
own first version.** `reference-transaction` *does* fire during the fetch, three
times, carrying nothing. A hook author — or a probe author — who keyed on *did
the event fire* rather than *what did it carry* would read that as coverage of
the `FETCH_HEAD` write. It is not. The first version of this probe logged only
argv, reported the claim CONTRADICTED, and was wrong. It is quoted here rather
than quietly fixed, because the differential between the two versions is the
evidence that the second one is measuring the right thing.

The probe has four controls, and they are not all the same kind — enumeration
(the oracle discriminates), denominator (24 ⊇ the 14 shipped samples),
apparatus (arm 1 fires the hook set), plumbing (arm 1's payload names the ref it
created). Beyond those it has the same two defences as the selftest: a **positive control**
(arms 2–5 are all "nothing happened", which is exactly what a hook set that was
never installed produces for free — arm 1 must fire, and its payload must be
captured, or the probe exits `2` and reports nothing) and a **negative control**
(`--prove-it` re-runs the probe with arm 2's expectation deliberately falsified
and requires exit `1`). `make wsguard` runs the `--prove-it` form.

```sh
hack/wsguard/hook-probe.sh --prove-it   # 0 confirmed · 1 contradicted · 2 nothing measured
```

The answer may be backend-dependent; the probe prints `rev-parse
--show-ref-format` in its provenance block so the reading travels with its
conditions.

### The shim is bypassable, and that is the design

`/usr/local/bin/git` still works, and `SCION_WSGUARD_OVERRIDE` is a documented
escape hatch. This is not a sandbox and does not pretend to be. What it removes
is the **accident** — the operator who did not know the tree was shared, or knew
and forgot, and got no warning until the work was gone. What it adds for the
deliberate case is a **record**: the override demands a reason of at least 12
characters and appends it, with the command and the agent name, to
`<root>/.git/wsguard-audit.log`. An override without a reason is refused like
any other destructive command, because an override that does not say why is
worth nothing to whoever reads the log afterwards.

### Scope: an instrument, not a blanket

A guard that refuses everything gets uninstalled within a week, and deserves to
be. This one fires only when **all** of these hold:

1. the workspace mode says the resource is actually shared — rule (a) arms in
   `shared-plain` only; rule (b) arms in `shared-plain` **and**
   `worktree-per-agent`, because the clone is shared in both;
2. the repository the command would act on is inside the guarded root
   (`SCION_WSGUARD_ROOT`, default `/workspace`) — the same command in your own
   clone under `/tmp` is permitted, and the selftest proves it (arm `P7`);
3. the command matches an enumerated rule.

`git clean -n`, `git stash list`, `git branch --list`, `git status`,
`git fetch origin main:refs/wsguard/<agent>/main`, and every read of a ref you
own are all permitted. Anything not enumerated is passed straight through
without even resolving the repository, so the shim costs nothing on the common
path.

## Exit codes

| Code | Meaning |
|---|---|
| `77` | **REFUSED.** Understood, matched a rule, and **not run**. |
| `78` | **CANNOT EVALUATE.** Armed and watching, but the target repository could not be determined, so no reading was taken and the command was **not run**. Fail-closed. |
| other | git's own exit code, passed through untouched — the shim `exec`s it. |

`77` and `78` are distinct on purpose and neither collides with git's own codes
(1, 128, 129). A caller reading only the status must be able to tell "I refused
you" from "I could not tell, so I did not act" from "git ran and said no".
Collapsing those three is how a gate starts reporting clean for the wrong
reason — and it is why the refusal path in the shim is a **captured status
branched on by value**, never `if cmd`: an `if` sees a boolean, so a gate whose
refusal path is an `if` cannot report cannot-evaluate at all, by construction.

The selftest follows the project's check contract instead: `0` evaluated-clean,
`1` evaluated-with-findings, `2` **nothing was tested**.

## Firing the control

A control that has never been fired is not a control.

```sh
hack/wsguard/selftest.sh            # 21 arms, 24 post-conditions
hack/wsguard/selftest.sh --prove-it # ... after first proving the harness can fail
hack/wsguard/hook-probe.sh --prove-it # the hook-mechanism rejection, measured
make wsguard                        # both, from CI
```

The suite builds four throwaway repositories under `mktemp -d` and runs the
guard against real destructive commands. Three things keep it from passing
vacuously:

- **Positive controls on the harness.** "The file was still modified after the
  guard refused" proves nothing unless `git checkout --` would have removed the
  modification. Each hazard is first reproduced with the *real* git and asserted
  to do its damage. If a control does not reproduce, the suite exits `2`.
- **A negative control on the harness.** `--prove-it` runs the whole suite once
  with one expectation deliberately falsified and requires it to exit `1`. A
  falsified run that still passes means the harness measures nothing, and the
  honest answer is `2`, not `0`.
- **Both directions.** Eleven refusal arms, two cannot-evaluate arms, eight
  permit arms — including the same `git checkout --` that is refused in the
  shared repository being permitted in a private one.

### Both sides of a path comparison must come from the same normaliser

The scoping decision is a string comparison between the guarded root and the
repository the command would act on. An earlier version normalised the root only
`if [[ -d "$root" ]]`, which produced two ways for an **armed** guard to fall
through to passthrough while still printing its arming banner: a root with a
trailing slash or a symlinked component never matches a normalised toplevel, and
a root that does not exist matches nothing. Both are now `78`
(cannot-evaluate) or a refusal, asserted by arms `U2-unresolvable-root` and
`N11-root-with-trailing-slash`. A guard must not answer *"not shared, go ahead"*
on the strength of a comparison it could not make.

### Instrument disclosure

Every assertion in the selftest is a bash `[[ "$x" == *literal* ]]` glob
containment test or an integer comparison. **No `grep`, `rg`, `awk` or `sed`
decides any verdict**, so no result depends on which binary `grep` resolves to
or on which regex dialect it defaults to — the failure mode that cost this
project a day, where an ERE pattern run under GNU grep's default BRE reported
`0` for a term that was present. The one thing resolved from the environment,
the real `git`, is resolved explicitly and printed with its path and version in
the run's provenance block.

## Environment

| Variable | Effect |
|---|---|
| `SCION_WORKSPACE_MODE` | Arms the guard: `shared-plain` arms both rules, `worktree-per-agent` arms rule (b) only. |
| `SCION_WSGUARD=off` | Disables the guard entirely, announced on stderr. |
| `SCION_WSGUARD_FORCE=1` | Arms both rules regardless of workspace mode. |
| `SCION_WSGUARD_ROOT` | Guarded root. Default `/workspace`. |
| `SCION_WSGUARD_AUDIT` | Override audit log. Default `<root>/.git/wsguard-audit.log`. |
| `SCION_WSGUARD_OVERRIDE` | A reason, ≥12 characters. Permits one command and records it. |

## What this does NOT cover

Stated plainly, because a guard whose limits are not written down gets read as
covering more than it does.

- **`merge`, `rebase`, `cherry-pick`, `revert`, `am`, `apply`** are not
  enumerated. They rewrite a shared working tree just as thoroughly as `reset`
  does. They are omitted because they are also the normal way work gets
  integrated, and a rule that fires on them would be suppressed within a week —
  the failure mode that matters more than the coverage gap. If the fleet decides
  those belong in the shared-tree prohibition, adding them is one `case` arm
  each plus one arm in the selftest.
- **Anything invoked as `/usr/local/bin/git`,** by absolute path, or by a
  process that does not inherit the shim's `PATH`. Deliberate; see above.
- **Non-git destruction** — `rm -rf`, an editor writing over a file another
  agent is holding, a build that cleans its output directory.
- **Installation.** This ships the guard and the proof that it works; it does
  not yet put `hack/wsguard/bin` on `PATH` inside agent containers. That belongs
  in the container provisioning path (`pkg/config/embeds/`), which is shared
  infrastructure and needs to be assigned deliberately. Until then the guard
  protects whoever exports the `PATH`, and `make wsguard` keeps it honest.
