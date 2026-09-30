# Destroy-protection harness

## `verify-destroy-protection.sh`

With a hub present and protection on, proves that the shared infrastructure
**cannot** be destroyed.

### Run this first

```bash
./verify-destroy-protection.sh --selftest
```

No GCP access, no credentials, no Terraform state. It exercises the classifier
and the snapshot comparison against fixtures, including negative controls.
**If the self-test fails, nothing else the script reports is worth reading.**

```bash
./verify-destroy-protection.sh --run \
  --project P --sql-instance tfha-pg \
  --filestore tfha-nfs --filestore-zone us-central1-a \
  [--tf-dir /path/to/shared/root] [--expect-commit <sha>] \
  [--out destroy-protection-report.md]
```

### Always pass `--expect-commit`

With `--tf-dir`, the harness records the checkout's `HEAD` in the report header,
and with `--expect-commit` it **refuses to run (exit 2)** unless `HEAD` starts
with the SHA you named. A short prefix is accepted; `UNKNOWN`, an empty HEAD and
an empty expectation are all refused.

This is not ceremony. It is here because I once reviewed one commit and ran
`terraform plan` against another, then filed a BLOCKING bug against code that
had already been fixed. Nothing lied to me — my own output printed the wrong
HEAD and I read past it. Reading and running took their inputs from different
places and only one was checked.

**Assert the SHA in the command that runs the test, not in a step next to it.**
A destroy-protection table attributed to the wrong commit is precisely the
confident-wrong-answer this harness exists to prevent, and the harness should
refuse rather than produce one.

### The one thing to understand before reading a result

**A non-zero exit is not a pass.** `gcloud sql instances delete` exits non-zero
when deletion protection refuses it, when you lack the permission, when the
instance does not exist, when the API is disabled, and when a token expired.
All five look identical at the exit-code level, and only the first is a pass.

So the harness classifies the refusal *text*:

| class | meaning | verdict |
|---|---|---|
| `PROTECTED` | the guard refused it, and said so | **PASS** |
| `DELETED` | the delete **succeeded** | **FAIL** (catastrophic) |
| `DENIED` | permission / auth refused it | INCONCLUSIVE |
| `NOTFOUND` | the resource is not there | INCONCLUSIVE |
| `OTHER` | refused for some fourth reason | INCONCLUSIVE |

INCONCLUSIVE scores red. "I could not tell" must never render as green.

Every delete attempt is followed by an **existence check**, and that overrides
the classification: if the resource is gone, the row is `FAIL` regardless of how
convincing the refusal looked. The operation's status is a claim; the resource
still being there is the postcondition.

### Exit codes

| code | meaning |
|---|---|
| 0 | every required row passed |
| 1 | a row failed or was INCONCLUSIVE — read the table |
| 2 | the harness could not run (bad args, missing tool, no credentials) |
| 3 | refused to act for safety |

### All fixtures are now captured from live refusals

As of 2026-09-23 there are no reconstructed fixtures left: `sql` and `filestore`
were captured from real refusals by `tfha-pg` and `tfha-nfs`, and `--selftest`
now prints *"All PROTECTED fixtures are captured from live refusals"* instead of
a count. That line is **derived** by grepping for the `UNCONFIRMED ` tag on the
fixture calls, so it cannot drift: add a guessed fixture with the tag and the
warning comes back by itself.

**The captured wording was not what I had reconstructed, and one difference was
load-bearing:**

| | reconstructed guess | what the API actually says |
|---|---|---|
| sql | "Cannot delete instance because deletion protection is enabled" | "**The instance is protected.** Please disable the deletion protection..." |
| filestore | "Instance cannot be deleted because deletion protection is enabled" | "... is **protected from deletion**: `<deletion_protection_reason>`" |

Filestore never says "deletion protection" at all. The old pattern matched it
**only because our own `deletion_protection_reason` string happens to contain the
literal `deletion_protection`** — so the test was keyed on text the operator
supplies, not on text the API guarantees. Reword that reason in
`modules/filestore` and a perfectly working guard would have scored
`INCONCLUSIVE`.

There is now a selftest row carrying the same refusal **with our reason string
stripped out**, so the classifier is held to the API's own words. Do not delete
it: it is the only row that fails if someone re-narrows the pattern.

### Row 4 is a stated gap and must never be dropped

