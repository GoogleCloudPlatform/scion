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
2. Add the snippet's first step, "Keep caches under HOME on /scratch (cr-ci
   runner only)", as the job's first step. It runs only when
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
- **Caches under `$HOME` go to `/scratch`, with ubuntu-latest's paths.**
  On a cr-ci runner, `$HOME` (`/home/runner`, as on ubuntu-latest) is on
  Cloud Run's in-memory root filesystem. The first step of both templates
  (cr-ci runner only) links `~/.cache` to `/scratch/cache` (the
  entrypoint's `XDG_CACHE_HOME`) and `~/go` to `/scratch/gopath` (its
  `GOPATH`). It then sets `GOCACHE=$HOME/.cache/go-build` and
  `GOMODCACHE=$HOME/go/pkg/mod` in `GITHUB_ENV`, overriding the entrypoint's
  `/scratch/...` values. `go env` and golangci-lint-action (which derives
  `~/.cache/golangci-lint` from `$HOME`) therefore report exactly the paths
  they report on ubuntu-latest, while the bytes land on the disk. The
  *parent* directories are linked: tar replaces a symlink that sits at an
  archived path, but follows one in a parent component. The PoC report shows
  where the links point and how much of `$HOME` stayed on the root
  filesystem (`du -shx ~`).

### Cache sharing

`actions/cache` finds an entry by its key **and** a version, which is a
hash of the cached path strings and the compression method. The keys
already agree: setup-go-cache builds them from `RUNNER_OS`, `RUNNER_ARCH`,
the Go version, the workload name, `go.sum` and the date, and these are the
same on both runners. So the version decides:

| | Paths equal | Compression equal | Result |
| :--- | :--- | :--- | :--- |
| **cr-ci as designed** (first step above, zstd in the image, HOME `/home/runner`) | yes | yes (zstd) | **Shared.** A routed job restores the caches ubuntu-latest saved, and what it saves on `main` is the same entry a fallback run on ubuntu-latest restores. One set of entries; quota unchanged. |
| Without the first step, without zstd, or with another HOME | no | or no | **Separate.** Every restore misses (a cold lint, about 818-1063 s against 584-776 s warm, per ci.yml's comments), and routed `main` runs save a second set: about 4.2 GB of Go build cache plus the module zips and the golangci-lint analysis cache, inside the repository's 10 GB Actions cache quota. That raises eviction pressure on every other workload. |

The first case applies: the templates set the paths, and the runner image
installs zstd. If a restore misses anyway (the PoC prints
`go cache restored: NONE`), check zstd in the image and `HOME=/home/runner`
first.

## PoC success criteria

From design section 7, phase 1. Read them from the PoC run's log, the
mediator log and the execution log
([github-setup.md, step 9](../docs/github-setup.md#9-verify-a-routed-job)):

- Queue to first step in under 20 s.
- The Go cache restore hits (`go cache restored: <key>`), so the lint is
  warm, like CI's.
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
- Both templates start with the self-hosted-only step that links `~/.cache`
  and `~/go` to `/scratch` and sets `GOCACHE`/`GOMODCACHE` to
  ubuntu-latest's paths in `GITHUB_ENV`.
- The snippet sets `GOMEMLIMIT`, `GOFLAGS` and `LINT_CONCURRENCY` per runner
  type.
- Every `uses:` is pinned to a full commit SHA. No tabs.
- `scripts/github-app-manifest.json` requests exactly Actions read,
  Administration write and Metadata read, and the `workflow_job` event. The
  matrix's `repo`-scope permission line still says the same.
