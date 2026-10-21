# cr-ci runner image

Each Cloud Run Job execution of `RUNNER_JOB_NAME` runs this image as one
ephemeral GitHub Actions JIT runner. That runner takes one job and then exits.

| File | Purpose |
| :--- | :--- |
| [`Dockerfile`](Dockerfile) | Image: Ubuntu 24.04, GitHub Actions runner, Go, golangci-lint, git, jq, curl, ca-certificates; non-root `runner` user (uid 1001) |
| [`entrypoint.sh`](entrypoint.sh) | PID 1: checks `/scratch`, redirects caches, does the claim exchange, then `exec ./run.sh --jitconfig` |
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
| `GOLANGCI_LINT_VERSION` / `GOLANGCI_LINT_SHA256` | golangci-lint, `golangci-lint-<v>-linux-amd64.tar.gz` | `golangci-lint-<v>-checksums.txt` on <https://github.com/golangci/golangci-lint/releases> (keep it in step with the version CI pins) |

Every download is checked with `sha256sum -c`, and a mismatch fails the build.
To override a pin, pass the version **and** its checksum with `--build-arg`,
for example `--build-arg GO_VERSION=<v> --build-arg GO_SHA256=<sha>`. git, jq,
curl and the runner's .NET dependencies (installed by the runner's own
`bin/installdependencies.sh`) come from the Ubuntu archive, and apt verifies
their signatures.

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
| `RUNNER_NAME` | `cr-job-<GH_JOB_ID>` (used in logs only) |

The JIT config is never passed in env, args or labels. The entrypoint does
this, in order:

1. **Checks the env.** All four variables must be set. The token must be 32
   lowercase hex chars. `MEDIATOR_URL` must be an https origin; plain http is
   accepted only for `127.0.0.1` and `localhost`, which the test harness uses.
2. **Checks `/scratch`.** `/scratch` must exist, be listed as a mount point in
   `/proc/self/mountinfo`, and be writable. If any check fails, it exits 2
   before the token is spent. Cloud Run's root filesystem is in memory, so a
   missing volume would silently turn the caches into RAM use. If `/scratch` is
   tmpfs (an in-memory volume), it logs a warning.
3. **Redirects to `/scratch`.** It exports `RUNNER_WORKSPACE=/scratch/_work`,
   `RUNNER_TEMP` and `TMPDIR=/scratch/_temp`, `GOPATH=/scratch/gopath`,
   `GOCACHE=/scratch/gocache`, `GOMODCACHE=/scratch/gomodcache`,
   `GOLANGCI_LINT_CACHE=/scratch/golangci-lint-cache` and
   `XDG_CACHE_HOME=/scratch/cache`, and creates those directories. The
   runner's `_work` and `_diag` directories are symlinks to `/scratch/_work`
   and `/scratch/_diag`, so the work folder lands on the disk whether the JIT
   config names `_work` or `/scratch/_work` (the mediator sends
   `/scratch/_work`).
4. **Claims the JIT config.** It sends `POST ${MEDIATOR_URL}/internal/claim`
   with `Authorization: Bearer <token>` and an empty body, and makes up to 5
   attempts:

   | Response | Action |
   | :--- | :--- |
   | 200 with `application/json` and a non-empty `jit_config` string | continue |
   | 5xx or connection error/timeout | retry after 1, 2, 4, then 8 s; after the 5th failure, exit 3 |
   | 410 (unknown, expired or used token) | exit 3 at once |
   | anything else (other status, wrong content type, bad JSON) | exit 3 at once; the token may already be spent |

5. **Starts the runner.** It unsets `RUNNER_CLAIM_TOKEN` and runs
   `exec ./run.sh --jitconfig <config>` in `/home/runner/actions-runner`, so
   the runner is PID 1 and its exit code is the execution's exit code.

Exit codes before `run.sh` starts: **2** for a configuration error (env,
`/scratch`, missing `run.sh`) and **3** for a failed claim. Neither path starts
`run.sh`. If a claim is lost (for example, the mediator restarted and its
in-memory claims are gone), the runner exits 3. `/reconcile` then deletes the
orphaned `cr-job-*` registration and relaunches the job if it is still queued.

**Secrets.** Neither the token nor the JIT config is ever printed, logged or
written to disk. The token reaches curl on stdin, as a curl config, not in its
argv. `curl -q` ignores any `~/.curlrc`, so no config file can turn on
verbose header output. The response stays in shell memory. The script never
uses `set -x`; keep it that way. The JIT config does appear in `run.sh`'s argv,
as the runner requires. That is acceptable because it is single use and dies
with the execution.

## Testing

```bash
cd cr-ci && go test ./runner/
```

The harness needs `bash`, `curl`, `jq` and `awk`, and no network or GCP
access. It uses throwaway random tokens and configs. Test-only overrides
point the script at a temp scratch directory, a fake mount table, a stub
runner directory, and a zero retry backoff: `CR_SCRATCH_DIR`, `CR_MOUNTINFO`,
`CR_RUNNER_DIR` and `CR_CLAIM_BACKOFF_SECONDS`. Production leaves them unset.
The harness covers:

- **Success.** `run.sh` gets `--jitconfig <config>`, is exec'd (same PID),
  runs in the runner dir, and sees the `/scratch` env but no token. The token
  and the config appear on neither stdout nor stderr.
- **Claim failures.** A 410 exits 3 without retrying. Two 5xx responses and
  then a success leads to a normal start. Five 5xx responses or five
  connection errors exit 3. A 405, a wrong content type, or bad, missing,
  empty or non-string `jit_config` exits 3 without retrying.
- **`/scratch` problems.** A missing directory, a directory that is not a
  mount point, an unreadable mount table, or a read-only `/scratch` exits 2
  without contacting the mediator. tmpfs only logs a warning.
- **Bad env.** Any missing variable, a malformed token, a non-numeric job ID,
  a non-https or path-bearing URL, or a missing `run.sh` exits 2 without
  contacting the mediator.

Lint the script with `shellcheck -x runner/entrypoint.sh` and
`bash -n runner/entrypoint.sh`.
