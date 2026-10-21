# cr-ci workflow templates

These files are **templates, not active workflows**. GitHub only runs
workflows under `.github/workflows/`, so the copies here never run. Putting
them there is a separate, reviewed step that needs ptone's go-ahead: the PoC
is P6, and the `ci.yml` change is P7.

| File | What it is |
| :--- | :--- |
| [`cr-runner-poc.yml`](cr-runner-poc.yml) | PoC workflow (`workflow_dispatch`): runs `golangci-lint` on a cr-ci runner without the `-c=1` throttle, then reports peak memory and `/scratch` use |
| [`ci-lint-routing.snippet.yml`](ci-lint-routing.snippet.yml) | The lines of `ci.yml`'s `lint` job that change: the routing `runs-on` expression and the lint step's env |
| [`templates_test.go`](templates_test.go) | Static checks (stdlib only) on these templates and on `scripts/github-app-manifest.json` |

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

## Applying the snippet to `ci.yml` (P7)

P7 is a separate pull request outside `cr-ci/`, and it needs ptone's word. It
changes only the `lint` job of `.github/workflows/ci.yml`:

1. Replace `runs-on: ubuntu-latest` with the expression above.
2. Add the snippet's first step, "Check cr-ci cache parity (cr-ci runner
   only)", as the job's first step. It runs only when
   `runner.environment == 'self-hosted'`.
3. In the **golangci-lint** step, replace the `env:` block with the one in
   the snippet. The throttles stay on GitHub-hosted runners and are lifted
   only when `runner.environment == 'self-hosted'`.
4. In the same step, change `--concurrency=2` to `--concurrency="$LINT_CONCURRENCY"`.
5. Update the step's comment block to say that the throttles apply to the
   16 GB runner only.

The change is inert until `SCION_LARGE_RUNNER` is set on a repository
([github-setup.md, step 8](../docs/github-setup.md#8-route-jobs-the-actions-variable-scion_large_runner-g10)).
The P7 PR therefore needs no coordination with the rollout, and upstream gets
it through the normal sync.

**Only host-process jobs may be routed.** Cloud Run cannot run Docker, so
jobs with `services:` (the Postgres jobs) or Docker steps must stay on
`ubuntu-latest` ([architecture: constraints](../docs/architecture.md#constraints)).
v1 routes `lint` only.

### Things P6 must look at before P7

- **golangci-lint version.** `ci.yml` installs golangci-lint v2.15.0 through
  `golangci-lint-action`, and the runner image pins the same version
  (`GOLANGCI_LINT_VERSION` in `runner/Dockerfile`). The routed `lint` job
  still installs it through the action, and the PoC uses the image's
  pre-installed binary. Bump both together: the image pin is the single
  place for the image, and `ci.yml`'s `version:` input for CI.
- **Caches under `$HOME` go to `/scratch`, in ubuntu-latest's layout.** See
  "Cache sharing" below. The runner entrypoint sets this up; the first step of
  both templates only checks it.

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
| **Parity holds** (the check passes) | **Shared.** A routed job restores the entries ubuntu-latest saved, and the entries it saves on `main` restore unchanged on a fallback ubuntu-latest run. One set of entries; quota unchanged. |
| Parity broken (the check fails) | **Separate.** Every restore misses or extracts beside the cache (a cold lint, about 818-1063 s against 584-776 s warm, per ci.yml's comments). Routed `main` runs save a second set (about 4.2 GB of Go build cache plus module zips and the golangci-lint analysis cache) inside the repository's 10 GB Actions cache quota, which raises eviction pressure on every other workload. |

**`GITHUB_WORKSPACE` stays `/home/runner/work/scion/scion`.** In actions/runner
v2.338.0 the work directory is `Path.GetFullPath(Path.Combine(root,
WorkFolder))`, and the workspace paths are `Path.Combine` of it.
`GetFullPath` is lexical and does not resolve symlinks (read from the source
by cr-ci-dev-2). If it ever canonicalised to `/scratch/_work/...`, the
archive names would differ again. That is unverified at runtime until P6;
the parity check fails the job in that case, and the PoC's "Runner facts"
step prints both the reported and the physical path.

## PoC success criteria

From design section 7, phase 1. Read them from the PoC run's log, the
mediator log and the execution log
([github-setup.md, step 9](../docs/github-setup.md#9-verify-a-routed-job)):

- Queue to first step in under 20 s.
- The Go cache is warm: the "Check that the Go cache is warm" step prints
  `go cache warm: yes`, based on the file count of `$(go env GOCACHE)` after
  the restore, not only the matched key.
- `golangci-lint` finishes in under 14 minutes (the step prints its
  duration). Compare it with CI's warm lint (584-776 s on ubuntu-latest).
- `memory.peak` at about 50% of 32 GiB (about 15.5 to 17 GiB), and `/scratch`
  takes the 5 to 8 GiB of caches instead of RAM.
- The runner deregisters itself, and the execution ends (and stops billing)
  when the job ends.
- The runner checkpoints in [runner/README.md](../runner/README.md#runtime-contract)
  hold: the `/scratch` mount check matches, and the claim succeeds on
  attempt 1.

## Static checks

`go test ./workflows/` (from `cr-ci/`) checks, without a YAML library:

- The snippet's `runs-on` line is exactly the routing expression, and so is
  every copy of it in `docs/architecture.md`, `docs/github-setup.md` and this
  README.
- The expression keeps the fork guard and the `ubuntu-latest` fallback.
- The PoC is `workflow_dispatch` only, and runs on the label alone. Its
  default label equals `RUNNER_LABEL`'s default in `config.example.env`.
  It contains no `-c=1` throttle, and it reports `memory.peak` and
  `df -h /scratch`.
- Both templates start with the self-hosted-only, verify-only cache parity
  step (`HOME`, the work folder and cache links, `go env` paths,
  `GITHUB_WORKSPACE`, zstd). It creates no links and writes no `GITHUB_ENV`.
- The PoC judges warmth by the contents of `$(go env GOCACHE)`.
- The snippet sets `GOMEMLIMIT`, `GOFLAGS` and `LINT_CONCURRENCY` per runner
  type.
- Every `uses:` is pinned to a full commit SHA. No tabs.
- `scripts/github-app-manifest.json` requests exactly Actions read,
  Administration write and Metadata read, and the `workflow_job` event. The
  matrix's `repo`-scope permission line still says the same.
