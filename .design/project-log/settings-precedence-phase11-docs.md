# Settings Precedence — Phase 11: reference doc + release notes (sp-dev7)

Branch: `scion/sp-dev7`
Baseline: `ff835b0b`
Date: 2026-07-29

Documentation only. **No Go code changed in this commit.**

## What landed

| File | Change |
| --- | --- |
| `docs-site/src/content/docs/reference/settings-precedence.md` | New. The reference page. |
| `docs-site/src/content/docs/reference/agent-config.md` | Cross-link + a "this is a partial ordering" note on the existing project-settings precedence section. |
| `docs-site/src/content/docs/release-notes.md` | New `## Jul 29, 2026` section with `### ⚠️ Breaking Changes`. |

`CHANGELOG.md` is deliberately **untouched** — see "CHANGELOG location" below.

The Technical Reference sidebar is `autogenerate: { directory: 'reference' }`, so the new page
needs no `astro.config.mjs` change.

## The organising decision: two tables, not one stack

The page is built around the fact that Scion has **two independent precedence systems** that rank
the same-named sources differently:

- **System A — environment variables:** `envScopePrecedence` and the config seeding in
  `pkg/hub/httpdispatcher.go`.
- **System B — `ScionConfig` limits and resources:** the ladder in `ResolveHarnessConfig` and
  `pkg/agent/provision.go`.

The reason they differ is structural and is stated on the page as its own section:

> **Env COLLAPSES; Resources INTERLEAVES.** The settings-file env sources merge to a single map
> inside `ResolveHarnessConfig`, which enters the outer chain as one only-if-absent tier. The same
> broker file contributes `resources` at three separate outer ranks with the hub tier sandwiched
> between them. **A single-tier contribution can have a single rank; an interleaved one cannot.**

Consequence, stated explicitly on the page so a reader cannot "repair" it by flipping an
inequality: `runtime_broker < hub` is **true** on env, **true** on the three scalar limits, and
**has no truth value** on `resources`.

Merging the two tables later is a cheap edit; splitting a merged one is not. That asymmetry is why
the split was decided before the first row was written.

## Citations

Every load-bearing claim on the page cites by **symbol** or by **comment text**, not by line
number, because these regions are edited constantly and a drifted line number always resolves — to
real code that is confidently the wrong thing. The page says so in its own method section.

Line-pinned citations live here rather than in the user-facing page. All were checked with
`sp-rev2`'s `citecheck.sh`, which resolves the sha, the path, the line, **and** the referent text,
and requires the referent to *begin* on the cited line.

### Verified — env ladder (Phase 10, landed at `14883cbf`)

| Citation | Referent |
| --- | --- |
| `pkg/hub/httpdispatcher.go:1070@14883cbf` | `runtime_broker  <  hub  <  project  <  user` |
| `pkg/hub/httpdispatcher.go:1098@14883cbf` | `var envScopePrecedence = []string{` |
| `pkg/hub/httpdispatcher.go:1087@14883cbf` | `AppliedConfig.Env, then storage fills only the keys config left ABSENT` |
| `pkg/hub/httpdispatcher.go:1076@14883cbf` | `The scope may be removed entirely in a future` |
| `sym pkg/hub/httpdispatcher.go@14883cbf` | `resolveEnvFromStorage`, `buildEnvSources`, `WarnOutrankedBrokerEnvKeys` |

### Verified — baseline `ff835b0b`

| Citation | Referent |
| --- | --- |
| `pkg/config/settings_v1.go:55@ff835b0b` | `result.Env = mergeMaps(result.Env, profile.Env)` — the merge G3-full deletes |
| `pkg/config/settings_v1.go:76@ff835b0b` | `result.Env = mergeMaps(result.Env, override.Env)` — the merge that **survives** |
| `pkg/config/settings_v1.go:112@ff835b0b` | `rtConfig.Env = mergeMaps(rtConfig.Env, profile.Env)` — the inert `runtimes.<n>.env` field |
| `pkg/agent/run.go:464@ff835b0b` | `if settings != nil && !opts.BrokerMode {` — the mode gate |
| `pkg/runtimebroker/handlers.go:2283@ff835b0b` | `for _, override := range profile.HarnessOverrides {` — unfiltered |
| `pkg/runtimebroker/handlers.go:2294@ff835b0b` | `for _, hcfg := range settings.HarnessConfigs {` — also unfiltered |
| `pkg/config/settings.go:319@ff835b0b` | `func MergeSettings(base *Settings, data []byte) error {` — the dead layering entry point |
| `pkg/config/settings.go:459@ff835b0b` | `hov.Env = mergeMaps(hov.Env, expandEnvMap(hv.Env))` — the layering merge that never runs |
| `pkg/config/settings_v1.go:75@ff835b0b` | `if override.Env != nil {` — decoy 1 of 3, and the only live one |
| `pkg/config/settings.go:220@ff835b0b` | `if override.Env != nil {` — decoy 2 of 3, inside the unreachable v0 method |
| `pkg/config/templates.go:712@ff835b0b` | `if override.Env != nil {` — decoy 3 of 3, different referent (`api.ScionConfig.Env`) |
| `pkg/runtimebroker/handlers.go:517@ff835b0b` | `required, secretInfo := s.extractRequiredEnvKeys(req, hydratedHCPath)` — the production caller proving the second channel live |
| `sym pkg/config/settings.go@ff835b0b` | `mergeMaps` (second argument wins), `MergeResourceSpec` |
| `sym pkg/config/settings_v1.go@ff835b0b` | `ResolveHarnessConfig`, `ResolveRuntime` |
| `sym pkg/config/resource_defaults.go@ff835b0b` | `BuiltinDefaultResources`, `ShouldEnforceResourceDefaults` |
| `sym pkg/secret/localbackend.go@ff835b0b` | `Resolve` — the secrets ladder, read from the implementation, not a comment |
| `sym pkg/runtimebroker/handlers.go@ff835b0b` | `extractRequiredEnvKeys` |

### Controls run

Every one of these **failed**, as required:

- `cite 14883cbf pkg/hub/httpdispatcher.go 1070 zzzznotarealtoken` → FAIL (referent control)
- `sym 14883cbf pkg/hub/httpdispatcher.go zzzzNotARealSymbol` → FAIL (symbol control)
- `cite origin/main pkg/config/settings.go 261 "func mergeMaps"` → FAIL, *"'origin/main' is a
  symbolic rev, not a citation (resolves to ae4b60e1 today); pin the sha"* — G6 exercised, not
  assumed
