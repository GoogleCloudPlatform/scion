# cr-ci scripts (planned, P4)

No scripts exist yet. This page fixes the list and the contract that every
script must meet, so that P4 can be reviewed against it.

## Planned scripts

| Script | Purpose | Main permissions ([matrix](../docs/permissions-matrix.md)) |
| :--- | :--- | :--- |
| `lib.sh` | Shared helpers: load and validate `config.env`, the `run` wrapper behind `--dry-run`, logging | none |
| `bootstrap-gcp.sh` | Enable APIs, create the service accounts, the Artifact Registry repo, the empty secrets, and the IAM bindings of design section 3.5 | C1-C6, C10, C11 |
| `build-runner-image.sh` | Build and push the runner image (`runner/`) with Cloud Build as `BUILD_SA_NAME` | C7 |
| `deploy-job.sh` | Create or update the Cloud Run Job `RUNNER_JOB_NAME` (CPU, memory, ephemeral disk, `--max-retries=0`, `--task-timeout`) | C8 |
| `deploy-mediator.sh` | Build and deploy the mediator service with the config contract as env vars and secret references | C12, C13 |
| `deploy-scheduler.sh` | Create or update the Cloud Scheduler job `RECONCILER_JOB_NAME` (OIDC as `SCHEDULER_SA_NAME`) | C14 |
| `put-secret.sh` | Add a secret version from a file or stdin (initial creation and rotation) | C5, C9 |
| `github-app-manifest.json` + `create-github-app.md` | GitHub App manifest (permissions from the matrix) and the manifest-flow steps (P5) | G1-G3 |
| `set-routing-variable.sh` | Set or clear `ROUTING_VARIABLE_NAME` on each target repo (rollout and fallback) | G10, G11 |
| `teardown.sh` | Delete everything the scripts above created, in reverse order | C18 |

## Contract (every script)

- **Config:** reads only `cr-ci/config.env` (a copy of `config.example.env`),
  or the file given with `--config FILE`, and validates the same rules as
  `mediator/config.go`. No hard-coded repo, org, project or region.
- **`--dry-run`:** prints every `gcloud` or `gh` command it would run, one per
  line, fully expanded and shell-quoted, then exits 0 without calling any API
  that changes state. Read-only describe or list calls used to decide
  idempotent behaviour are also printed rather than run, and the script
  assumes that nothing exists yet. Dry-run transcripts are attached to the P4 PR.
- **Idempotent:** a second run with the same config changes nothing and
  succeeds (create-if-missing, update-in-place).
- **No secret values:** never in arguments, never echoed, never in dry-run
  output. Secret values enter only through stdin or a file path given to
  `gcloud secrets versions add --data-file`.
- **Shell:** `bash`, `set -euo pipefail`, shellcheck clean, and `--help`.
- **Failure:** exits non-zero with the failing step named. It stops on a
  permission denial instead of retrying with other credentials.