`google_container_cluster.deletion_protection` is a **Terraform-side guard
only**. GKE container v1 has no deletion-protection field at all — as of
2026-09-23, zero of the `Cluster` properties in the container v1 discovery
document match `delet|protect`, checked with a positive control proving the
filter can match. An out-of-band `gcloud container clusters delete` succeeds.

Three green rows with no fourth row read as "the estate is protected", which is
false. Every stated-gap row carries an `@unconditional-row` marker and the
self-test asserts the **count** of them, so a gap cannot quietly disappear.

The expected count lives in `want_rows` in the script, deliberately not repeated
here: if you add a gap row, you change one number in one place. A README that
restates a number the code owns is a second source of truth that nobody
re-checks.

The second stated gap: a `-target` apply can flip `deletion_protection` to
false, because `prevent_destroy` keys on *destroy*, not on in-place update, and
`-target` skips preconditions. That path is **not** tested live — doing so
would mean actually turning off protection on live shared resources, which is
exactly what this residual risk warns against.

### Attempt 4 is two-stage, and the gate is deliberate

- **4a** `terraform plan -destroy` — read-only, must be refused.
- **4b** `terraform destroy` — only with `--with-real-destroy`, and **only if 4a
  was refused.**

If 4a does not show a refusal, 4b is skipped and scored INCONCLUSIVE rather than
run. A test that inflicts the failure it is checking for is not a test.

Both stages are bracketed by a whole-estate snapshot (`terraform state list`
plus a 13-way gcloud sweep for `${NAME_PREFIX}*`), because attempt 4's real
failure mode is a **partial** destroy: protected resources survive, the plumbing
around them does not, and no per-resource check can see it.

Failed sweeps write an explicit `ERROR:` line and bump a counter. Two blind
spots would otherwise diff as identical — a silent pass built out of seeing
nothing twice.

### Four traps that have already bitten, kept as selftest rows

All four were real defects in this file or in my own operating, not
hypotheticals. They are selftest
rows rather than comments because **comments did not stop them.**

1. **No churn fields in an equality postcondition.** The snapshot headers used
   to contain the words `(before)` and `(after)`, so the two files being diffed
   against each other could never be equal, and the must-pass "Estate unchanged"
   row would have failed on every run — reporting `THE ESTATE CHANGED` on an
   untouched estate. Same defect as diffing an IAM policy `etag`. Never compare
   `etag`, `version`, `updateTime` or `generation`: that asks "did anything
   happen" when the question is "did anything remain".

2. **A test that builds both sides with the same helper is blind to that
   helper.** Reintroducing the label above leaves all four comparison tests
   green, because it appears identically on both sides and cancels out. Only the
   fifth, content-inspecting test catches it. Do not delete that fifth test as
   duplication.

3. **An absence assertion over a medium that might be missing is not a check.**
   Two self-lints read `$0`. Piped into a shell, `$0` is not this file and both
   greps return nothing — from which the count lint fails closed and the
   absence lint printed `ok`. "I looked and found nothing" and "I could not
   look" rendered identically. There is now a positive control ahead of both:
   `$0` must be readable and contain a known sentinel, and if it is not, the
   absence lint reports `SKIPPED (source unreadable), not passed`. **Skipped and
   passed must never render the same.**

4. **A "fails closed" test is also satisfied by an instrument that can never
   find anything.** `commit_matches` refusing `UNKNOWN` proves nothing on its
   own if `repo_head` returns `UNKNOWN` for every path, including real repos —
   the run would refuse for the wrong reason. So there is a positive control:
   `repo_head` must read a real 40-hex HEAD out of a throwaway repo, and if
   `git` is unavailable that row is **SKIPPED and counted as a failure**, not
   passed. Same rule as trap 3, one layer down.

Plus a self-lint for the `printf | grep -q` SIGPIPE race under `pipefail`, which
has caught live code in this file.

### Running it in CI

Invoke it **by path**, never piped into a shell:

```bash
deploy/terraform/tools/verify-destroy-protection.sh --selftest
```

It fails closed either way, but by-path is the only invocation in which the
source lints are meaningful rather than merely red.

**Treat exit 2 as a failure, not as "skipped".** A step that special-cases a
tooling problem as neutral reintroduces trap 3 at the pipeline level: the run
that could not look would report the same green as the run that looked and found
nothing wrong.

### Safety

`--run` deletes nothing that is not already protected, and 4b is opt-in and
gated. The GKE cluster name, if given, is recorded in the report and is **never
passed to a delete command.**
