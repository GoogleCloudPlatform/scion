# scripts/single-node-vm/tests

A stub-`gcloud` test harness for `deploy.sh`. It never contacts GCP: it
puts a fake `gcloud` (`tests/lib/gcloud`) first on `PATH`, then drives
`deploy.sh` as a real subprocess (or, for a function-level test, sources a
helper directly) and asserts on exactly what it asked `gcloud` to do.

This harness is shared across several PRs, each adding stub cases and
fixtures for the feature it covers (NAT reuse, IAP firewall scoping, the
hybrid tier, ...) on top of this common core. Keep additions here generic
to `deploy.sh`'s own behavior; put feature-specific stub cases and test
files in the PR that adds the feature.

## Requirements

- **bash >= 4** (the runner uses `mapfile` and associative arrays). This
  is a dev-only test runner; `deploy.sh` itself targets bash 3.2+, and
  this suite is not the vehicle for verifying that support.
- **python3** — the stub's own JSON fixtures (firewall rules, service
  accounts, ...) are built and parsed with python3, not jq.
- **jq** — `deploy.sh` checks for jq as a hard Phase 1 prerequisite (it's
  required to safely detect an existing Cloud NAT before creating any
  resources; see the Cloud NAT reuse check), and fails immediately if it's
  missing -- before Phase 1 even gets to reading `--version`. It's also
  used, when reached, to parse a GitHub Releases response for the default
  `VERSION`, but every test in this suite passes `--version` and so never
  exercises that second path. Install jq to run this suite at all; a test
  that specifically wants to simulate jq being absent (see
  `test_nat_reuse.sh`'s `_dir_without_jq`) does so for a single
  subprocess invocation, not for the suite as a whole.

## Running the suite

```
./run.sh
```

Runs every `test_*.sh` file in this directory and prints a final
pass/fail count. Exits non-zero if any assertion failed.

## How it works

- `run.sh` puts `tests/lib` at the front of `PATH` (so the stub `gcloud`
  shadows any real one) and sources `tests/lib/harness.sh` once, for
  shared fixture/assertion helpers.
- **Every `tests/test_*.sh` file is sourced into its own subshell, not
  into `run.sh`'s own shell, and each `test_*` function it defines then
  runs in a further, per-test subshell of that.** A file-level global or
  helper — `HUB`, `DEPLOY_SH`, `run_deploy_create`, or anything else a
  test file defines at source time — is therefore **private to the file
  that defines it**. Two files are free to reuse the same name (this
  harness's own `test_deploy_base.sh` and a feature PR's test file both
  commonly define `HUB` and `INSTANCE_NAME`, for instance) without one
  clobbering the other, and files run in no particular dependency order
  beyond the sort order used to pick which one's `test_*` functions
  appear first in the output. What subshell isolation does *not* give
  you: two `test_*` functions with the *identical* name across two files
  is still a mistake, not a feature — `run.sh` detects this (a shared,
  on-disk list of every test name claimed so far) and fails the later
  one loudly rather than silently dropping it. Shared state that isn't a
  shell variable or function — files under `$GCLOUD_STUB_STATE_DIR`, the
  real `deploy.sh`/stub subprocesses a test starts — is not isolated by
  this and is each test's own responsibility, via `fresh_gcloud_state`
  and each test running in its own subshell.
- **Corollary: a test file may use only `lib/harness.sh`'s shared helpers
  plus what it defines itself.** It cannot call a function or read a
  variable another `tests/test_*.sh` file happens to define — per-file
  isolation means that dependency is invisible at the point the other
  file needs it, not just discouraged. If two or more test files
  genuinely need the same helper or constant, add it to
  `lib/harness.sh` (see `line_number()` there for an example of exactly
  this: a helper more than one test file's ordering assertions need,
  moved here instead of staying private to whichever file used it
  first) rather than having one file source or duplicate another's.
- **Before sourcing a file, `run.sh` runs `bash -n` on it and fails the
  whole run loudly if it doesn't parse** (a leftover merge-conflict
  marker after a rebase is the realistic way this happens). Without this
  check, bash abandons a syntactically broken file partway through
  instead of treating it as fatal, so every `test_*` function after the
  bad line would simply never be defined — and the run would still
  report a pass. Do not remove this check when editing `run.sh`.
