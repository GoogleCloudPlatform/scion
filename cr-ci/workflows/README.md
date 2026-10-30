# cr-ci workflow templates

These files are **templates, not active workflows**. GitHub only runs
workflows under `.github/workflows/`, so the copies here never run. Putting
them there is a separate, reviewed step that needs ptone's go-ahead: the PoC
is P6, and the routing in `.github/workflows` is P7 (see "Routing in
`.github/workflows`" below).

| File | What it is |
| :--- | :--- |
| [`cr-runner-poc.yml`](cr-runner-poc.yml) | PoC workflow (push to a throwaway `cr-ci-poc-*` branch, or `workflow_dispatch`): runs `golangci-lint` on a cr-ci runner without the `-c=1` throttle, sampling memory and `/scratch` use |
| [`templates_test.go`](templates_test.go) | Static checks (stdlib only) on these templates and on `scripts/github-app-manifest.json` |

## Running the PoC: a push-triggered throwaway branch

`workflow_dispatch` only offers workflows whose file is on the default branch,
and a fork's main must mirror upstream exactly. So the PoC (P6) runs from a
throwaway branch instead, which is how the P6 PoC ran:

```bash
git switch -c cr-ci-poc-1 <a commit that contains cr-ci/>
cp cr-ci/workflows/cr-runner-poc.yml .github/workflows/cr-ci-poc.yml
git add .github/workflows/cr-ci-poc.yml && git commit -m "cr-ci PoC (throwaway; delete after the run)"
git push origin cr-ci-poc-1             # the push triggers the run
gh run list --repo REPO --branch cr-ci-poc-1 --limit 1
```

- The template triggers on `push` to `cr-ci-poc-*` branches; the inputs fall
  back to their defaults on push. Pushing needs write access, the same trust as
  a dispatch. The mediator treats a same-repo push as allowed (fork guard).
- Pushing a branch that adds a workflow file needs a token with the
  `workflows` permission (or the classic `workflow` scope).
- Afterwards, delete the branch (`git push origin --delete cr-ci-poc-1`) and any
  cache the run created (`gh cache list --repo REPO --ref refs/heads/cr-ci-poc-1`,
  then `gh cache delete <id> --repo REPO`).
- Other one-off checks (for example poc-checklist E1) use the same pattern,
  with `on: push: branches: [cr-ci-poc-<name>]`.

## The routing expression

```yaml
runs-on: ${{ (github.event_name == 'push' || (github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository)) && vars.SCION_LARGE_RUNNER || 'ubuntu-latest' }}
```

| Case | Runner |
| :--- | :--- |
| `SCION_LARGE_RUNNER` unset (every repo before rollout, every unconfigured fork) | `ubuntu-latest` (`vars.*` is then `''`, which is falsy) |
| Push, or pull request from a branch of the same repository, with the variable set | the variable's value (`RUNNER_LABEL`) |
| Pull request from a fork (head repository differs, or was deleted) | `ubuntu-latest` |
| Any other event (`pull_request_target`, `workflow_run`, `schedule`, `workflow_dispatch`, `merge_group`, ...) | `ubuntu-latest` |

**Where this differs from the design.** The design's expression
(`github.event_name != 'pull_request' || ...`) routes *every* event other
than `pull_request`, including `pull_request_target` and `workflow_run`. The
mediator refuses runners to exactly those two events, so a job of a workflow
triggered by them would sit queued on the label for up to 24 hours. Routing
only an allow-list (push and same-repo pull requests, which are `ci.yml`'s
two triggers) fails safe for any other trigger. Extend the allow-list
deliberately, for example with `workflow_dispatch`, if another workflow needs
it.

**Labels.** The expression yields a single label. Keep it that way. The
mediator serves only jobs whose labels are all `RUNNER_LABEL`; anything else
(`[self-hosted, scion-cr-8c32g]`) is logged as `labels_unservable` and never
gets a runner ([architecture](../docs/architecture.md#flow), "Labels").

## Routing in `.github/workflows`

The routing lives in the workflows themselves, not in a template: the
primary-runner work (PR-wf1, phase A; PR-wf2, phase B) gives each routed job
the expression above as its `runs-on`. PR-wf1 also adds
`hack/check-runner-routing.sh` (its own job in `ci.yml`, `runner-routing-check`),
which keeps every routed `runs-on` byte-identical to it and keeps jobs that must
not run on cr-ci off it, and a cache-parity check in
`.github/actions/setup-go-cache` for routed jobs (cr-ci runners only). Until
those PRs land, nothing in `.github/workflows` routes to cr-ci. Nothing is routed until `SCION_LARGE_RUNNER` is set on
a repository
([github-setup.md, step 8](../docs/github-setup.md#8-route-jobs-the-actions-variable-scion_large_runner-g10)).
The design (which jobs, which phase, capacity) is in the exchange bucket,
`cr-ci/primary/design.md`.

The earlier `ci-lint-routing.snippet.yml` template (P7, lint only) is gone: it
also lifted the lint step's memory throttles on the cr-ci runner, and the lint
job no longer has those since #4061 (`hack/golangci-lint-split.sh`).

**Only host-process jobs may be routed.** Cloud Run cannot run Docker, so
jobs with `services:` (the Postgres jobs) or Docker steps must stay on
`ubuntu-latest` ([architecture: constraints](../docs/architecture.md#constraints)).
`hack/check-runner-routing.sh` enforces this for every job whose `runs-on` reads
the variable (`grep -n SCION_LARGE_RUNNER .github/workflows/*.yml` lists them).

### Things P6 must look at before P7

- **golangci-lint binary.** `ci.yml`'s lint job runs golangci-lint v2.15.0
  built with the go-tools#1839 patch by `cr-ci/runner/golangci-lint/build.sh`:
  it restores the binary from its own Actions cache entry (key
  `golangci-lint-patched-<runner.environment>-<os>-<arch>-go<version>-<hash of cr-ci/runner/golangci-lint/**>`,
  path `$RUNNER_TEMP/golangci-lint-patched`), or builds it, and puts it first on
  `PATH`; `golangci-lint-action` then only restores the analysis cache
  (`install-mode: none`). A routed `lint` job does the same on the cr-ci runner
  (`RUNNER_TEMP` is `/home/runner/work/_temp` on both runner types, but the key
  carries `runner.environment`, so each runner type has its own entry; see "Cache
  scope") and uses that binary, not the image's. The image ships the same
  build (`runner/Dockerfile`, `golangci-lint` stage) for the PoC template and
  anything else that calls `golangci-lint` directly. One set of pins
  (`cr-ci/runner/golangci-lint/VERSIONS`) serves all of them.
- **Caches under `$HOME` go to `/scratch`, in ubuntu-latest's layout.** See
  "Cache sharing" below. The runner entrypoint sets this up; the first step of
  both templates only checks it.

### Tuning (P6b results)

Measured on 2026-10-22 in us-west2 (8 vCPU / 32 GiB, 23 GiB `/scratch`, warm Go
and golangci-lint caches restored as in CI; `tuning-results.md` in the cr-ci
exchange bucket, `p6/`). Five runs of the same lint:

| conc / GOGC / GOMEMLIMIT | 4 / 25 / 24GiB | 8 / 25 | 8 / 50 | 8 / 100 | 8 / 100 (repeat) |
| :--- | :--- | :--- | :--- | :--- | :--- |
| golangci-lint wall time | 1036 s | 978 s | 843 s | **772 s** | **796 s** |

- **Used by the PoC template** (its defaults): `--concurrency=8`, `GOGC=100`,
  `GOMEMLIMIT=24GiB`, no `-c=1`, build cache on disk. (The live lint job no
  longer has these knobs: since #4061 it runs `hack/golangci-lint-split.sh`
  with the patched golangci-lint.)
- **Memory:** 25.5 GB peak sampled, 6.6 GB still available; GOMEMLIMIT is what
  bounds pkg/hub's compile. No guard trip. GOGC above 100 or `off` was not tested.
- **Disk:** `/scratch` peaked at 15.8-16.5 GB. An on-disk build cache needs at
  least 20 GiB of `RUNNER_DISK_GI`; the RAM build cache is not needed then.
- **CPU and I/O:** iowait about 0 and system time 5-10 % under gVisor; 8 of 8
  cores busy while 8 compiles run. What remains is a single-threaded analysis
  tail of 280-320 s in every run (about 38 % of the lint), which no knob changed,
  and pkg/hub's single compile (170-210 s).
- **Queue to start:** 17-18 s warm (sandbox 5-7 s, runner connect about 5 s). The
  first execution after an image push adds a ~15 s import (`--warm-up` takes it).
  GitHub's webhook delivery can itself lag (101 s once in P6b); cr-ci cannot
  control that, and only the reconcile backfill covers it.
- **Against CI today:** `ci.yml`'s comment still cites a warm lint of 584-776 s.
  On 2026-10-21/22 the warm lint step took 1506-1802 s on 15 PR runs and
  1310-1690 s on 8 `main` runs, and the 16 GB runner got down to 137 MB
  available: the ubuntu-latest lint is memory-starved. cr-ci is about 2x faster
  than CI today.

### Cache sharing

`actions/cache` (behind `setup-go-cache`, `save-go-cache` and
golangci-lint-action) has to agree with ubuntu-latest on three things:

1. **The key.** It agrees already: setup-go-cache builds it from
   `RUNNER_OS`, `RUNNER_ARCH`, the Go version, the workload, `go.sum` and the
   date.
2. **The version**, a hash of the cache path strings and the compression
   method. It needs `HOME=/home/runner`, `GOCACHE=$HOME/.cache/go-build`,
   `GOMODCACHE=$HOME/go/pkg/mod` and zstd.
3. **The paths inside the archive.** actions/cache stores each path relative
   to `GITHUB_WORKSPACE` and extracts with `tar -C "$GITHUB_WORKSPACE"`. On
   ubuntu-latest the workspace is `/home/runner/work/scion/scion`, so the Go
   build cache is stored as `../../../.cache/go-build`. The cr-ci runner
   must therefore use the same work folder, `/home/runner/work`, or the
   names differ. And because the kernel resolves `..` **physically** while
   the work folder is a link to `/scratch/_work`, `../../../.cache` lands in
   `/scratch/.cache`. So `/scratch` must mirror `$HOME`:
   `~/.cache -> /scratch/.cache` and `~/go -> /scratch/go`. With
   `/scratch/cache`, entries would extract next to Go's cache, the restore
   would report a hit, and the lint would still run cold.

On the cr-ci runner this is set up before any step runs, as agreed in
`gs://scion-xproject-exchange/cr-ci/cache-parity.md`:

- the image: `HOME=/home/runner`, uid 1001, zstd;
- the mediator: `work_folder` `/home/runner/work` in `generate-jitconfig`;
- the entrypoint: `/home/runner/work -> /scratch/_work`,
  `~/.cache -> /scratch/.cache` and `~/go -> /scratch/go` (the parents, so
  tar never replaces a link), and `GOCACHE`, `GOMODCACHE` and `GOPATH` under
  `$HOME`.

Parity therefore holds for any routed workflow. The templates' first step,
"Check cr-ci cache parity", creates nothing: it fails the job if `HOME`, the
links, the work folder, `go env GOCACHE`/`GOMODCACHE`, `GITHUB_WORKSPACE` or
zstd are wrong.

| | Result |
| :--- | :--- |
| **Parity holds** (the check passes) | **Shared key and paths.** A routed job restores the entries ubuntu-latest saved, and the entries it saves on `main` restore unchanged on a fallback ubuntu-latest run. One set of entries; quota unchanged. The Go build cache's *contents* still differ by runner type (cgo; see "Cache scope"), so a restore is warm only for the type that saved it. |
| Parity broken (the check fails) | **Separate.** Every restore misses or extracts beside the cache (a cold lint; ci.yml's comments cite 818-1063 s cold against 584-776 s warm, and the warm lint took 1310-1802 s on ubuntu-latest in October 2026, see "Tuning"). Routed `main` runs save a second set (about 4.2 GB of Go build cache plus module zips and the golangci-lint analysis cache) inside the repository's 10 GB Actions cache quota, which raises eviction pressure on every other workload. |

**Cache scope (ptone/scion#5979).** Routed jobs on pushes to `main` save caches in
`main`'s scope, and every later run on either runner type restores them. Decision:
- **The patched golangci-lint binary is scoped by runner type.** Its key includes
  `runner.environment` (`github-hosted` or `self-hosted`): a job executes the binary
  it restores, so each runner type only runs binaries it built itself. Only the type
  that runs lint on main pushes saves an entry: on a fork with `SCION_LARGE_RUNNER`
  set that is `self-hosted`, so a hosted lint run there (a fork pull request) builds
  the binary each time (about 50 s); upstream, with the variable unset, only
  `github-hosted` exists. Harmless either way.
- **The Go build and module caches and golangci-lint's analysis cache keep one key
  for both runner types, but the build cache is warm only for the type that saved
  it (decision: document this, cr-ci-rev-16 R1 option (a)).** The cr-ci image has no
  C compiler, so Go there defaults to `CGO_ENABLED=0`, while ubuntu-latest builds
  with cgo on. With cgo off, `net` and `os/user` compile other files, and every
  package that imports them, which is nearly all of scion and most dependencies,
  gets another build-cache key. Path parity holds; content parity does not, beyond
  the standard library and dependencies below `net`. The analysis cache splits the
  same way; module zips do not.
  - On a fork with the variable set, the routed main push is the only writer, so
    a workload's cache holds cgo-off entries: warm for cr-ci, effectively cold for
    a hosted run of the same workload (a fork pull request, a run with the variable
    unset, or a job moved back after a problem). Accepted: same-repo pull requests,
    which run on cr-ci, are nearly all of the fork's.
  - The first routed main push of a workload restores the hosted (cgo-on) cache,
    misses on almost all of it, and `save-go-cache` then deletes every entry it did
    not use ("trim"), so from that day on the key holds a cgo-off cache.
  - Follow-up for the owner (option (b)): set `CGO_ENABLED: "0"` for the routed Go
    jobs on both runner types, which restores true parity. Release builds are
    already cgo-off, but it changes what hosted runs test (the pure-Go resolver,
    `os/user`), so it needs the owner's decision. Adding gcc to the image is not an
    option: it widens what a job can do there.
- **Who may write these caches.** Only pushes to `main` save them; pull requests
  restore but never save, and fork pull requests never run on cr-ci. Module zips are
  verified against `go.sum`; build-cache and analysis entries are not. Routing adds
  the cr-ci execution environment (runner image, mediator, GCP project) to the
  trusted writers of `main`'s caches, whose build-cache objects can be linked into
  binaries a hosted run executes. That is acceptable: those are trusted main-push
  jobs, cr-ci already gates merges through the jobs it runs, and per the cgo split
  above little is reused across runner types in practice.

**`GITHUB_WORKSPACE` stays `/home/runner/work/scion/scion`.** In actions/runner
v2.338.0 the work directory is `Path.GetFullPath(Path.Combine(root,
WorkFolder))`, and the workspace paths are `Path.Combine` of it.
`GetFullPath` is lexical and does not resolve symlinks (read from the source
by cr-ci-dev-2). If it ever canonicalised to `/scratch/_work/...`, the
archive names would differ again. That is unverified at runtime until P6;
the parity check fails the job in that case, and the PoC's "Runner facts"
step prints both the reported and the physical path.

**P6 checkpoints for cache sharing** (runtime assumptions no step proves):

- `GITHUB_WORKSPACE` is reported lexically (`/home/runner/work/...`). The
  parity step fails the job if it is not.
- When cr-ci *saves*, actions/cache (through `@actions/glob`) archives the
  logical `$HOME/...` paths, not their `/scratch/...` realpaths. The source
  says so, but nothing checks it at run time. Confirm it once: let a routed
  `main` run on cr-ci save the lint cache under a fresh key, then restore
  that key on ubuntu-latest and check that the entries land in
  `/home/runner/.cache/go-build`.

## Fork CI gate

Forks of `GoogleCloudPlatform/scion` (such as `ptone/scion`) are for staging
pull requests, so a fork does not run CI on `push` or `schedule`. Every job in
a workflow triggered by a branch push or a schedule starts its job-level `if:`
with the same gate:

```yaml
if: ${{ (github.event_name != 'push' && github.event_name != 'schedule') || github.repository == 'GoogleCloudPlatform/scion' }}
```

A job that already had an `if:` keeps it, ANDed after the gate:
`${{ (<gate>) && (<old condition>) }}`. Upstream, the gate is always true, so
push and schedule runs are unchanged. `pull_request`, `pull_request_target`
and `workflow_dispatch` runs are unchanged everywhere, the fork included. On
a fork push or schedule every job is skipped, including the `always()`
summary jobs, so nothing turns red and no check is left pending. Jobs that
already carry the stricter upstream-only guard
(`if: github.repository == 'GoogleCloudPlatform/scion'`, in
`build-nightly.yml` and `build-preview-weekly.yml`) keep it as it is. No
variable or other fork-side setting is involved.

As a result, the routing expression's `push` branch routes nothing for now.
Upstream, `SCION_LARGE_RUNNER` is unset, and on a fork the push run is
skipped. On a fork, cr-ci serves only same-repository pull requests.

To run push CI on a fork anyway, start the workflow with `workflow_dispatch`
where it has that trigger (`docs.yml`, `full-test-suite.yml`, `web-e2e.yml`,
`build-nightly.yml`, `build-preview-weekly.yml`, `race-detection.yml`), or
edit the gate in that fork's copy of the workflow.

`hack/check-fork-ci-gate.sh` (the `fork-ci-gate-check` job in `ci.yml`, with
fixtures in `hack/testdata/fork-ci-gate/`) fails when such a job lacks the
gate. It also fails when the gate is weakened, for example by an `||` outside the
job's own condition. It skips push triggers that filter only tags. It has no
YAML parser and reads only the 2-space block style used here. In every
workflow except `build-release.yml` (skipped by name, layout rules included),
it reports `unrecognised layout` (exit 1) for:

- an inline `on:` value that is not a bare event or a one-line list of bare
  events (for example a flow map, a quoted event, or a list over several lines);
- any line under a block `on:` that is not one of: a 2-space `- event` entry,
  a 2-space `event:` key, that key's settings indented by 4 or more spaces, a
  comment, or a blank line. This covers entries at column 0, a list or scalar
  on the next line, and 4-space entries;
- an `on:` with no entry (a block `on:` holding only comments or blank lines,
  or `on: []`);
- no top-level `on:` or `jobs:` key it recognises (for example a quoted
  `"jobs":` or `jobs :`);
- an inline `jobs:` value, a job key that is not an unquoted 2-space key, or a
  `jobs:` with no job.

Keep each `if:` on one line. A block-scalar `if:` is reported, and a trailing
comment with no quotes in it is ignored.

`build-release.yml` (tag pushes) is not gated yet; its jobs carry the
upstream-only repository guard instead.

## PoC success criteria

These are part of the full P6 list in
[docs/poc-checklist.md](../docs/poc-checklist.md) (items D1-D8, E1).

From design section 7, phase 1. Read them from the PoC run's log, the
mediator log and the execution log
([github-setup.md, step 9](../docs/github-setup.md#9-verify-a-routed-job)):

- Queue to first step in under 20 s. (P6b in us-west2: 17-18 s warm, the sandbox
  taking 5-7 s. P6 in us-central1 measured 40-67 s, the sandbox alone 18-47 s.
  The first execution after an image change adds an image import, 15-34 s;
  `deploy-job.sh --warm-up` removes it. GitHub's webhook delivery can lag on its
  own, 101 s once in P6b.)
- The Go cache is warm: the "Check that the Go cache is warm" step prints
  `go cache warm: yes`, based on the file count of `$(go env GOCACHE)` after
  the restore, not only the matched key.
- `golangci-lint` finishes in under 14 minutes (the step prints its
  duration). Compare it with CI's warm lint on ubuntu-latest: 1310-1802 s in
  October 2026 (ci.yml's comment still says 584-776 s). P6 measured 1026 s with
  the build cache in RAM at concurrency 4; P6b 772-796 s at concurrency 8, GOGC
  100, on disk.
- Memory: `memory.peak` is not available under gVisor, so read the lint step's
  `sample peaks:` line (`MemTotal - MemAvailable`, which includes RAM-held
  files). P6 measured a compile peak near 21 GB, and 27.9 GB with the build
  cache in RAM, of 32 GiB.
- Disk: the caches (8-9.5 GB of Go build cache during the lint, plus the
  workspace, modules and temp dirs; about 15-20 GiB in total) fit on `/scratch`.
  That needs `RUNNER_DISK_GI` of at least 20 after the per-instance quota increase
  (P6b: peak 16.5 GB on 23 GiB, the us-west2 limit;
  [gcp-setup, "Quotas"](../docs/gcp-setup.md#9-quotas)), or
  `RUNNER_BUILD_CACHE_IN_RAM=true`. With the default 10 GiB the lint runs out of
  space.
- The runner deregisters itself, and the execution ends (and stops billing)
  when the job ends.
- The runner checkpoints in [runner/README.md](../runner/README.md#runtime-contract)
  hold: the `/scratch` mount check matches, and the claim succeeds on
  attempt 1.

## Static checks

`go test ./workflows/` (from `cr-ci/`) checks, without a YAML library:

- Every copy of the routing expression in `docs/architecture.md`,
  `docs/github-setup.md` and this README is exactly the canonical expression
  (the live workflows are checked by `hack/check-runner-routing.sh`, added by PR-wf1).
- The expression keeps the fork guard and the `ubuntu-latest` fallback.
- The PoC triggers on `push` to `cr-ci-poc-*` branches and on
  `workflow_dispatch`, and runs on the label alone. Its default label (input
  and push fallback) equals `RUNNER_LABEL`'s default in `config.example.env`.
  It contains no `-c=1` throttle, samples memory and `/scratch` during the lint,
  and reports `df -h /scratch`.
- The PoC template starts with the self-hosted-only, verify-only cache parity
  step (`HOME`, the work folder and cache links, `go env` paths,
  `GITHUB_WORKSPACE`, zstd). It creates no links and writes no `GITHUB_ENV`.
- The PoC judges warmth by the contents of `$(go env GOCACHE)`.
- Every `uses:` is pinned to a full commit SHA. No tabs.
- `scripts/github-app-manifest.json` requests exactly Actions read,
  Administration write and Metadata read, and the `workflow_job` event. The
  matrix's `repo`-scope permission line still says the same.
