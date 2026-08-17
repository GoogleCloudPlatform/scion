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
| `core.hooksPath` hook set | rejected | **Git has no `pre-checkout` hook.** `post-checkout` fires after the working tree has already been overwritten. `reference-transaction` can abort a ref update, so it sees branch switching and `reset` — but `git checkout -- <path>`, `git restore <path>` and `git clean -fd` touch no ref at all, and `FETCH_HEAD` is not written through the ref backend either. A hook set would have been a control that passes vacuously against `git checkout -- .`, the exact command that caused the casualty. |
| `git config alias` shadow | rejected | Git does not let an alias shadow a built-in subcommand. `alias.checkout` is ignored whenever `git checkout` resolves to the built-in, which is always. The mechanism silently does nothing, which is the worst available failure mode. |
| A CI check | rejected **as the mechanism**, adopted **as the verification** | CI runs after the push. The work this guard protects is uncommitted, so by the time CI could speak, there is nothing left to protect. But CI is exactly the right place to prove the guard still works: `make wsguard` runs the selftest, so a regression in the guard is caught by the same pipeline as any other regression. |
| Documentation alone | rejected | This project has ample evidence that prose rules are not obeyed under pressure — including in the incident above, where the rule existed and was written down. Documentation is shipped alongside the shim, not instead of it. |
| A read-only mount / filesystem ACL | rejected | It would break the legitimate 95% of writes. The problem is not that agents write to the shared tree; it is that a handful of git subcommands write *over other agents*. |

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
hack/wsguard/selftest.sh            # 19 arms, 20 post-conditions
hack/wsguard/selftest.sh --prove-it # ... after first proving the harness can fail
make wsguard                        # the same, from CI
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
- **Both directions.** Ten refusal arms, one cannot-evaluate arm, eight permit
  arms — including the same `git checkout --` that is refused in the shared
  repository being permitted in a private one.

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
