# cr-ci runner image

Each Cloud Run Job execution of `RUNNER_JOB_NAME` runs this image as one
ephemeral GitHub Actions JIT runner. That runner takes one job and then exits.

| File | Purpose |
| :--- | :--- |
| [`Dockerfile`](Dockerfile) | Image: Ubuntu 24.04, GitHub Actions runner, Go, golangci-lint, git (built from pinned source, >= 2.48), jq, curl, zstd, ca-certificates, and the tools routed CI jobs expect preinstalled (make, procps, python3 + python3-yaml, ripgrep, tmux, shellcheck 0.9.0, openssh-client); non-root `runner` user (uid 1001, `HOME=/home/runner`) |
| [`entrypoint.sh`](entrypoint.sh) | PID 1: checks the volume at HOME, creates the work and cache directories on it, does the claim exchange, then runs `./run.sh --jitconfig` as a child, forwards signals and exits with the runner's real exit code |
| [`entrypoint_test.go`](entrypoint_test.go) | Offline harness: runs `entrypoint.sh` against a fake claim server on loopback with a stub `run.sh` |

## Build arguments

Every tool version and checksum is pinned in **one place only**: the `ARG`
block at the top of the [`Dockerfile`](Dockerfile). This page does not repeat
the values. Read them there.

| Build arg | Meaning | Where to find the value when bumping it |
| :--- | :--- | :--- |
| `UBUNTU_IMAGE` | Base image, `ubuntu:24.04@sha256:<index digest>` | Docker Hub `library/ubuntu` tag `24.04`, top-level (multi-arch index) digest |
| `ACTIONS_RUNNER_VERSION` / `ACTIONS_RUNNER_SHA256` | GitHub Actions runner, `actions-runner-linux-x64-<v>.tar.gz` | The release notes of <https://github.com/actions/runner/releases> list the SHA-256 of each tarball |
| `GO_VERSION` / `GO_SHA256` | Go toolchain, `go<v>.linux-amd64.tar.gz` | <https://go.dev/dl/> (keep it in step with the repo's `go.mod`) |
| `GIT_VERSION` / `GIT_SHA256` | git, `git-<v>.tar.xz`, built from source in the `git` stage (gcc only there) | `sha256sums.asc` in <https://mirrors.edge.kernel.org/pub/software/scm/git/>; also check the tarball against Junio C Hamano's `git-<v>.tar.sign` (key `96E07AF25771955980DAD10020D04E5A713660A7`). Must be 2.48 or newer: the build asserts it. git is built with `NO_RUST=YesPlease` (2.55+ builds Rust parts by default, which needs cargo); git 3.0 makes Rust mandatory, so a bump to 3.x must revisit the git stage |

golangci-lint is not a download: the `golangci-lint` stage builds it with
[`golangci-lint/build.sh`](golangci-lint/README.md), the same script, pins
(`golangci-lint/VERSIONS`: module checksums and the patch sha256) and go-tools#1839
patch that CI's lint job uses, so the image's binary is the one CI runs
(`golangci-lint version` reports `2.15.0-scion-gotools1839`). The stage builds with
the image's Go and fetches source from the Go module proxy, so the image build
needs network access to proxy.golang.org (Cloud Build has it). Bump it in
`golangci-lint/` (its README), not here.

Every download is checked with `sha256sum -c`, and a mismatch fails the build.
To override a pin, pass the version **and** its checksum with `--build-arg`,
for example `--build-arg GO_VERSION=<v> --build-arg GO_SHA256=<sha>`. jq, curl,
zstd and the runner's .NET dependencies (installed by the runner's own
`bin/installdependencies.sh`) come from the Ubuntu archive, and apt verifies
their signatures.

