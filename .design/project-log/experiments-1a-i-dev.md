# Experiments Phase 1a-i — Go core (ptone/scion#2217)

PR: ptone/scion#2276 · Branch: `scion/experiments-1a-i` · Design: revision 6
(frozen), §3.2, §3.3, §3.6, §7 1a-i, §9, §10.

## Scope

Go core only: `pkg/experiments` registry, the `opsettings` `experiments`
section, `OperationalSettings` accessors, and the server-side resolution and
`requireExperiment` gate. No HTTP endpoints and no authorization wiring — that
is Phase 1a-ii. No web changes; the `DEFAULT_ON_FLAGS` consistency test reads
`web/src/utils/feature-flags.ts` as it stands today and passes unmodified,
per §7.

## Decisions / notes for reviewers

- **Registry entry placement.** The `opsettings.Registry` entry for
  `"experiments"` is appended at the end (after `harness_configs`), and the
  schema-map entry is inserted after `auto_expose_ports`. ptone/scion#2270
  (`quotas`) inserts mid-`Registry` after `auto_expose_ports`, not at the
  tail, and both PRs touch the `TestRegistryHasAllSections` expected-names
  line (`opsettings_test.go:33-35`). My change there is a single-element append to
  keep that conflict minimal and textual, as instructed.
- **`TestSectionHasKoanfPaths`** (pre-existing test, not called out in the
  design) also enumerates DB-only sections by name. Added `"experiments"`
  there alongside `"maintenance"`/`"messaging"` — needed for the suite to
  pass, same rationale as the `TestRegistryHasAllSections` addition.
- **`ExperimentsReadResult`** fields are `{Overrides, Revision, Malformed,
  Err}`, per the design's "FYI notes" (left to the implementer).
- **Seeding-skip test.** The design points at extending
  `seed_roundtrip_test.go`; that file's existing tests are about
  `SeedEquivalent` env-var round-tripping, not per-section seeding skip, so I
  added a new, separately-scoped test there
  (`TestExperimentsSectionSkippedBySeeding`) asserting `KoanfPaths == nil` and
  that `ExtractSectionFromKoanf` returns an empty document, rather than
  bending an existing test to fit.
- **Consistency test, without touching the TS file.** Besides the real
  read-and-compare test against `feature-flags.ts`, there's a second test
  (`TestDefaultOnFlagsConsistency_DetectsMissingEntry`) that feeds a synthetic
  registry and a stale flag list to the comparison function directly, proving
  the check fails on drift (an explicit §9 criterion) without needing to
  simulate an out-of-sync file on disk.
- **Zero-value `&Server{}` test seam.** `experimentRegistry()` and
  `experimentsSnapshot()` are nil-safe (fall back to `experiments.Default()`
  and an empty snapshot respectively), matching the existing
  `route_classification_test.go` pattern of building `&Server{...}` by struct
  literal without `New()`. `TestNew_StoresServerConfigExperimentsRegistry`
  (`experiments_new_test.go`, tagged `!no_sqlite` since it needs a real store)
  separately covers the other end of that seam: the `New()` assignment that
  actually carries `ServerConfig.Experiments` onto a real server.
- **No package-level mutation.** All registry tests build fresh `*Registry`
  values via `NewRegistry`; none touch `compiled`/`compiledRetired` directly,
  so `t.Parallel()` would be safe if added later (design.md §3.2, §9).

## Deviations from the initial draft

- Trimmed the first pass of tests toward the M-size target by consolidating
  several single-assertion tests into table-driven tests
  (`TestExperimentEnabledIn_Resolution`, `TestExperimentsSnapshot`,
  `TestReadAuthoritativeExperiments`, `TestNewRegistry_InvariantViolations`).
  The diff still ends up above the ~450–600 target — the §9/§10 rows assigned
  to 1a-i cover a wide matrix (registry invariants, the malformed-row policy,
  snapshot/read-result cases, `requireExperiment`'s gate and panic behaviors)
  plus coverage added during review, and I did not cut coverage to hit the
  number. See the PR diff stat for the current size rather than a count here,
  which would go stale on the next push.

## Verification

- `go build ./...` — pass.
- `go vet ./pkg/experiments/... ./pkg/config/opsettings/... ./pkg/hub/...` —
  clean.
- `go test -p 2 ./pkg/experiments/... ./pkg/config/opsettings/...` — pass.
- `go test -p 2 -run 'Experiment|RequireExperiment' ./pkg/hub/...` — pass.
- `go test -p 2 -timeout 25m ./pkg/hub/...` (the project's own
  `test-hub-sqlite` gate, including its existing `-skip` list for four
  pre-existing tracked failures, ptone/scion#1847): ran to near-completion in
  this sandbox before the 25m wall-clock budget ran out mid-suite (on
  `TestBrokerAuthGates`, unrelated). The only failures observed before the
  timeout — `TestBackupBinary`, `TestBackupAndRestoreRoundtrip`,
  `TestReincarnateAgent_WorkerStepsBumpRecordUpdatedAt` — are in binary
  backup/update and agent-reincarnation code this PR does not touch. All
  experiments-related tests within that run passed.