- `git cat-file -e 9999999999^{commit}` → rc=128 (sha-existence control)
- `grep -rn zzzznotarealtoken --include='*.go' .` → empty (grep-instrument control)

All six cited shas were confirmed to resolve to commits before use: `ff835b0b`, `14883cbf`,
`86c571bf`, `dacbedeb`, `8649a216`, `ce23801a`.

## Rulings this document had to absorb mid-write

**Scope of the profile-env retirement flipped three times in seven minutes.** The final, settled
answer, and the one the doc ships:

```
profiles.<p>.env                          RETIRED
profiles.<p>.harness_overrides.<hc>.env   SURVIVES
harness_configs.<hc>.env                  SURVIVES — and it is THE MIGRATION PATH
```

Three things follow, and all three are in the shipped text:

1. **Both surviving paths get a row saying they survive.** A reader who sees `profiles.<p>.env`
   retired will assume the whole `profiles` env family went with it. Publishing "removed" for a
   surviving path is worse than omitting the row.
2. **The retirement removes the MIDDLE rank of a three-rank order.** So
   *"harness-config env now takes precedence over profile env"* is **still false** after the
   change — `harness_overrides` still outranks the base. Under the wider scope it would merely have
   been vacuous; under the narrow one it stays false. The page says this in as many words.
3. **The migration path is `harness_configs.<hc>.env`** — a *different, top-level* key, not
   `harness_overrides`. Named explicitly in the release note, because a breaking-change notice that
   says what you lost and not what to do is half a notice.

### Criterion 17d — naming the replacement *bare* would cause the misconfiguration

Late correction, and the most user-consequential one in this phase. My first draft said the
migration "preserves both the values and their precedence." **That is true about rank and silently
wrong about scope.** The two keys are scoped on orthogonal axes:

| Key | Scope |
| --- | --- |
| `profiles.<p>.env` | one **profile**, across every harness config resolved under it |
| `harness_configs.<hc>.env` | one **harness config**, across **every profile** |

`baseConfig := vs.HarnessConfigs[harnessConfigName]` (`pkg/config/settings_v1.go:44@ff835b0b`) is a
top-level map lookup; the profile is read only afterwards, at
`profile, ok := vs.Profiles[profileName]` (`pkg/config/settings_v1.go:46@ff835b0b`), and only for
overrides. So the naive migration **fans out** (one key becomes N, once per harness config the
profile covered) and, worse, **leaks**: with `dev` and `prod` sharing a harness config, a migrated
dev-scoped credential silently applies to `prod`, which never opted in. Measured — `prod` resolves
`{MIGRATED: from-harness-configs}`.

Both artifacts now carry the full 17d wording including the clause that must not be dropped for
brevity: **"There is no per-profile, all-harness-configs equivalent."** The reference page adds the
one partial recovery that does exist —
`profiles.<p>.harness_overrides.<hc>.env` is profile-scoped and survives, at the cost of one entry
per harness config, so it pays the fan-out to avoid the leak.

Fence carried: the gap is stated, **not** claimed unrecoverable. Nobody has checked whether some
other mechanism recovers per-profile scope.

**Correct as a pointer, wrong as an equivalence** — the guidance was right about where to go and
wrong about what you get when you arrive.

**`profiles.<p>.env` still parses.** `V1ProfileConfig.Env` is not removed, so the key is accepted
with no error, no warning and no log line. That is a user-facing property of the retirement and is
called out as a caution in both the reference page and the release note.

## Deliberate, measured omission — the `(*Settings).ResolveHarness` deletion

**It gets no release-note entry. This is deliberate, and it is measured.**

`(*Settings).ResolveHarness` has **zero non-test callers**. Positive control: the same grep shape
against its `VersionedSettings` sibling returns 5 live sites, so the instrument finds callers when
they exist. Three absence escape hatches were closed first — method values, interface method sets,
and reflection by name. The hunk is therefore **not user-visible**, and a breaking-change notice
for an unreachable path is a false notice in the same way a "removed" row for a surviving path is,
only inverted.

Recorded here so a later AC23 sweep does not flag it as a missing row: **AC23's second direction
does not apply to this hunk**, because the hunk is not user-visible.

### Do not call the `Settings` type "pre-v1" or "the old path"

**"v1" / "pre-v1" names the FILE SCHEMA, never the TYPE.** `Settings` is the current in-memory
settings type of the whole CLI and broker, constructed at **86 non-test sites**. Calling it legacy
tells a reader the wrong thing about all 86 of them. Phase 11 uses "pre-v1" only where it means the
on-disk schema, and names the method directly everywhere else. Swept and verified.

This is also why the decision above survives being checked. Criterion 17a as originally phrased
asked whether `Settings` is **reachable** — and the answer is emphatically *yes*. **The gate as
phrased answers yes and argues for an entry that must not ship.** The right gate is **method**
reachability, positive-controlled against a sibling symbol known to be live. Type and method come
fully apart here: a live type carrying a dead method. The decision did not change; the premise did.
That general shape is now a line in the reference page's own method section.

## CHANGELOG location — the design named a file with no entry section

The design says *"CHANGELOG.md — three entries minimum."* The tree does not support that:

```
CHANGELOG.md   4 lines of index prose pointing at changelog/. NO ENTRY SECTION EXISTS.
changelog/     <date>-changelog.md dailies, generated per merged-PR day
release-notes.md   docs-site/src/content/docs/ — user-facing prose, dated sections
```

Ruled by `sp-em`: prose goes in `release-notes.md`. `CHANGELOG.md` is left untouched — an
`## Unreleased` section would be the only entry section in a pure index file and the daily
generator does not know it exists. No `changelog/<date>-changelog.md` was hand-written; those are
generated from merged PRs and writing one by hand pre-empts the generator.

Reported to `sp-arch` as a design defect.

## Deliberate scope exclusions — flagged, not silently narrowed

- **`admin-settings.md` — not touched.** Documenting hub `agent_defaults` there would require
  publishing Bucket 4's precedence position as settled, and it is not: the intended model
  (low-priority fallback) and the implementation (applied at agent-create time, effectively
  highest) disagree, unresolved pending a follow-up workstream. The reference page documents the
  *intended* model and flags the actual as pending against issue #623. It does not publish a
  settled position.