**git from source.** The repository's tests need git 2.48 or newer (worktree
`--relative-paths`); Ubuntu 24.04 ships 2.43, which fails 11+ test call sites and
silently skips 65 more. The `git` stage builds the pinned release tarball, sha256-checked
like the other downloads, and the final stage copies the installed tree and asserts
`git --version` >= 2.48. This pins more strongly than the git-core PPA: a PPA publishes
only its current build, so a pinned package version disappears and rebuilds then fail or
drift, while the tarball and its sha are permanent. gcc and the -dev libraries stay in the
build stage; the runner image has no compiler.

**Cache parity with ubuntu-latest.** The contract is
`gs://scion-xproject-exchange/cr-ci/cache-parity.md`. `actions/cache` (and
`setup-go-cache`) derives a cache's version from its paths and its
compression method. It stores entries relative to `GITHUB_WORKSPACE` and
extracts them with `tar -P -C "$GITHUB_WORKSPACE"`. A cache entry is
therefore shared in both directions only if all of these hold:
- **zstd:** the image has `zstd` on `PATH`, so `actions/cache` picks zstd, as
  on ubuntu-latest.
- **`HOME=/home/runner` and uid 1001:** both match ubuntu-latest. `HOME` is
  set in the image config, and the entrypoint refuses any other `HOME`
  (exit 2).
- **Work folder `/home/runner/work`:** the mediator's JIT `work_folder`, so
  `GITHUB_WORKSPACE` reads `/home/runner/work/<repo>/<repo>`, as on
  ubuntu-latest.
- **Go paths as on ubuntu-latest:** the entrypoint exports
  `GOCACHE=$HOME/.cache/go-build`, `GOMODCACHE=$HOME/go/pkg/mod`,
  `GOPATH=$HOME/go` and `GOLANGCI_LINT_CACHE=$HOME/.cache/golangci-lint`, and
  leaves `XDG_CACHE_HOME` unset.
