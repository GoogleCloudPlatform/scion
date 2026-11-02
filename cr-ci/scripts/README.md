# cr-ci scripts

Deploy, operate and tear down cr-ci on GCP and GitHub. Every script defaults to
a **dry run** that prints the exact commands; only `--apply` changes anything.
The step-by-step order is in [docs/gcp-setup.md](../docs/gcp-setup.md) and the
[README quick start](../README.md#quick-start).

| Script | Purpose | Permissions ([matrix](../docs/permissions-matrix.md)) |
| :--- | :--- | :--- |
| `lib.sh` | Shared helpers (sourced): config parser and validator, `run` wrapper, idempotency checks, tool version checks | none |
| `check-config.sh` | Validate `config.env` (same rules as the mediator) and print the resolved values; read-only | none |
| `bootstrap-gcp.sh` | Enable APIs; create the 4 service accounts, the Artifact Registry repo, the Cloud Build staging bucket and the 2 secret containers (no versions); IAM bindings C4, C6, C7, C11, C19 | C1-C7, C11, C19 |
| `set-secrets.sh` | Add secret versions from a file (0600, outside the checkout) or stdin: App private key, webhook secret, or a generated webhook secret | C5, C9 |
| `build-runner-image.sh` | Build `runner/` with Cloud Build (or `--local` docker) as `BUILD_SA_NAME` and push it to `ARTIFACT_REPO` | C7 |
| `deploy-job.sh` | Create or update the Cloud Run Job `RUNNER_JOB_NAME` (shape, ephemeral disk mounted at HOME, no retries, task timeout, `EXEC_SA_NAME`); binding C10 | C8, C10 |
| `deploy-mediator.sh` | Refuse the placeholder `GITHUB_APP_ID=1`; build `mediator/Dockerfile` (upload limited by the `.gcloudignore` allowlist) and deploy `MEDIATOR_SERVICE_NAME` (`max-instances=1`, env from the config, secrets by name); binding C13; prints the URL for `MEDIATOR_URL` | C12, C13 |
| `deploy-scheduler.sh` | Create or update `RECONCILER_JOB_NAME` (POST `/reconcile`, OIDC as `SCHEDULER_SA_NAME`, audience `MEDIATOR_URL`) | C14 |
| `set-routing-variable.sh` | Set or clear `ROUTING_VARIABLE_NAME` on `TARGET_REPOS` (gh only; `--repo`, `--clear`, `--value`) (P5) | G10, G11 |
| `teardown.sh` | Delete everything above in reverse order (typed confirmation; `--keep-secrets`, `--keep-images`) | C18 |
| `cloudbuild.yaml` | Cloud Build config for both images (pinned builder, `CLOUD_LOGGING_ONLY`) | |
| `github-app-manifest.json` | GitHub App manifest (P5, see [github-setup.md](../docs/github-setup.md)) | G1-G3 |

## Contract (every script)

- **Config:** `cr-ci/config.env` (a filled-in copy of `config.example.env`), or
  `--config FILE`. `lib.sh` parses it line by line (`KEY=value`; blank lines
  and `#` comments skipped; surrounding spaces trimmed) and **never sources
  it**: values may contain spaces, quotes are not interpreted and nothing is
  expanded or executed. Unknown and duplicate keys are rejected. Keys and
  defaults come from `config.example.env`, and validation follows
  `mediator/config.go` exactly (`TestScriptValidatorMatchesLoad` keeps the two
  in lockstep). Every key is validated by every script, so `GITHUB_APP_ID` and
  `TARGET_REPOS` are needed from the first step (a placeholder App ID such as
  `1` is fine until the App exists).
- **Dry run is the default.** Each command is printed on one line, shell-quoted:
  - `+ <command>`: a command that changes something (run only with `--apply`);
  - `? <command>`: a read-only check (describe, list); a dry run does not run
    it and assumes the resource does not exist yet, so it shows the create path;
  - `# <text>`: a note.

  A dry run calls no tool at all (the tests run the scripts with recording
  stubs and assert zero calls). Golden transcripts are in
  [`testdata/golden/`](testdata/golden/).
- **`--apply`** runs the commands. It checks tool versions first (gcloud >=
  `CRCI_MIN_GCLOUD` in `lib.sh`; docker for `--local`; gh for
  `set-routing-variable.sh`). Every command passes `--project` explicitly; the
  gcloud default project is never used.
- **Idempotent:** resources are created only if a check says they are
  missing; updates are in place (`run jobs update`, `run deploy`,
  `scheduler jobs update`); IAM bindings are additive; an image tag that
  already exists is not rebuilt; teardown skips what is gone.
- **Failure:** a failing step stops the script with the command named. A
  check that fails for any reason other than "not found" stops it too;
  nothing is retried with other credentials. "Not found" is recognised by one
  helper, `crci_is_not_found` in `lib.sh`, which lists the exact messages
  (gcloud run's "Cannot find job/service [", the API's `NOT_FOUND`, "not
  found", "does not exist"). A `PERMISSION_DENIED` is never "not found", even
  with gcloud's "(or it may not exist)". The test stubs print each command's
  real message.
- **No secret values** in argv, output or files. Secrets enter only through
  stdin or a file given to `set-secrets.sh`, and reach gcloud on stdin
  (`gcloud secrets versions add --data-file=-`). The `--local` image push gets
  the build SA's access token into `docker login --password-stdin` the same
  way. The tests check argv, output and goldens for the throwaway values.
- **Image tags:** default `git-<12 hex>` from the git tree hash of the image's
  source at HEAD (`runner/`; `mediator/` plus `go.mod`), so an unchanged source
  keeps its tag. With `--apply`, uncommitted changes there are refused; pass
  `--tag TAG` to override (and to build outside a git checkout).
- **Shell:** bash >= 4, `set -euo pipefail`, `--help` on every script, and
  shellcheck clean under both of these (see [Static checks](#static-checks)).

## Static checks

Every cr-ci shell file must pass two shellcheck runs, because they disagree on
some codes (0.9.0 reports a trap-invoked function's body as SC2317, 0.11.0 as
SC2329; a directive must name both):

- **Upstream CI's invocation**, which is what blocks a PR: ubuntu-latest's apt
  shellcheck (0.9.0), from the repository root, on every `*.sh` file:
  `shellcheck -x --source-path=SCRIPTDIR ./cr-ci/scripts/deploy-job.sh` (no
  `--norc`, so a repository `.shellcheckrc` would apply). Outside Ubuntu, the
  same 0.9.0 binary comes in the PyPI wheel `shellcheck-py` 0.9.0.6
  (`shellcheck_py-0.9.0.6-py2.py3-none-manylinux_2_5_x86_64.manylinux1_x86_64.manylinux_2_17_x86_64.manylinux2014_x86_64.whl`,
  sha256 `730235c4f92657884f8b343d5426e4dc28e9a6ba9ad54d469cd038e340ea5be0`;
  the binary is `shellcheck_py-0.9.0.6.data/scripts/shellcheck` inside it).
- **A current release** (0.11.0) with `shellcheck --norc -x`, on the `*.sh` files
  and the extension-less test stubs in [`testdata/stubs/`](testdata/stubs/),
  which upstream CI does not check.

## Tests

```bash
cd cr-ci && go test ./scripts/ ./mediator/   # -update rewrites the goldens
```

[`scripts_test.go`](scripts_test.go) runs each script with stub `gcloud`,
`docker` and `gh` ([`testdata/stubs/`](testdata/stubs/)) first on `PATH`. The
stubs record argv (and the stdin of secret and login calls) and answer checks
from a per-test state file; they never contact anything. It asserts:

- the dry-run goldens, and that a dry run calls nothing;
- that `--apply` runs exactly the transcript's commands;
- an idempotent second run;
- that a permission denial stops the script;
- that secrets travel on stdin only;
- `max-instances=1`;
- that the IAM bindings equal the permissions matrix;
- teardown confirmation and order;
- the default tag.
