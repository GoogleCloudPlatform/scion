# GCP setup

Step by step, with the scripts in [`scripts/`](../scripts/README.md). The GitHub
side (App, installation, routing) is in [github-setup.md](github-setup.md);
the overall order is the [README quick start](../README.md#quick-start). The
roles for every step are in [permissions-matrix.md](permissions-matrix.md#2-gcp-steps).

Every script prints its commands and changes nothing unless you add
`--apply`. Run each one without `--apply` first and read the transcript; the
expected transcripts for a sample config are in
[`scripts/testdata/golden/`](../scripts/testdata/golden/). In a transcript,
`+` lines change something, `?` lines are read-only checks (a dry run assumes
nothing exists yet), and `#` lines are notes.

## 1. Prerequisites

- **Deploy preconditions** (required): see
  [architecture.md](architecture.md#deploy-preconditions). In particular the
  target repo (or org) requires approval of fork PR workflows for **all**
  external contributors, `EXEC_SA_NAME` gets no roles (the scripts check
  this), and `roles/run.viewer` stays narrow.
- **Project and region.** A dedicated project is simplest (`roles/owner` then
  covers everything). The design suggests `deploy-demo-test` / `us-central1`
  for the PoC. On a shared project, the deployer needs the least-privilege
  bundle in [matrix 2.1](permissions-matrix.md#21-deployer-roles-least-privilege-bundle).
- **Tools:** bash >= 4, the gcloud CLI (version floor `CRCI_MIN_GCLOUD` in
  `scripts/lib.sh`; see [matrix U8](permissions-matrix.md#5-design-corrections-and-open-items)),
  authenticated as the deployer (`gcloud auth login`). docker only for
  `--local` builds, gh only for `set-routing-variable.sh`.
- **Config.**

  ```bash
  cd cr-ci
  cp config.example.env config.env      # gitignored; never commit it
  $EDITOR config.env                    # TARGET_REPOS, GITHUB_APP_ID (1 until the App exists),
                                        # GITHUB_APP_INSTALLATION_SCOPE, GCP_PROJECT, REGION
  scripts/check-config.sh               # validates like the mediator, prints resolved values
  ```

  The file holds resource names only, never secret values.

## 2. Bootstrap: `scripts/bootstrap-gcp.sh`

```bash
scripts/bootstrap-gcp.sh            # read the transcript
scripts/bootstrap-gcp.sh --apply
```

It does the following (each create only if the resource is missing; bindings
are additive, so a re-run is harmless):

1. Enables the Cloud Run, Artifact Registry, Secret Manager, Cloud Scheduler,
   Cloud Build and IAM APIs (C1).
2. Creates `BUILD_SA_NAME`, `MEDIATOR_SA_NAME`, `SCHEDULER_SA_NAME` and
   `EXEC_SA_NAME` (C2).
3. Creates the Docker repository `ARTIFACT_REPO` in `REGION` (C3).
4. Creates the two secret containers, with no versions yet (C5).
5. Binds:
   - `BUILD_SA_NAME`: `artifactregistry.writer` on the repository (C4) and
     `logging.logWriter` on the project (C7, needed by `CLOUD_LOGGING_ONLY`
     builds);
   - `MEDIATOR_SA_NAME`: `secretmanager.secretAccessor` on each secret (C6),
     and `iam.serviceAccountUser` on `EXEC_SA_NAME` (C11).
6. Checks that `EXEC_SA_NAME` holds no project role, and stops if it does.

The job-level binding C10 and the service-level binding C13 need the job and
the service to exist, so `deploy-job.sh` and `deploy-mediator.sh` make them.

## 3. Runner image: `scripts/build-runner-image.sh`

```bash
scripts/build-runner-image.sh --apply         # Cloud Build as BUILD_SA_NAME
scripts/build-runner-image.sh --apply --local # or: local docker, pushed as BUILD_SA_NAME
```

- Builds `runner/` (all versions pinned in `runner/Dockerfile`, see
  [runner/README.md](../runner/README.md)) for linux/amd64 and pushes
  `REGION-docker.pkg.dev/GCP_PROJECT/ARTIFACT_REPO/RUNNER_IMAGE_NAME:TAG`.
- **Tag:** default `git-<12 hex>`, the git tree hash of `runner/` at HEAD, so
  an unchanged runner keeps its tag and is not rebuilt. Uncommitted changes
  under `runner/` are refused; `--tag TAG` overrides. The script prints the
  tag to pass to `deploy-job.sh` (whose default is the same).
- **Cloud Build** runs `scripts/cloudbuild.yaml` (builder pinned by digest,
  `logging: CLOUD_LOGGING_ONLY`) as `BUILD_SA_NAME`. The deployer needs
  `roles/cloudbuild.builds.editor` and `iam.serviceAccountUser` on
  `BUILD_SA_NAME` (C7). Unverified: whether the build SA also needs read
  access to the staging bucket `gs://GCP_PROJECT_cloudbuild`, where
  `gcloud builds submit` uploads the source
  ([matrix U10](permissions-matrix.md#5-design-corrections-and-open-items)).
- **`--local`** builds with the local docker daemon and pushes with a
  short-lived token of `BUILD_SA_NAME`, which goes from
  `gcloud auth print-access-token --impersonate-service-account` to
  `docker login --password-stdin` without touching argv. It then logs out.
  The deployer needs `roles/iam.serviceAccountTokenCreator` on
  `BUILD_SA_NAME` for this.

## 4. Runner job: `scripts/deploy-job.sh`

```bash
scripts/deploy-job.sh --apply [--tag TAG]
```

- Creates the Cloud Run Job `RUNNER_JOB_NAME`, or updates it in place, from
  the config:
  - `RUNNER_CPU`, `RUNNER_MEMORY`;
  - one task, `--max-retries=0`, and `--task-timeout` from `TASK_TIMEOUT` (in
    seconds);
  - service account `EXEC_SA_NAME`, label `managed-by=cr-ci`;
  - an ephemeral-disk volume of `RUNNER_DISK_GI` GiB mounted at `/scratch`.
- An update replaces the volume (`--clear-volumes --clear-volume-mounts`,
  then the same `--add-volume`). Unverified: that gcloud accepts these flags
  together, and whether ephemeral-disk volumes need `gcloud beta`
  ([matrix U9](permissions-matrix.md#5-design-corrections-and-open-items)).
- With `--apply` it refuses to run if the image tag does not exist.
- Grants `MEDIATOR_SA_NAME` `roles/run.developer` on this job only (C10), then
  re-checks that `EXEC_SA_NAME` holds no project role.

## 5. Mediator URL and the GitHub App

The mediator needs `GITHUB_APP_ID` to start, and the App needs the webhook
URL. Cloud Run's deterministic URL `https://MEDIATOR_SERVICE_NAME-PROJECT_NUMBER.REGION.run.app`
is known before the first deploy, so:

1. Set `MEDIATOR_URL` to it ([github-setup.md, step 1](github-setup.md#1-fix-the-mediator-url)).
2. Create the App ([step 2](github-setup.md#2-create-the-github-app-from-the-manifest-recommended))
   and put its ID into `GITHUB_APP_ID`.

## 6. Secrets: `scripts/set-secrets.sh`

```bash
chmod 600 app-key.pem webhook-secret
scripts/set-secrets.sh --app-key-file app-key.pem --webhook-secret-file webhook-secret --apply
# manual App setup without a GitHub-generated webhook secret:
scripts/set-secrets.sh --generate-webhook-secret new-webhook-secret --apply
```

- Adds a new version to `GITHUB_APP_PRIVATE_KEY_SECRET_NAME` and/or
  `GITHUB_WEBHOOK_SECRET_NAME` (C5). Each value is read from a file or from
  stdin (`-`, for at most one of them) and piped to
  `gcloud secrets versions add --data-file=-`. It is never put in an argument
  or printed.
- Files readable by group or others are refused (`chmod 600`); browser
  downloads are often `0644`. The key file must look like a PEM private key.
- One trailing newline (LF or CRLF) is stripped from the webhook secret,
  because the mediator uses the stored bytes as they are, and GitHub signs
  with exactly what was typed into the App settings.
- `--generate-webhook-secret OUT` writes 32 random bytes as 64 hex characters,
  with no newline, to a new file `OUT` (mode 0600; it must not exist), and
  adds exactly that. Paste `OUT` into the App's webhook secret, then delete
  `OUT`.
- Delete the local key and secret files once they are stored. Rotation is in
  [operations.md](operations.md#secret-rotation).

## 7. Mediator: `scripts/deploy-mediator.sh`

```bash
scripts/deploy-mediator.sh --apply [--tag TAG] [--local] [--no-invoker-iam-check]
```

1. Builds `mediator/Dockerfile` (pinned base images; static binary on
   distroless, non-root) like the runner image. The default tag is the tree
   hash of `mediator/` plus `go.mod`.
2. Deploys `MEDIATOR_SERVICE_NAME` as `MEDIATOR_SA_NAME` (C12):
   - `--max-instances=1` (**required**: claims live in the mediator's memory)
     and `--min-instances=0`; 1 vCPU, 512 MiB, request timeout 300 s;
   - public ingress;
   - every config key as an env var (`--set-env-vars` with the `^;^`
     delimiter, because values contain commas and spaces). The secrets are
     passed **by name**; the mediator reads them from Secret Manager with its
     own identity.
3. Re-reads the service and stops if max-instances is not 1.
4. Grants `SCHEDULER_SA_NAME` `roles/run.invoker` on the service only (C13).
5. Prints the service URL. If `MEDIATOR_URL` is empty, it tells you to set it
   and run the script again; the service answers 503 on `/webhook` and
   `/reconcile` until then. If `MEDIATOR_URL` is not one of the service's URLs
   (`status.url` or the deterministic URL), it warns: update `MEDIATOR_URL`
   and the App webhook URL, then run the script again.

**Public access.** GitHub cannot present Google credentials, so the service
must be reachable without Google auth; HMAC protects `/webhook`, and the
mediator checks Google ID tokens itself on `/reconcile` and `/internal/claim`.
The default `--allow-unauthenticated` binds `allUsers` as invoker, which needs
`roles/run.admin` on the service. If an org policy (domain-restricted sharing)
forbids `allUsers`, use `--no-invoker-iam-check`
([matrix U6](permissions-matrix.md#5-design-corrections-and-open-items)).

## 8. Reconciler: `scripts/deploy-scheduler.sh`

```bash
scripts/deploy-scheduler.sh --apply
```

Creates or updates the Cloud Scheduler job `RECONCILER_JOB_NAME` in `REGION`
(C14):

- `RECONCILE_SCHEDULE`, time zone UTC;
- `POST MEDIATOR_URL/reconcile`, attempt deadline 300 s;
- an OIDC token for `SCHEDULER_SA_NAME` with audience `MEDIATOR_URL`.

It refuses to run while `MEDIATOR_URL` is empty.

Note: on projects that enabled the Cloud Scheduler API before 2019-03-19,
the Cloud Scheduler service agent
(`service-PROJECT_NUMBER@gcp-sa-cloudscheduler.iam.gserviceaccount.com`) may
also need `roles/iam.serviceAccountTokenCreator` to mint the OIDC token (per
the Cloud Scheduler docs). Newer projects have it by default.

## 9. Quotas

Runner executions consume the regional Cloud Run ephemeral-disk quota as
`RUNNER_DISK_GI` x running executions. The default quota of 100 GiB per region
fits the defaults (5 x 20 GiB). To raise `MAX_CONCURRENT_RUNNERS`:

1. In the console (IAM & Admin > Quotas), filter for Cloud Run Admin API
   quotas in `REGION`: ephemeral disk, and the CPU and memory allocation
   quotas if `MAX_CONCURRENT_RUNNERS` x `RUNNER_CPU` / `RUNNER_MEMORY` exceed
   them. Request an increase to cover the new maximum (C15,
   `roles/cloudquotas.admin`).
2. After approval, raise `MAX_CONCURRENT_RUNNERS` in `config.env` and run
   `scripts/deploy-mediator.sh --apply`. The mediator enforces the cap; the
   job itself needs no change.

## 10. Teardown: `scripts/teardown.sh`

```bash
scripts/teardown.sh                    # read the transcript
scripts/teardown.sh --apply            # asks you to type GCP_PROJECT
scripts/teardown.sh --apply --keep-secrets --keep-images
```

It deletes, in reverse order, skipping what is already gone:

1. the scheduler job;
2. the mediator service;
3. the runner job;
4. the Artifact Registry repository with all images (`--keep-images` keeps it
   and removes `BUILD_SA_NAME`'s binding);
5. both secrets with all versions (`--keep-secrets` keeps them and removes
   `MEDIATOR_SA_NAME`'s access);
6. `BUILD_SA_NAME`'s project-level `logging.logWriter` binding;
7. the four service accounts.

APIs stay enabled, since the project may use them. Uninstall the GitHub App
and clear the routing variable first
([github-setup.md, step 10](github-setup.md#10-removing-cr-ci-from-a-repository)),
so no job waits for a runner that no longer exists.