- **Bytes on the disk:** the Cloud Run Job mounts its ephemeral-disk volume at
  HOME (`/home/runner`), and the entrypoint creates `~/work`, `~/.cache`, `~/go`
  and `~/.npm` as **real directories** on it (step 3). There are no symlinks, so
  every physical path is the logical one: an entry `../../../.cache/X` from the
  workspace lands in `~/.cache/X`, and a whole-directory archive (the member
  `../../../.npm/` that actions/setup-node's npm cache produces) restores onto
  the real `~/.npm`. A symlink there would be replaced by GNU tar with a real
  directory (it does that for a directory member unless `--keep-directory-symlink`,
  which actions/cache does not pass), and would also make an action's
  `process.cwd()` differ from `GITHUB_WORKSPACE` (actions/setup-node's lock-file
  hashing then finds nothing). `TestEntrypointCacheTarRoundTrip` and
  `TestEntrypointCacheTarWholeDirectory` reproduce the round trips.
  (Runner images before cr-ci FIXUP-5 mounted the volume at `/scratch` and
  linked these paths into it; see docs/operations.md, "Runner layout switch".)

The workflow templates' self-hosted step only verifies this layout; it
creates nothing. Parity holds for any routed workflow.

The image is built for linux/amd64 only. The build context is this directory:
`docker build -t <ref> cr-ci/runner`. `scripts/build-runner-image.sh` (P4)
builds it with Cloud Build and pushes it to Artifact Registry.

**Keep the runner current.** GitHub stops sending jobs to runner versions it
considers too old, and an outdated runner may update itself at start-up, which
costs time on every execution. Bump `ACTIONS_RUNNER_VERSION` soon after each
runner release.

## Runtime contract

The mediator sets these env overrides on each execution (see
[`docs/architecture.md`](../docs/architecture.md)):

| Env | Value |
| :--- | :--- |
| `RUNNER_CLAIM_TOKEN` | 32 lowercase hex chars, single use, valid for `CLAIM_TTL` |
| `MEDIATOR_URL` | https origin of the mediator, no trailing slash |
| `GH_JOB_ID` | numeric GitHub job ID (used in logs only) |
| `RUNNER_NAME` | normally `cr-job-<GH_JOB_ID>` (used in logs only). After a runner swap or hijack recovery the mediator may reuse another job's name, so a mismatch is only a warning |
| `GH_REPOS` | comma-separated `owner/name` list (1-100) of the repositories whose jobs the runner may be handed: its registration scope. Repo scope: the one repository; org scope: every `TARGET_REPOS` entry of the owner (an org runner may take any of their jobs). The job guard refuses a job of any other repository |

The JIT config is never passed in env, args or labels. The entrypoint does
this, in order:

1. **Checks the env and the job guard.** All five variables and `HOME` must be set, and `HOME`
   must be `/home/runner`; `GH_REPOS` must be 1-100 comma-separated `owner/name` entries. The job-started hook must be
   the pinned, protected file ("Job guard", below), or it exits 2 here, in warm-up too. The token must be
   exactly 32 lowercase hex chars (the mediator uses `hex.EncodeToString`;
   uppercase is rejected). `MEDIATOR_URL` must be an https origin; plain http is
   accepted only for `127.0.0.1` and `localhost`, which the test harness uses.
2. **Checks the volume at HOME.** HOME (`/home/runner`) must exist, be a mount
   point, and be writable. If any check fails, it exits 2 before the token is
   spent. Cloud Run's root filesystem is in memory, so a missing volume would
   silently turn the caches into RAM use. The mount check first looks for HOME
   in `/proc/self/mountinfo`. If it is not listed there, or the table cannot be
   read, the fallback accepts HOME when `stat -c %d` reports a different device
   for it than for its parent directory. The log line
   `/home/runner is a mount (<fstype>): ...` says which check matched. If the
   volume is tmpfs (in memory), it logs a warning. A job definition that still
   mounts the volume at `/scratch` (an image/job mismatch) fails here: exit 2,
   with a hint to run `deploy-job.sh`, which updates the image and the mount
   together.

   It also logs the volume root's owner and mode
   (`volume root /home/runner: owner uid N, mode M`). Cloud Run creates the
   volume "so that any user can read or write to it"; the entrypoint sets it to
   0755 only if this uid owns it, and otherwise leaves it. A world-writable HOME
   is a documented residual ([architecture](../docs/architecture.md#constraints)).
3. **Creates the work and cache directories on the volume** (see "Cache parity"
   above). Each of `~/work`, `~/.cache`, `~/go` and `~/.npm` is created as a real
   directory, or kept if it already is one; a symlink or a non-directory there
   exits 2 before the token is spent.

   | Directory | Holds |
   | :--- | :--- |
   | `~/work` | the runner work folder (`work_folder` = `/home/runner/work`) |
   | `~/.cache` | `GOCACHE`, `GOLANGCI_LINT_CACHE`, any XDG cache |
   | `~/go` | `GOPATH`, `GOMODCACHE` |
   | `~/.npm` | npm's cache (actions/setup-node's npm cache step also needs the directory to exist) |

   **RAM build cache.** With `RUNNER_BUILD_CACHE_IN_RAM=true` (set on the job by
   `deploy-job.sh` from `config.env`), `~/.cache` is instead a link to a
   directory on the in-memory root filesystem (`/tmp/cr-ci-build-cache`). Archive
   entries under `~/.cache/...` still land in RAM (tar follows a link in a leading
   component), but a whole-`~/.cache` archive would replace the link. The cache
   then counts against the memory limit
   ([architecture](../docs/architecture.md#constraints)). Any other value than
   `true` or `false` exits 2.

   It exports `GOCACHE`, `GOMODCACHE`, `GOPATH` and `GOLANGCI_LINT_CACHE` as
   on ubuntu-latest, leaves `XDG_CACHE_HOME` and `TMPDIR` unset (temp files go
   to `/tmp`, root-owned and sticky, as on ubuntu-latest; tools that check a
   temp directory's ancestors accept it), exports `GOTMPDIR=~/.cr-ci/go-tmp`
   (Go's build and test temp work stays on the volume), and exports
   `RUNNER_WORKSPACE=$HOME/work` and `RUNNER_TEMP=$HOME/work/_temp`. For job
   steps, the runner sets `RUNNER_WORKSPACE` and `RUNNER_TEMP` itself, under its
   work folder. Other temp files (Go tests' `t.TempDir()`, npm) now use `/tmp`,
   that is RAM: measure before routing temp-heavy jobs such as build-and-test.
   The runner is installed at `/opt/actions-runner`, not under HOME (the mount
   would hide it), so its `_diag` directory (its own logs, and the job's step
   logs under `_diag/pages`, which grow with the job's output: about 1 MB for
   the P6 lint job) stays on the root filesystem: a job that fills the volume
   cannot crash the runner and lose the job log with it (P6 PoC run 2). The image
   also keeps a `_work` symlink to `/home/runner/work` in the runner directory,
   for a JIT config that names the default `_work`.

   The work folder is a real directory, so `GITHUB_WORKSPACE`
   (`/home/runner/work/<repo>/<repo>`) is also the physical path, and
   `go env GOCACHE` is `/home/runner/.cache/go-build`; the setup-go-cache parity
   step checks both on every routed Go job.
4. **Claims the JIT config** (claim contract v2,
   `gs://scion-xproject-exchange/cr-ci/claim-contract-v2.md`). The claim is
   bound to the execution's workload identity, so a principal that can read
   the execution's env (`run.viewer`) cannot race the runner for the token.
   1. **Gets an ID token.** It sends `GET
      http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=<MEDIATOR_URL>&format=full`
      with `Metadata-Flavor: Google`, with no proxy and `MEDIATOR_URL` URL-encoded.
      Fetching its own identity token needs no IAM role. Trailing CR/LF is
      stripped, then only a JWT-shaped response (three base64url segments)
      is accepted. The token is fetched
      once and reused for claim retries.
   2. **Claims.** It sends `POST ${MEDIATOR_URL}/internal/claim` with
      `Authorization: Bearer <ID token>`, `X-Runner-Claim: <RUNNER_CLAIM_TOKEN>`
      and an empty body. The mediator checks the identity
      (`EXEC_SA_NAME@GCP_PROJECT`, `aud` = `MEDIATOR_URL`) before it looks up
      the claim, so a bad identity does not consume the claim.

   **P6 PoC checkpoint:** the real metadata server has not been called yet.
   Confirm on Cloud Run that the identity response is a bare JWT (possibly
   with a trailing line ending, which is stripped), that the mediator accepts
   it (no 401), and that the log shows `claim succeeded on attempt 1`. If the
   body format differs, every execution exits 3 with "metadata server
   returned 200 without an ID token".

   Metadata and claim failures share one budget of 5 attempts:

   | Response | Action |
   | :--- | :--- |
   | claim 200 with `application/json` and a non-empty `jit_config` string | continue |
   | metadata or claim 5xx, or connection error/timeout | retry after 1, 2, 4, then 8 s; after the 5th failed attempt, exit 3 |
   | claim 401 (identity rejected) | exit 3 at once |
   | claim 410 (claim missing, malformed, unknown, expired or used) | exit 3 at once |
   | metadata: any other status, or 200 without a JWT | exit 3 at once |
   | claim: anything else (other status, wrong content type, bad JSON) | exit 3 at once; the claim may already be spent |

5. **Runs the runner and reports its real exit code.** It unsets
   `RUNNER_CLAIM_TOKEN` and starts `./run.sh --jitconfig <config>` in
   `/opt/actions-runner` as a **child** (not `exec`), for two reasons:
   - **Exit code.** In actions/runner (checked in v2.338.0), `run-helper.sh`
     maps every Runner.Listener exit code it does not know, an abort's 134
     included, to exit 0 after printing `Exiting with unknown error code: N`.
     `run.sh` then exits 0 too: it passes on only 2 (restart, handled inside it)
     and 7. So `run.sh`'s own exit code cannot show a crashed runner (P6 PoC run 2:
     the runner aborted with ENOSPC and the execution showed as succeeded). The
     entrypoint copies the runner's output to stdout unchanged and matches
     exactly that line (anchored, digits only). It exits with `run.sh`'s exit code
     when that is non-zero, otherwise with N, otherwise 0.
   - **Signals.** Cloud Run sends SIGTERM (CancelExecution or task timeout) to
     PID 1, the entrypoint, which forwards SIGTERM or SIGINT to `run.sh`. The image
     sets `RUNNER_MANUALLY_TRAP_SIG=1`, so `run.sh` takes its `runWithManualTrap`
     path and turns the signal into a graceful job cancel: SIGINT to the runner's
     process group, before the SIGKILL at the end of the grace period. `run.sh` is
     started under job control (`set -m`): a plain background job would inherit an
     ignored SIGINT, and the graceful cancel would silently stop working (a test
     checks the disposition). The traps are installed when the script starts.
     A SIGTERM or SIGINT that arrives before `run.sh` runs (during the checks or the
     claim) stops the script at the next checkpoint with 128+n (143 or 130), without
     starting the runner; `/reconcile` reaps the unused registration.
   - **Idle timeout.** If no `Running job:` line from the runner listener has appeared
     within `RUNNER_IDLE_TIMEOUT_SECONDS` (default 600; `deploy-job.sh` sets it from
     `RUNNER_IDLE_TIMEOUT`) of the container start, the entrypoint logs
     `idle timeout: ...`, sends SIGTERM to `run.sh` (the same graceful path as a
     cancel) and exits **0**, logging `idle_timeout: runner stopped ...`. GitHub may give
     any queued job with the label to an idle runner, and that job only gets the rest
     of the execution's task timeout; the idle timeout makes that at least
     `TASK_TIMEOUT - RUNNER_IDLE_TIMEOUT`. A job whose `Running job:` line the
     entrypoint has read before the deadline is never touched. **Remaining race:** the
     check and the SIGTERM are not atomic, so a job that GitHub assigns in the moment
     between them (or whose line is still in the pipe) is cancelled by the SIGTERM.
     The entrypoint then sees both markers, logs `ERROR idle_timeout_raced_job` and
     exits **4**, so the execution fails visibly instead of passing for an idle stop;
     re-run the cancelled job. The timer is a `read -t` on a FIFO in a subshell, killed when
     `run.sh` exits.

   The output goes through FIFOs in a private temp dir under `/tmp`, small and
   on the root filesystem, never on the volume. A watcher process that does not
   finish within 5 s of `run.sh`'s exit is abandoned, so a stray process holding
   the pipe cannot keep the execution alive.

Exit codes: the runner's (above) once `run.sh` has started, 0 for an idle stop,
**4** when an idle stop raced a job start, and **5** when the job guard refused the
assigned job (above). A 4 can also be the runner's own
code passed through; the `ERROR idle_timeout_raced_job` log line tells them apart. Before that, **2** for a configuration error (env,
`HOME`, the volume at HOME, a work or cache directory, missing `run.sh`) and **3** for a failed claim. Neither path starts
`run.sh`. If a claim is lost (for example, the mediator restarted and its
in-memory claims are gone), the runner exits 3. `/reconcile` then deletes the
orphaned `cr-job-*` registration and relaunches the job if it is still queued.

**Secrets.** Neither the claim token, the ID token nor the JIT config is
ever printed, logged or written to disk. Both request headers reach curl on
stdin, as a curl config, not in its argv. `curl -q` ignores any `~/.curlrc`,
so no config file can turn on verbose header output. Responses stay in shell
memory, and the ID token is cleared before `run.sh` starts. The script never
uses `set -x`; keep it that way. The JIT config does appear in `run.sh`'s argv,
as the runner requires.

**Trust boundary: job code can read the runner's credentials.** The entrypoint
(PID 1), `run.sh`, the runner listener and worker, and every job step run as the same
uid (`runner`, 1001); there is no boundary between the runner and the job code it runs.
So job steps can read this runner's ephemeral registration, the JIT config on
`run.sh`'s argv (`/proc/<pid>/cmdline`), and the spent claim token in
`/proc/1/environ`. The claim token is worthless by then: single use, already
exchanged. The JIT config lets a job act as its own runner registration until GitHub
removes it after the job. sec-review-3 rates this **LOW**: the window in which untrusted
code could run on a cr-ci runner is closed by the runner-side job-started hook ("Job
guard", below). On top of it:
- the routing expression's fork-PR guard routes only same-repo `push` and `pull_request`
  events and sends everything else to `ubuntu-latest` (fork PRs, `pull_request_target`,
  `workflow_run`, `merge_group`, schedule, workflow_dispatch);
- the mediator's fork guard (`forkReason`) is an allowlist of the same two events and
  also refuses runs whose repository or head repository is not the target repository
  (fork PRs);
- one job runs per runner;
- the hijack guard and the reaper catch a runner registration that ends up running
  another job or is left behind.

**Residual:** a compromised trusted job can still read its own runner's registration.
The structural fix (a runner-only uid with job steps dropped to another uid, or passing
the JIT config by file and deleting it once the listener has started) is tracked in
ptone/scion#5978
([architecture](../docs/architecture.md#residual-risk-job-code-can-read-the-runners-credentials)).

**Job guard (`cr-ci-job-guard.sh`).** The entrypoint exports, read-only,
`ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/bin/cr-ci-job-guard.sh`,
`CR_JOB_GUARD_MARKER` and `CR_JOB_GUARD_REPOS` (= `GH_REPOS`, the repositories the
runner serves). The hook allows a job only if `GITHUB_REPOSITORY` is one of
`CR_JOB_GUARD_REPOS` (whole entries, case-insensitive) and `GITHUB_EVENT_NAME` is
`push`, or `pull_request` with the payload's `pull_request.head.repo.full_name` equal to
`GITHUB_REPOSITORY`. It refuses anything else, and also an empty or malformed served
list or a missing or unparsable payload (fail closed).

**No fail-open: the hook must be the pinned one.** If the worker cannot run the hook
(missing, not executable), only its pre-job step fails and the job's `always()` steps
still run. So before any claim (and in warm-up) the entrypoint exits **2**, without
starting `run.sh`, unless:
- the hook and `/etc/cr-ci/job-guard.sha256` are regular files (not symlinks), owned by
  root, with no group or other write bit and not writable by the runner's uid, and so
  are their directories (`/usr/local/bin`, `/etc/cr-ci`); the runner must not run as
  root;
- the hook is executable and its sha256 equals the one in `/etc/cr-ci/job-guard.sha256`.
  That value is the committed `runner/cr-ci-job-guard.sh.sha256`, installed read-only by
  the Dockerfile, which also checks it (`sha256sum -c`) and the owners and modes at build
  time. `TestJobGuardPinnedSHA` fails when the hook changes without its pin.

**The path is fixed in production.** `CR_JOB_GUARD_HOOK`, `CR_JOB_GUARD_SHA_FILE`,
`CR_JOB_GUARD_OWNER_UID` and every other `CR_*` override take effect only in test mode:
`CR_TEST_MODE=1` and the entrypoint not PID 1. Production runs it as PID 1 (the
image's exec-form `ENTRYPOINT`), so it cannot enable test mode; otherwise each override
is logged (`ignoring CR_...`) and dropped. A job cannot set any of them anyway: the
entrypoint's environment is the Cloud Run execution's (deploy-job.sh and the
mediator's overrides), and the entrypoint has made its checks and exported the guard's
settings before `run.sh`, and so any job, exists. A value for one of the exported
variables in the execution environment is replaced, not used.

Checked against actions/runner v2.338.0:
- **Before any step:** Runner.Worker adds the hook to its pre-job steps ahead of
  container setup, every action's `pre` step and all job steps, checkout included
  (`src/Runner.Worker/JobExtension.cs` L303-312, L315, L328, L362; steps = pre-job
  steps then job steps, L585-586). Only action downloads (no job code) happen
  earlier (L301).
- **Not overridable:** the worker reads the variable from its own process environment
  (`Environment.GetEnvironmentVariable`, L304), inherited from `run.sh`. A workflow's or
  job's `env:` only shapes step environments, and no step has run yet.
- **A failed hook alone does not stop the job:** later steps with `if: always()`,
  `failure()` or `!cancelled()` would still run (`src/Runner.Worker/StepsRunner.cs`
  L200-235). So a refusal is a **hard stop**: the hook writes the marker (the reason
  and the job's own sanitized event, head and repository; never the served list, a
  token or the JIT config), prints the reason code into the job's log, then SIGKILLs, in this order, its parent (the worker), the parent's process group,
  and its own group unless that group is its alone. The runner spawns its children
  without a new session or group (`src/Runner.Sdk/ProcessInvoker.cs` L226-298), so
  normally both are run.sh's group (listener, worker, hook) and the hook dies last.
  Killing the parent first means the stop does not rest on the hook sharing the
  worker's group. The worker is still waiting for the hook to exit
  (`JobHookProvider.RunHook` awaits it), so it cannot reach a step first.
- **Runs under `bash -e`:** the runner starts a `.sh` hook as `bash -e <file>`
  (`src/Runner.Worker/Handlers/ScriptHandler.cs` L213-216 with
  `ScriptHandlerHelpers.cs` L19), so the hook turns errexit off first (`set +e`): a
  failing `kill` cannot end it before the rest of the stop sequence.
- **Fail closed:** if the hook is still alive after the kills (a group of its own), it
  checks once a second that the worker and its group are gone and exits 1; if they are
  not gone after 10 s it exits 1 with `could not stop the runner`. Independently, the
  entrypoint's enforcer, which sees the marker, SIGKILLs run.sh's group itself.
  (Residual, rated very unlikely by sec-review-3: if every SIGKILL and the marker write
  all fail, the enforcer has nothing to act on.)

- **What the job's log shows:** only `job refused by the cr-ci runner's job guard
  (reason=<code>)`. The refused job's log may be public (a fork pull request), and under
  org scope the served list can name private repositories, so the hook prints no
  repository name. The marker sits in the entrypoint's private temp dir; the job runs as
  the same uid but no step of it ever runs to read it.

After `run.sh` has died, the entrypoint logs, to its own stderr (the container log in
Cloud Logging, never the job's log), `ERROR job_guard_refused: ... (reason=... event=...
head=... repository=... served=<GH_REPOS>)`, adding the served list itself, and exits
**5**. The mediator's hijack guard deletes the registration
when GitHub reports the job.

## Testing

```bash
cd cr-ci && go test ./runner/
```

The harness needs `bash`, `curl`, `jq`, `awk` and `tar`, and no network or GCP
access. It uses throwaway random tokens and configs. Test-only overrides
point the script at a fake mount table (which lists the temp HOME as the
volume), a stub runner directory, a zero retry backoff, and a fake metadata
server on loopback: `CR_MOUNTINFO`, `CR_RUNNER_DIR`,
`CR_CLAIM_BACKOFF_SECONDS` and `CR_METADATA_URL`. `CR_EXPECTED_HOME` (default
`/home/runner`) lets the harness use a temp dir as `HOME`. `CR_METADATA_URL` defaults
to `http://metadata.google.internal` and accepts only that value or a
loopback http origin. `CR_JOB_GUARD_HOOK`, `CR_JOB_GUARD_SHA_FILE` and
`CR_JOB_GUARD_OWNER_UID` point the job guard check at a copy of the hook and its pin
owned by the test's uid. All of them need test mode (`CR_TEST_MODE=1`, which the
harness sets; the entrypoint is not PID 1 there) and are ignored otherwise.
Some tests also put stub `stat` and `sleep` commands first on `PATH`. Every
run has a `~/.curlrc` containing `verbose`, so if `curl -q` were dropped, the
leak assertions would catch the tokens on stderr. Like the real metadata
server, the fake one answers 403 without `Metadata-Flavor: Google`; it also
answers 400 on a wrong audience or a missing `format=full`. The harness
covers:

- **Exit code and signals.** A stub `run.sh` exiting 134 makes the
  entrypoint exit 134. One that prints run-helper's `Exiting with unknown error
  code: 134` and exits 0 also makes it exit 134; the same text inside another
  line, with trailing text or without digits, is ignored. SIGTERM and SIGINT
  sent to the entrypoint reach `run.sh`, and its exit code is returned.
- **Success.** The metadata request carries the flavor header,
  `audience=MEDIATOR_URL` (with any trailing slash stripped) and
  `format=full`. The claim carries `Bearer <ID token>` and
  `X-Runner-Claim: <claim token>`. `run.sh` gets `--jitconfig <config>`, is
  the entrypoint's child with SIGINT not ignored, runs in the runner dir, and
  sees the volume env (`RUNNER_TEMP` under `~/work`, `GOTMPDIR`, no `TMPDIR`) but no token. The claim token, the ID token and the config appear on neither
  stdout nor stderr. A RUNNER_NAME/GH_JOB_ID mismatch only warns.
- **Identity.** A claim 401 exits 3 without retrying. Two metadata 5xx
  responses and then a token leads to a normal start on attempt 3. Five
  metadata 5xx responses, or an unreachable metadata server, exit 3 without
  any claim request. Metadata 404 or 403, an empty body, a non-JWT body, or
  a header-injection attempt exits 3 without retrying. A token followed by
  LF, CRLF or two LFs is accepted with the line ending stripped. Two metadata failures
  plus three claim failures use up the shared budget of 5. The ID token is
  fetched once and reused when the claim is retried.
- **Claim failures.** A 410 exits 3 without retrying. Two 5xx responses and
  then a success leads to a normal start. Five 5xx responses or five
  connection errors exit 3. A 405, a wrong content type, or bad, missing,
  empty or non-string `jit_config` exits 3 without retrying. The retry delays
  are 1, 2, 4, 8 s by default, and they double from a fractional base too
  (checked with a `sleep` stub).
- **Volume problems.** A missing HOME, a HOME that is not a mount point by
  either check (including a job that mounts the volume at `/scratch`), or a
  read-only volume exits 2 without
  contacting the mediator. tmpfs only logs a warning. The device fallback
  accepts a mount that is missing from the table, or used when the table is
  unreadable (checked with a `stat` stub).
- **Cache parity.** Success leaves `~/work`, `~/.cache`, `~/go` and `~/.npm`
  as real directories on the volume (or `~/.cache` a link to RAM in RAM mode),
  and `run.sh` sees the ubuntu-latest Go paths and no `XDG_CACHE_HOME` or
  `TMPDIR`. Existing directories are kept with their contents. A symlink at any
  of them (the old `/scratch` layout included), a non-directory, a wrong `HOME`
  or an unset `HOME` exits 2 without contacting the mediator. The volume root's
  owner and mode are logged, and a root this uid owns is set to 0755. A tar
  round trip with actions/cache's scheme (`-P -C` the workspace, entries
  `../../../.cache/...` and `../../../go/pkg/mod/...`) restores an ubuntu-latest
  archive to where Go reads, with the bytes on the volume. The same round trip
  restores a cr-ci archive under a plain ubuntu `HOME`. Whole-directory archives
  (`../../../.npm` and `../../../.cache`, directory entries included) restore
  onto the real directories, physically on the volume.
- **Bad env.** Any missing variable, a malformed (short or uppercase) token,
  a non-numeric job ID, a non-https or path-bearing URL, a non-loopback
  `CR_METADATA_URL`, or a missing `run.sh` exits 2 without contacting the
  mediator.

Lint the script with `bash -n runner/entrypoint.sh` and both shellcheck runs in
[scripts/README.md, "Static checks"](../scripts/README.md#static-checks)
(upstream CI's 0.9.0 invocation and 0.11.0 `--norc -x`).