- `tests/lib/gcloud` is the fake `gcloud`. Every invocation is logged
  (see `gcloud_log`/`gcloud_call_count` in harness.sh) before being
  dispatched on `$1 $2 [$3 [$4]]`. Responses are served from small files
  under `$GCLOUD_STUB_STATE_DIR`, populated by a test's fixture ("seed_*"
  / "set_*" helpers in harness.sh) and inspectable afterwards.
- **Unknown commands fail loudly, on purpose.** Any invocation the stub
  doesn't recognize falls through to `gcloud-stub: unhandled invocation:
  $*` on stderr and `exit 1` — the same as a real `gcloud` call the
  script isn't supposed to make. If a test needs a new command handled,
  add a case for it (see below); do not make the fallback case silently
  succeed, or a real wiring bug (e.g. a typo'd subcommand) will pass
  silently too.
- `test_deploy_base.sh` is the harness's own smoke test: it runs
  `deploy.sh` through a fresh create, a re-run against the state that
  create left behind (proving each resource's own "already exists"
  fixture persists, not just that a test can hand-seed one), and a
  `--delete` against both a clean project and the state a create left
  behind, using only stub cases and fixtures this file provides. It has
  no feature-specific code in it — if it fails, the harness itself is
  broken, independent of any feature built on top of it.

## Adding a stub case and fixture

1. **Find (or add) the dispatch case** in `tests/lib/gcloud`. Cases are
   matched by word count: a `case "${1:-} ${2:-}" in ...` block for
   two-word commands (`services enable`), one for three-word commands
   plus a resource name at `$4` (`compute instances create NAME ...`),
   and one for four-word commands (`compute routers nats create ...`).
   Put a new case in the block matching how many literal words come
   before the resource name/flags.
2. **Log first, dispatch second.** The stub always appends the full
   invocation to `$GCLOUD_STUB_LOG` before dispatching (see the
   `printf '%s\n' "$*" >> "$GCLOUD_STUB_LOG"` near the top) — you don't
   need to log anything yourself inside a case.
3. **Read flags with `get_flag`/`require_flag`.** `get_flag KEY "$@"`
   echoes the value of `--KEY=VALUE` among the args, or fails with no
   output if absent; `require_flag KEY "$@"` does the same but exits 1
   loudly if it's missing or empty (`require_project` is a shorthand for
   `require_flag project`). Use these instead of hand-parsing `$@`.
4. **Store fixture state as plain files** under
   `$GCLOUD_STUB_STATE_DIR`, one file (or JSON file, via `$PYTHON -c
   '...'`) per resource, keyed by name. Add a small `mkdir -p` for any
   new subdirectory both in the stub's own top-of-file `mkdir -p` line
   and in `fresh_gcloud_state` in `tests/lib/harness.sh`, so a test
   doesn't depend on directory-creation order between the two.
5. **Add `seed_*`/`set_*` helpers in `tests/lib/harness.sh`** for
   whatever your case reads: `seed_X` for "a resource already exists with
   this state", `set_X_will_fail` (or `..._error`) for "the next call for
   this resource fails". Follow the naming and docstring style of the
   existing helpers immediately above/below where you add yours.
6. **Default to the realistic "doesn't exist yet" response**, not to
   success or failure — e.g. `compute routers list` returns `[]` (a real,
   empty JSON list) when no fixture has been seeded, never empty stdout
   or `exit 1`, so a caller's own JSON parsing is exercised the same way
   whether or not a test cares about the list's contents.
7. **Write the test** in your own `tests/test_*.sh` file (not
   `test_deploy_base.sh`, which is base-only), following the
   `test_deploy_base.sh` calling pattern for a subprocess-driven test:
   write a config file, run `deploy.sh` (directly for `--delete`, which
   never blocks; via a background process plus a sentinel file for
   create-mode, to stop it before its real SSH-readiness retry loop), and
   assert on `gcloud_log`/`DEPLOY_LOG`/`DEPLOY_RC` with the `assert_*`
   helpers; for an ordering assertion ("X must happen before Y"), use
   `line_number PATTERN LOG` (also in `lib/harness.sh`) to find each
   call's line number and compare them. Remember the isolation rule
   above: if what you need isn't in `lib/harness.sh` and isn't something
   your own file defines, it isn't reachable — add it to
   `lib/harness.sh` instead of relying on another test file happening to
   run first.
8. **Run `./run.sh`** and confirm both your new test and the full suite
   pass, then `shellcheck -x` every changed file from the repo root.