- **`harness-settings.md` — not touched.** Same reason, for the profile-env retirement.
- **`harnesses/gemini-cli/home/.gemini/settings.json` — unmodified**, deliberately. See the known
  gap on the misspelled redaction key: correcting
  `security.allowedEnvironmentVariables` → `security.environmentVariableRedaction.allowed` would
  switch on a 34-entry allowlist covering `SCION_AUTH_TOKEN` and `GITHUB_TOKEN`. That is a
  security-posture change and must not ride along inside a precedence change.
- **`cmd/hub_secret.go:58@d2989488` — not "fixed"** to match `cmd/hub_env.go:59@d2989488`. The two
  now genuinely differ (issue #624) and the doc records that as a *filed difference*, not as
  documented design. `cmd/hub_secret.go:58@d2989488` reads
  `hub -> user -> project -> broker -> agent config`; `cmd/hub_env.go:59@d2989488` reads
  `broker -> hub -> project -> user -> agent config`.

  **These two citations are pinned to `d2989488`, not to this document's `ff835b0b` baseline, and
  that is deliberate.** At `ff835b0b` the two lines are **byte-identical** — the divergence only
  exists after `hub_env.go` was rewritten on the integration branch and `hub_secret.go` was not. A
  reader who applied this document's own pinning convention and resolved an unpinned citation at
  the declared baseline would find two identical strings and would hold **evidence for the very
  edit this warning forbids.** An unpinned citation inside a prohibition is not inert: it points at
  the one change the workstream has spent the day preventing. Caught by `sp-rev2`.

  Note also that the ladder line **moved within `hub_env.go`**, from `:58` at `ff835b0b` to `:59` at
  `d2989488`, because the rewrite inserted a line of prose above it. Pinning `cmd/hub_env.go:58@d2989488` at
  `d2989488` therefore resolves — to `first — a later scope overrides the same key set by an earlier
  one:`, which is not the ladder and not the thing being compared. Verified by running the wrong
  pin as a negative control and requiring it to fail.

## Known gaps catalogued on the page

Each is stated as a gap, not a fix, and each has a fence on what was and was not measured:

1. Annotation-set thinking level reaches the environment but is never written to
   `scion-agent.json`, so `scion agent config` and the web form do not display it — in effect and
   invisible on every surface an operator would check.
2. gemini-cli does not consume `SCION_THINKING_LEVEL` (control: it *does* read `SCION_MODEL`).
3. The gemini-cli redaction allowlist key is misspelled, outbound-only, and disabled by default —
   three independently sufficient reasons it is inert.
4. Hub `agent_defaults` bind once at create; a restart refreshes inline config but not hub
   defaults (#623).
5. Hub `agent_defaults` rank is **mode-dependent** — bottom of the broker chain in file mode,
   just above broker settings in Postgres mode.
6. File mode with a *remote* broker cannot reach hub `agent_defaults` at all.
7. `runtimes.<name>.env` is merged into `V1RuntimeConfig.Env` and **nothing reads that field**.
8. `extractRequiredEnvKeys` over-collects three ways: it still walks retired `profiles.<p>.env`;
   it walks **every** `harness_overrides` entry unfiltered by the selected harness config; and it
   walks **every** `harness_configs` entry likewise. The second **outlives the retirement**, since
   `harness_overrides` survives. Fenced: read from the collection loops; the downstream effect of a
   spurious required-secret was not traced.
9. Auth-candidate env keys are `delete`d from `opts.Env` when the resolved auth method does not
   require them — so delivery of `GOOGLE_CLOUD_PROJECT` / `CLOUD_ML_REGION` is conditional.
10. `default-model` / `InlineConfig.Model` do not reach the argv path for all harnesses.
11. `ScionConfig.Secrets` is accepted and persisted but inert.
12. Env and secrets rank `user`/`project` in opposite directions (#624).

## Two design notes carried onto the page

Both exist to stop a correct conclusion from being remembered by way of a refuted reason.

- **The rejected "delete the `!opts.BrokerMode` conjunct and nothing else" alternative.** Its
  stated rejection reason does not distinguish it from what shipped: wherever a harness config is
  named — the common case in a hub deployment — the two are byte-for-byte identical and the
  laundering the alternative was rejected for is fully present in both. What actually justifies the
  shipped design is the deletion of the explicit profile branch. Recorded so nobody reads "that
  alternative was rejected" as meaning the laundering was avoided.
- **One findings-matrix row compresses both precedence systems into one line.** It has no correct
  edit — a row spanning both systems cannot be corrected, only rewritten. Flagged rather than
  resolved.

## The `harness_overrides.<hc>.env` channel row, and why the deadness claim was re-measured

`sp-dev3` proved `MergeSettings` and `(*Settings).ResolveHarness` dead by rename-and-build at
`dacbedeb`. This page ships against `ff835b0b`, so the claim was **re-run here** rather than
inherited across a rev — a deadness result is a property of a tree, not of a symbol.

Procedure, in a throwaway worktree so the working tree was never mutated:

1. Baseline `go build ./...` **green** first. A mutation result means nothing without it.
2. Rename the declaration; **assert the rename actually applied** (`git diff --quiet` must fail).
   A `sed` that silently matches nothing also builds green, and would read as "dead".
3. Positive control: rename `ResolveHarnessConfig`, which is canonical, and require **red**.

Result: `MergeSettings` green, `(*Settings).ResolveHarness` green, control red at
`pkg/harness/resolve.go:81@ff835b0b` (`settingsEntry, _ := opts.Settings.ResolveHarnessConfig(opts.ProfileName, opts.Name)`),
column 37. `sp-dev3`'s finding reproduces at `ff835b0b`.

Step 2 earned its place immediately: the `extractRequiredEnvKeys` mutation **did not apply**, and
the assertion caught it. The symbol is a method — `func (s *Server) extractRequiredEnvKeys` — so a
receiver-less pattern missed it. Without the assertion that would have printed GREEN and been read
as "dead", which is the opposite of the truth; it is live, called from production at
`pkg/runtimebroker/handlers.go:517@ff835b0b`.

Two instrument findings from this pass:

- **`$PIPESTATUS[0]` is empty in zsh** (its array is 1-indexed), so `cmd | head; echo $PIPESTATUS[0]`
  yields an empty rc that reads as success — the same laundering family as the `cat-file -e` pipe,
  arriving through the *shell* rather than through git. Remedy used throughout: branch on rc in an
  `if`, never capture it.
- **`citecheck.sh` is NOT exposed** to the fail-open pipe form. It uses
  `git cat-file -e … 2>/dev/null || { fail; return 1; }` and `rev-parse --verify --quiet`, rc branched
  in a `||` with no pipe. Confirmed empirically as well as by reading: a bad sha and a missing path
  are both rejected.

The most useful part of the row is a **negative**: `MergeSettings` layers a whole `harness_overrides`
block and reads as load-bearing, so a reader could easily infer a settings-file layering precedence
rule for this key. There is none. The row says so explicitly.

The decoy warning is measured, not assumed: one grep for `if override.Env != nil {` returns exactly
three hits in `pkg/config` at `ff835b0b`, and only `ResolveHarnessConfig`'s is this key. The
`MergeScionConfig` hit is a different type (`api.ScionConfig.Env`) whose variable is also named
`override` and whose field is also named `Env`.

## Rows not verifiable against a landed commit at write time

Reported to `sp-em`; AC23's forward direction is a post-integration gate (criterion 24) and is not
this phase's to close.

| Row | State at write time |
| --- | --- |
| `runtime_broker` demotion | **Now citable** — Phase 10 landed at `14883cbf`. Written to the target ladder `runtime_broker < hub < project < user`, *not* to `ce23801a`, where `runtime_broker` is still highest. |
| harness-config env visible to auth pipeline | On `86c571bf`, a dev tip — not on `ff835b0b`. |
| `profiles.<p>.env` retirement | Deletions were local to `sp-dev4` at write time; `ff835b0b` still carries all three merges. |

### Closed — re-verified against the integration branch `d2989488`

The integration branch came up after this page was written, so the two rows above that were pending
a landed commit are now measurable. **All hold.** Measured at `d2989488`, each against `ff835b0b`
with a positive control sharing pathspec and rev:

| Claim on the page | `ff835b0b` | `d2989488` | Verdict |
| --- | --- | --- | --- |
| `profiles.<p>.env` **retired** | merge present (1) | merge **gone** (0) | holds |
| `harness_overrides.<hc>.env` **survives** | present (1) | **present** (1) | holds |
| `runtimes.<n>.env` inert (`Known gap`) | present (1) | present (1) | holds |
| retired injection point `else if profileName != ""` | present (1) | **gone** (0) | matches criterion 17 predicate |
| the three-decoy `if override.Env != nil {` set | 3 hits | **3 hits** | holds |
| `MergeSettings` callerless | compiler-proven dead | 0 non-test callers (grep) | holds, weaker instrument |

The `d2989488` figure for `MergeSettings` is **grep-level, not compiler-level** — the rename-and-build
was run at `ff835b0b`. Recorded as the weaker claim rather than presented as equivalent.

**The line drift is the useful part, and it arrived as evidence rather than as an argument.** The
same three decoy referents moved between the two revs *within hours*:

```
pkg/config/settings.go      220 -> 233   (+13)
pkg/config/settings_v1.go    75 -> 106   (+31)
pkg/config/templates.go     712 -> 712   (  0)
```

The reference page cites these by **symbol**, so nothing on it drifted. This log cites them by
`file:line@ff835b0b`, which also did not drift, because a sha names an immutable tree — that is
exactly what the `@sha` requirement buys, and it is why a branch name would have been wrong here
rather than merely unfashionable. Had either artifact cited a bare line number, two of the three
would now resolve to real code that is confidently the wrong thing, and the third would still be
right — which is the worst possible mix, because it makes the convention look like it works.

One integration hazard, recorded because it is branch-dependent right now: the
`else if profileName != ""` branch in `pkg/agent/run.go` is **present** at `ff835b0b` and absent at
`dacbedeb` (extracted into `resolveAuthEnvOverlay`). Criterion 17 must be run **on the integrated
branch**, never on a dev tip.

The mechanism as first written here was wrong and is corrected, because a refuted mechanism is how
the next reader talks themselves out of a requirement. I wrote that a **misresolved conflict** would
reinstate the retired injection point. It cannot: the change is **one-sided**, and a one-sided change
does not conflict at all. Git will merge it clean and silently. The real failure mode is a **dropped
commit** — silent, with nothing to review — not a bad conflict resolution, which would at least be
loud. `sp-rev-p5` established this and `sp-dev4` sharpened it independently.

**The requirement is unchanged and should stay exactly as strict.** Only the reason moves. This is
the third time in this workstream that a correct requirement was found resting on a mechanism that
could not fire, and the repair is the same each time: keep the requirement, replace the reason. The
failure mode being guarded against here is not "reviewer resolves a conflict badly" but "nobody ever
sees the region at all".

## Citation coverage gate — and the parser bug it was written to catch

`sp-rev2` found that my citecheck run reported "16 parsed, 0 failed" on an artifact holding
eighteen citations. Nothing failed because two inline-prose citations were never *seen*: the row
parser required a table shape they did not have. **A parser control validates the parser against
itself.** The count it printed was rows found, not citations present, and those two numbers are
only equal when there is no bug.

The gate is now: inventory the **raw** artifact by citation *shape*, then require
`checked == contained`. Result on this file — 25 references, 25 pinned, 22 unique, 22 checked,
0 no-referent, 0 failed. Three fixes rode along, all `sp-rev2`'s: four citations that carried no
sha now carry one, the bare-filename citation is now `pkg/runtimebroker/handlers.go:517@ff835b0b`
(two files named `handlers.go` exist in the tree, so the bare form named neither), and the
`hub_secret`/`hub_env` pins below.

The gate then fired on **this very section**: writing the sentence above in its first form
introduced a bare, unpinned reference into the artifact, and the run flagged it at its line number.
That is the cheapest possible demonstration that the gate is live, and it is a better one than a
synthetic control, because I did not plant it.

Two instrument failures were found *while building the gate*, and both are the day's shape:

- **The shell version silently skipped every predicate.** It built a `sed` expression as
  `s/.*${cit}...` where `$cit` contains `/`, which breaks the delimiter — every row took the
  "no referent" branch and printed a plausible summary with a healthy-looking total. Rewritten in
  Python, where the citation is *data* and never re-parsed by a text processor.
- **My first distance-ordered extractor fabricated 13 predicates out of nothing.** Scanning one
  window *across* the citation shifts backtick pairing by one, so the text *between* two spans is
  returned as if it were a span. The failure was loud (13 FAILs) only because the checker is
  fail-closed on a wrong referent; had the fabricated strings happened to appear at the cited
  lines, this would have been 13 silent passes. The two windows are now scanned separately.

The gate carries its own positive control — `citecheck.sh` is handed a referent that is *not* at
the cited line and must reject it — and every `PASS` is void if that control does not fire, which
is asserted rather than eyeballed.

## `envScopePrecedence` is introduced by this workstream, not inherited — measured, four revs

Prompted by `sp-arch`'s §7 ("a referent is a set and a moment"), I re-derived the symbol my System-A
section is built on, at four revs, each with a positive control sharing the pathspec and the rev:

| Rev | What it is | files containing `envScopePrecedence` | control: files containing `ScopeRuntimeBroker` |
| --- | --- | --- | --- |
| `ff835b0b` | this document's baseline | **0** | 9 |
| `14883cbf` | integration, cited by this log | 2 | 10 |
| `d2989488` | integration, later | 2 | 10 |
| `f0093316` | `origin/main` now | **0** | 9 |

The two zeros are **not** a deletion and it matters that they are not: the symbol is **absent at
main and absent at my own baseline, and present only on the integration branch**, so it *arrives*
with this workstream. A reader who resolved the page's symbol citation against `origin/main` today
would find nothing and could reasonably conclude the page documents something that does not exist.
It documents something that does not exist **yet**, which is what a release note is for.

The list body is byte-identical at both integration revs (`runtime_broker`, `hub`, `project`,
`user`, all four via `store.Scope*` constants), so the System-A order the page states is unchanged
across the integration branch's movement. Recorded because a symbol citation is drift-proof against
line motion and **not** against a symbol that has not landed on main.

## Published blob equalities, re-checked under the shape guard

`sp-arch` §3: `git rev-parse "<rev>:<path>"` **echoes its input on failure**, so `[ -n "$v" ]`
passes on a bad path. I had published three blob equalities in the out-of-band SHA notice using
exactly that instrument. Re-run with `[[ $v =~ ^[0-9a-f]{40}$ ]]`: all three still IDENTICAL, both
sides real blobs; the must-differ control (the project log itself) differs with both sides
shape-valid; and the negative control confirms the guard rejects the 34-character echo a bad path
produces. The equality limb was never at risk — two bad paths echo two *different* strings, so a
bad path reports DIFFERS — but the control certifying it was, and that is the limb that was fail-open.

## `sp-dev3`'s REFUTED row — the page ranked a tier it says twice it will not rank

This is the one review finding that changed the **user-facing page**, and it is this document's own
criterion firing against its author.

The `resources` chain listed six rungs, one of them `hub agent_defaults.resources`, sitting between
`profiles.<p>.resources` and `default_resources`. Two other places on the same page contradict that:

- the `Pending` box: "**it does not publish a settled precedence position for hub
  `agent_defaults`.** Do not build on either reading." (issue #623)
- the hub-storage-mode table: Postgres mode → **above** the broker's `settings.yaml` defaults;
  file mode → **Bottom** of the broker chain, which is *below* `default_resources`.

The chain and the file-mode row cannot both be true. And a third symptom was sitting underneath:
the note below the chain already read "**The five ranks**" while the chain listed **six** — the hub
tier was reified as a rank in one place and counted out of existence in the other.

**Why it happened is the interesting part.** The section spends four paragraphs refusing to collapse
the broker's `settings.yaml` into one rung, because one file contributes at three ranks. It then
did exactly that to the hub tier, whose rank moves with deployment mode. Same defect, other axis.
A chain has one rung per participant, so *the form itself* manufactures a rank — it cannot express
"this rung moves with deployment mode" any more than it can express an interleave. **I saw the
problem clearly for one participant and not for the other, in the same block.**

Fix, doc-only, no re-ranking and no code: the hub tier is removed from the chain and rendered as an
explicit non-rung with a pointer to the mode table; a new subsection states the general rule (**a
contribution can be given a single rank only if it occupies a single rank**) and names both
failures of it; the "five ranks" wording is now true of a five-rung chain; and the mode table gained
a paragraph saying in as many words why the tier is absent from the chain. Every pairwise relation
`sp-dev3` CONFIRMED is untouched.

Verified by a negative control on the defect itself — the chain block must contain zero rungs
matching `hub agent_defaults`, with a positive control that the block was located at all (10 lines,
4 `>` rungs + the head = the five ranks the note claims).

### `sp-dev3` corrects this document upward — and the reason I first recorded was wrong

**Conclusion, unchanged and confirmed:** the integration branch carries both the env ladder and the
retirement, and all six env pairs hold there, not merely on a dev branch. `envScopePrecedence` is
present at `d2989488` in two files. That is what matters for this page and it is right.

**The stated cause was false, and this paragraph carried it for an hour.** An earlier revision read:
"`d2989488` was not in its object store, and `git grep` over an unresolvable rev returns zero
matches with no error." Measured here, both halves fail:

```
git grep -c PATTERN <unresolvable-sha> -- <path>
  UNPIPED   rc=128, "fatal: unable to parse object: …"    LOUD
  PIPED     value empty, pipeline rc=0                    SILENT
```

**The pipe is the defect, not the rev.** Unpiped, an unresolvable rev is one of the *noisiest*
failures git produces. Stated unconditionally — as I stated it — the rule is simply untrue, and it
would teach the next reader that pinning a rev is a hazard when the actual hazard is `| tail -1`
swallowing rc=128. That is the same instrument-collapse family already catalogued at the top of
this log, and I filed it under the wrong heading.

**On the attribution.** `sp-dev3` says it made no such retraction. I hold the message — 21:27:51Z,
delivered four times, headed "🔴 RETRACTION from sp-dev3" and stating verbatim "CAUSE: `d2989488`
WAS NOT IN MY OBJECT STORE" and "AN UNFETCHED SHA IS A SILENT EMPTY SCOPE." So the paragraph was a
faithful record of what arrived. `sp-dev3` subsequently re-measured, found rc=128 unpiped, and
withdrew the rule as a duplicate of an existing item — **a withdrawal this document never received.**
Both parties are describing something real: it sent the claim, it later retired it, and I was still
holding the first version.

That is the failure mode `sp-dev3` itself names, and its own §3 is an instance of it:

> **A CORRECTION'S PREMISE IS THE LEAST-AUDITED SENTENCE IN ANY REVIEW, BECAUSE THE CONCLUSION IS
> RIGHT AND THE CONCLUSION IS WHAT GETS CHECKED.**

Its conclusion ("your document teaches a false rule") is correct and I have acted on it. Its premise
("the retraction did not occur") is checkable and does not survive the receipt. Neither of us
audited a premise while both of us were right about the thing we cared about. **A withdrawal
propagates to fewer readers than the claim it withdraws**, which is the argument for a retraction
ledger rather than for either of us being more careful.

**My two absence rows survive on their own evidence, not on the withdrawn rule.** The four-rev table
has zeros at `ff835b0b` and `f0093316`. Each carried a positive control sharing rev and pathspec
(9 and 10 hits), which proves the rev resolved and the pathspec was live; `sp-dev3` re-measured them
independently with a `package hub` control reading 275/276 and confirms them as real absences. The
durable habit is the one it names, and four instruments failed today for want of it:

> **EVERY ABSENCE ROW CARRIES A POSITIVE CONTROL IN THE SAME INVOCATION.**

The possession check (`git cat-file -e "$r^{commit}"`) is still worth running before trusting a rev
— it caught `e2514675` missing from my store earlier tonight — but it is a *cheap precaution*, not
the explanation for a silent zero. Those are two different findings and I had merged them.

### The anchor gate produced a self-inflicted false positive for the second time

It reported two broken anchors on headings containing `_`. github-slugger **keeps** underscores;
my slugger stripped them. The first time this happened the cause was `re.sub(r'\s+','-')` collapsing
runs of whitespace where github-slugger replaces each space individually, and it manufactured eight
breakages. Both times the instrument accused the document and both times the document was right.

Twice is a pattern worth naming: **a reimplementation of someone else's normaliser is a hypothesis,
not a measurement.** The two controls now in the gate — it must accept a slug derived from a real
heading and reject one that is not a heading — catch a *dead* slugger and cannot catch a *subtly
wrong* one, because both controls pass under a slugger that is wrong only about `_`. The honest
statement of coverage: this gate proves the anchors match **my** slugging rule, and my slugging rule
has now been wrong twice.

Current: 42 headings, 12 in-page links, 0 broken, both controls fire.

## The four unpinned citations belong at `ff835b0b`, not `d2989488` — measured, not argued

`sp-em` (21:26Z) asked for the four unpinned citations to be pinned **to `d2989488`**. They are
pinned to `ff835b0b`, and that is deliberate. Every citation was run at **both** revs:

| Citation as pinned | resolves at its own pin | same line re-pinned to `d2989488` |
| --- | --- | --- |
| `pkg/harness/resolve.go:81@ff835b0b` | PASS | PASS |
| `pkg/runtimebroker/handlers.go:517@ff835b0b` | PASS | PASS |
| `pkg/config/settings.go:220@ff835b0b` | PASS | **FAIL** |
| `pkg/config/settings_v1.go:75@ff835b0b` | PASS | **FAIL** |
| `pkg/config/templates.go:712@ff835b0b` | PASS | PASS |

**Pinning as instructed would have shipped two citations that do not resolve.** The two that fail
are exactly the two decoys recorded earlier as having drifted (`settings.go` +13,
`settings_v1.go` +31); `templates.go` did not move, which is why it passes at both.

The directive over-generalises from a case where it *is* right. `cmd/hub_secret.go:58@d2989488` and
`cmd/hub_env.go:59@d2989488` must be pinned to that rev **because the claim they support is only
true there** — at `ff835b0b` the two lines are byte-identical. The other four support claims stated as
of this document's baseline, so `ff835b0b` is their moment. **A pin is not a freshness setting; it
names the tree in which the sentence is true.** Pin the rev the claim belongs to, per citation, not
per document.

And this is the "partially drifted is worse than fully drifted" hazard arriving as a live instance
rather than an argument: a spot-check of the three decoys at `d2989488` returns **two FAIL and one
PASS**, and the one that passes is the one a reader is most likely to try first.

### The coverage gate passes an empty artifact, and that is the same bug it was built to catch

Pointed at the published page, the gate printed `contained=0 pinned=0 checked=0` and then
**`COVERAGE OK`**, with its own positive control still firing. The page is clean for a real reason —
every `file:line` citation lives in this log, not in user-facing prose — but the gate cannot tell
"nothing to check" from "everything checks out", and it was written *specifically* to stop a run
reporting health over rows it never examined. It grew a coverage assertion on the parser and none on
the subject. **A COVERAGE GATE NEEDS A NON-EMPTINESS ASSERTION ON ITS SUBJECT, OR ITS GREEN IS ALSO
AVAILABLE TO AN EMPTY FILE.** Same shape as the unfetched-sha defect `sp-dev3` retracted over: an
empty scope answers every question with "no problems found".

It also fired a third time on my own freshly written prose — the measurement table above introduced
seven unpinned references, taking `pinned` to 26 of 33. Three unplanted catches now, all on text
written *while documenting the citation rules*. That is not carelessness so much as evidence that
prose about citations reliably manufactures citation-shaped strings, and the only thing standing
between that and the artifact is the gate.

Disk, per `sp-em`'s fleet warning: `df` read 96% / 9.0G free **before and after** this measurement,
unchanged. No test binary was run — this phase's gates are `go build`, `golangci-lint`, and two
text gates, none of which allocate scratch space.

## The "they cannot drift apart" sentence was false, and `citecheck` was never going to catch it

The page claimed the env order is stated in exactly one place, that **every** consumer derives from
`envScopePrecedence`, and that changing that list "is the only edit needed to change the order
everywhere." `sp-rev2` measured it as AFFECTED. There is a fourth provenance surface,
`Server.buildEnvGatherResponse`, which reads no list.

**Why every gate on this page passed it.** The sentence is a faithful transcription of the source
comment at `pkg/hub/httpdispatcher.go:1121@d2989488`, which says the same thing —
`(WarnOutrankedBrokerEnvKeys). Changing the order here is the ONLY edit` — and
`pkg/hub/httpdispatcher.go:1196@d2989488` repeats it:
`the same property that makes the resolver and the provenance reporter unable`.
The citation resolved, the predicate matched, the gate went green.

> **`citecheck` VERIFIES CITATION → REFERENT. IT CANNOT VERIFY REFERENT → CLAIM. A DOC THAT
> FAITHFULLY TRANSCRIBES A FALSE COMMENT PASSES EVERY CITATION GATE WE HAVE.**

That is the same defect class as the coverage gate passing an empty file and the anchor gate
accusing a correct document: **an instrument that checks the link and not the destination.** The
prose was not sloppy — it was *too* faithful. It inherited the comment's authority along with its
error, and the citation is what made it feel checked.

### What I verified before transcribing, and one place the brief was loose

The brief handed me a replacement paragraph. Transcribing it on report would have repeated the
exact failure being fixed, so each clause was measured at `d2989488`:

- `envScopePrecedence` is `runtime_broker, hub, project, user`
  (`pkg/hub/httpdispatcher.go:1124@d2989488`, `var envScopePrecedence = []string{`).
- `envScopeSourceLabel` (`:1175@d2989488`) emits `hub`, `project`, `user`, **`broker`**.
- `buildEnvGatherResponse` defaults `Scope` to `"hub"` and probes `user`, `project`, `config`,
  `secret` — **no `runtime_broker` probe anywhere.** A broker-only key therefore falls through to
  the `hub` default while `scion hub env list` calls it `broker`. Confirmed.

**The divergence is bidirectional, and the brief only had one direction.** It reported that the
gather API misses `broker`. It also *emits* `config` and `secret`, which are not env scopes in the
ladder at all and cannot be set with `scion hub env set`. So the two surfaces do not merely rank
differently — **they do not share a vocabulary in either direction.** The published caution states
both directions, because a reader told only about the missing `broker` rung would still assume the
two answer from the same alphabet.

**Correction to the brief's scoping, which does not change the instruction.** The brief calls
`buildEnvGatherResponse` "outside both file sets." Its **file** is not:
`pkg/hub/handlers_agents_core.go` differs between `ff835b0b` and `d2989488` (blob `78c55296` →
`409ca0f9`). But all ten hunks land in `createAgentInProject`, and the function body itself hashes
identically at `ff835b0b`, `d2989488` and `f0093316` — 112 lines, same digest at all three. The
must-differ control (`createAgentInProject`, same extraction) *does* differ across the pair, so the
extraction discriminates rather than returning a constant. **The function is untouched; the file is
not.** "Outside the file set" and "untouched by the workstream" came apart here, and only the
second one is true.

### The citation convention has no range form, and a range reads as UNPINNED

Writing the section above I reached naturally for a hyphenated line **range** on
`pkg/hub/httpdispatcher.go`, because the claim genuinely spans five comment lines. The gate scored
it **unpinned**: it matches the leading `file:<line>`, then finds a hyphen and a second number
where it expects `@<sha>`. So the range form silently loses its pin while *looking* more precise
than a single line. (Naming the offending form literally here would itself plant an unpinned
citation — it did, on the first draft of this paragraph, which is the fifth catch.)

The right repair is not to teach the parser ranges. It is that **the convention already answers
this and I ignored it**: the referent must BEGIN on the cited line, which makes the start line the
only one carrying information. `:1119-1123` asserts nothing `:1121` plus a predicate does not, and
the predicate is the part that actually gets verified. Both are now single-line with quoted
predicates.

Worth noting the near-miss: a range citation is *unpinned*, not *wrong*, so it fails loudly on the
pin check. Had the gate only verified referents it would have passed — the text at `:1119` is real.
**The pin check caught a defect the predicate check could not see**, which is the first time today
those two limbs have come apart in that direction.

Fourth unplanted catch on my own prose, and again in text written about citation discipline.

**And the same run caught a third bad line number handed to me in a brief.** The brief attributed
`the same property that makes the resolver and the provenance reporter unable to drift apart` to
`:1195`; it begins at **`:1196`**. I had transcribed the brief's number and written the predicate
from the brief's quotation, so the two disagreed and the gate failed the row rather than passing a
citation that resolves to the wrong line. That is `:58` and the `d2989488` re-pin making three
today, all off-by-a-little rather than off-by-a-lot.

> **THE DANGEROUS LINE NUMBER IS NEVER THE ONE THAT IS OBVIOUSLY WRONG. IT IS THE ONE A FEW LINES
> OFF, INSIDE THE SAME COMMENT BLOCK, WHERE THE SURROUNDING TEXT STILL LOOKS RIGHT.**

The predicate is what saves this. A line number alone would have been accepted by any reader
skimming the neighbourhood.

### The non-emptiness assertion is implemented and its control fires

`citecov.py` now fails a subject containing zero citations unless `--allow-empty` is passed
explicitly, so "there are no citations here" must be *asserted* rather than defaulted. Verified in
both directions: the project log passes with 35 contained / 35 pinned, and the published page —
which legitimately has no `file:line` refs — now reports `SUBJECT EMPTY … this is NOT a pass` where
it previously printed `COVERAGE OK`. Adopted `sp-arch`'s prologue as well:
`git rev-parse --is-inside-work-tree || ABORT`, since a stray `cd` produces a uniform column of
zeros and **uniformity reads as rigour**.

### The first correct pin anyone handed me today, and why it is not on the page

`sp-rev2` re-measured `buildEnvGatherResponse` rather than carrying `sp-em`'s coordinate forward,
and its pin is right: `pkg/hub/handlers_agents_core.go:1019@ff835b0b` —
`func (s *Server) buildEnvGatherResponse(ctx context.Context, agent *store.Agent, brokerReqs *RemoteEnvRequirementsResponse) *EnvGatherResponse {`.
The function begins exactly there. Three bad line numbers today and one good one, and the good one
came from the reviewer who re-measured instead of relaying.

**The pin lives here and not on the published page, deliberately.** The page carries **zero**
`file:line` citations — it cites by symbol (`buildEnvSources`, `WarnOutrankedBrokerEnvKeys`,
`envScopePrecedence`) throughout. That is not an oversight; a line number in user-facing prose rots
against every refactor and a reader cannot check it without the repository at that sha. Putting one
line pin into a page whose convention is symbols would also be the *only* one, so it would read as
significant when it is merely more brittle. Recorded here where the citation gate can verify it.

### `sp-arch`'s sentinel reading is sharper than mine and I have adopted it

I had written that a broker-only key "falls through to the `hub` default." True and too gentle.
Reading the function body confirms `sp-arch`'s version: `Scope: "hub"` is the **initial value**,
and `source.Scope == "hub"` is then used **four times as the not-yet-resolved test**. So `hub` is
simultaneously a sentinel and a shippable answer.

> **A SENTINEL THAT IS ALSO A VALID VALUE CANNOT BE DISTINGUISHED FROM A RESULT BY ANY CALLER,
> INCLUDING THE FUNCTION THAT SET IT.**

The key is reported as `hub` **not because it was found at hub scope but because it was found
nowhere**, and "nowhere" is spelled the same as a real answer. The page now says this, because the
difference is exactly what a reader debugging a wrong provenance label needs to know. It is also
today's recurring shape appearing in production code rather than in our instruments: empty read as
zero, absence read as pass, unresolved read as hub.

### The arithmetic line `sp-em` is owed: 26 contained vs 22 checked

`sp-rev2` could not reconcile my `contained=26` against its own count of 22 unique / 33 occurrences,
and correctly guessed the gap is definitional. It is, and my summary line invited the confusion:

- **`contained`** counts **occurrences** of a `file:line` reference in the raw artifact.
- **`unique`** counts distinct `(citation, sha)` **pairs**.
- **`checked`** counts pairs actually resolved — one resolution per pair, not per occurrence.

So the gate's invariants are `pinned == contained` (every occurrence carries a sha) and
`checked == unique` (every distinct pair was verified). **`checked == contained` is not an
invariant and cannot be**, since a citation repeated four times is one referent. The 26/22 gap was
four repeats. `sp-rev2`'s 33 is a count taken at a different tip of a file I have amended several
times since; the current run reads 36/36/25/25.

`sp-em` is still right about the reporting: **when a gate prints two numbers under a rule stated as
"total equals rows checked", an inequality is the gate firing, not a footnote.** The defect was the
summary line, which printed three counts side by side under a one-line rule that only governed two
of them. Naming which pairs of numbers are asserted equal is now part of the output rather than
something a reader must infer.

`sp-arch`'s companion catch on `sp-rev2`'s own 33/33 is the same family from the other side: the
two arms matched because `\.go:[0-9]+` is a **prefix** of every pinned citation, so the arithmetic
balanced for a reason unrelated to the property being measured. **Cheap check: is one pattern a
prefix of the other.**

### My own caveat walked into the trap `sp-dev3` had just named

`sp-dev3` reported that a 10-vs-0 pathspec count "looked exactly like a second precedence ladder in
production, and only READING the function showed it was a labeller, not a resolver." My caveat had
just described `buildEnvGatherResponse` as working "from its own hardcoded **chain**" — and *chain*
is the word this page uses everywhere else for a precedence ladder. A reader who knows the page's
vocabulary would have concluded the codebase contains a **third** precedence system.

That would have been a worse error than the one the caveat exists to fix. The original sentence
overclaimed unity; this would have manufactured a rival ladder that does not exist.

The distinction is now explicit on the page: **it is a labeller, not a resolver.** It never decides
which value wins, only what to call a value it was already handed — so what it gets wrong is a
*name*, which is a reporting bug, not a precedence bug. "Chain" is now "probe order".

> **THE WORD THAT IS SAFE IN GENERAL PROSE IS NOT SAFE IN A DOCUMENT THAT HAS ALREADY GIVEN IT A
> TECHNICAL MEANING. A GLOSSARY IS A CONSTRAINT ON THE AUTHOR, NOT ONLY A SERVICE TO THE READER.**

Same class as the `hub`/`global` caution this page already carries — a word with an established
local meaning being reused for something else. I wrote that caution and then did the thing it warns
about, four hundred lines further down.

### Transport, closing note

`sp-em` withdrew the `--in 30s` mandate at 22:04Z as recovered, then corrected itself at 22:06Z to
**intermittent**. Both of my reports went out on the scheduler path and both returned rc=0 with the
`scheduled for` literal, so nothing needs resending — and per the standing no-retry ruling I have
not resent anything. The durable line is `sp-dev8`'s:

> **AN OUTAGE WORKAROUND IS A CLAIM ABOUT THE WORLD WITH A SHORT HALF-LIFE, AND UNLIKE A
> MEASUREMENT IT DOES NOT CARRY ITS OWN EXPIRY.**

Which is the same defect as a stale pin, in a different medium: my `ff835b0b` citations are still
true because they name their moment, and "the channel is down" was false within twenty minutes
because it did not.

### `sp-dev3`'s Phase 11 report is de-anchored — but only half of it needs re-running

Confirmed here, its numbers exactly:

```
git cat-file -e f36e99d1^{commit}                 -> resolves
git merge-base --is-ancestor f36e99d1 HEAD        -> NOT an ancestor
git diff --stat f36e99d1 HEAD                     -> 2 files, +468 -19
                              settings-precedence.md   +72 -15
```

> **A VERDICT PINNED TO A SHA SURVIVES A FORCE-PUSH AS HISTORY AND DIES AS A REVIEW. NOTHING IN THE
> REPORT ANNOUNCES THE EXPIRY, BECAUSE THE REPORT CANNOT KNOW.**

Right, and this is the amend-per-review-cycle cost that a one-commit branch quietly imposes on
everyone reviewing it. **My branch discipline is what expired their work.**

**But the report is two kinds of claim and only one of them expired**, and the distinction is worth
making before anyone re-runs 400 lines:

- **Code-facing verdicts are untouched.** The four CONFIRMED pairwise relations cite
  `provision.go@01b869ae`, and the six env pairs cite the integration revs. Those are statements
  about trees my branch does not contain and cannot move. A force-push to `scion/sp-dev7` has no
  bearing on them. They are still live and still correct.
- **Doc-facing verdicts genuinely died.** Those assert that *this page* says something, and the page
  moved.

**And the doc-facing half is narrower than +468 suggests.** 400 of those lines are this project log,
which is not user-facing and which none of `sp-dev3`'s verdicts are about. The page itself moved
`+72 -15`, in exactly two neighbourhoods: the env ladder (where the provenance caveat landed) and
the resources chain / hub-storage-mode region (the REFUTED-row fix). Both were reviewed regions, so
re-anchoring **is** warranted — but it is two sections, not a document.

Stated as a rule, since it generalises past this branch: **a review verdict expires against the
artifact it describes, not against the branch it was measured on.** Re-run what the diff touched;
carry forward what cites a rev the diff cannot reach.

## Gate

`go build ./...` and `golangci-lint` run clean. Whole-repo `go test ./...` is **not** a valid gate:
`internal/fixturegen`'s `TestFixtureCoverage` is red on `main` for unrelated reasons (#625) and is
excluded, both from the gate and from the test recipe the reference page documents.
