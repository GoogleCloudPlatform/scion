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
- **Region choice.** `REGION` is the only place a region is set; nothing in the
  code or scripts assumes `us-central1`. Any region where Cloud Run Jobs and
  Artifact Registry are available works: the job, the mediator, the image
  repository and the staging bucket all go to `REGION`. Cost differs:
  `us-central1` has Tier 1 Cloud Run pricing, while for example `us-west2` is
  Tier 2 ([Cloud Run locations](https://docs.cloud.google.com/run/docs/locations));
  Tier 2 costs more per vCPU-second and GiB-second (current rates:
  [Cloud Run pricing](https://cloud.google.com/run/pricing)). The design's remark
  that Cloud Run *Instances* are not available in some regions does not matter
  here: cr-ci uses Jobs. Check that the region offers Cloud Run Jobs, and the
  ephemeral-disk volumes they use, before choosing it. The ephemeral-disk quotas (section 9) are per
  region, so a new region needs its own quota requests. Moving region means
  redeploying everything (a new `MEDIATOR_URL`, so the App's webhook URL changes too).
- **Tools:** bash >= 4, the gcloud CLI (version floor `CRCI_MIN_GCLOUD` in
  `scripts/lib.sh`; see [matrix U8](permissions-matrix.md#5-design-corrections-and-open-items)),
  authenticated as the deployer (`gcloud auth login`). docker only for
  `--local` builds, gh only for `set-routing-variable.sh`.
- **Config.**

  ```bash
  cd cr-ci
  cp config.example.env config.env      # gitignored; never commit it
  $EDITOR config.env                    # TARGET_REPOS, GITHUB_APP_ID (1 until the App exists),
                                        # GCP_PROJECT, REGION (GITHUB_APP_INSTALLATION_SCOPE defaults to repo)
                                        # (optional: BUILD_STAGING_BUCKET if the default name is taken)
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

1. Lists the enabled APIs and enables only the missing ones among Cloud Run,
   Artifact Registry, Secret Manager, Cloud Scheduler, Cloud Build, IAM and
   Cloud Storage (C1). If all are enabled, no enable call is made, so
   `serviceusage.services.enable` is only needed when an API is missing; the
   list itself needs `serviceusage.services.list` (for example
   `roles/serviceusage.serviceUsageConsumer`). A dry run shows the check and
   an enable of all seven; `--apply` enables only what the check does not
   report.
2. Creates `BUILD_SA_NAME`, `MEDIATOR_SA_NAME`, `SCHEDULER_SA_NAME` and
   `EXEC_SA_NAME` (C2).
3. Creates the Docker repository `ARTIFACT_REPO` in `REGION` (C3), and the
   Cloud Build source staging bucket `BUILD_STAGING_BUCKET` (default
   `GCP_PROJECT-cr-ci-staging`; uniform access, public access prevention,
   uploaded sources deleted after 7 days; C19). Bucket names are global: if
   the default is taken, set `BUILD_STAGING_BUCKET`.
4. Creates the two secret containers, with no versions yet (C5).
5. Binds:
   - `BUILD_SA_NAME`: `artifactregistry.writer` on the repository (C4),
     `storage.objectViewer` on the staging bucket only (C19, to read the
     uploaded build source), and `logging.logWriter` on the project (C7,
     needed by `CLOUD_LOGGING_ONLY` builds);
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
  `BUILD_SA_NAME` (C7). The source is uploaded to the dedicated staging
  bucket (`--gcs-source-staging-dir=gs://BUILD_STAGING_BUCKET/source`), the
  only bucket the build SA can read (C19); the shared default
  `gs://GCP_PROJECT_cloudbuild` is not used.
- **`--local`** builds with the local docker daemon and pushes with a
  short-lived token of `BUILD_SA_NAME`, which goes from
  `gcloud auth print-access-token --impersonate-service-account` to
  `docker login --password-stdin` without touching argv. It then logs out.
  The deployer needs `roles/iam.serviceAccountTokenCreator` on
  `BUILD_SA_NAME` for this.
- **IAM propagation:** bindings made by `bootstrap-gcp.sh` take effect after
  a short delay. A build started right after bootstrap can fail, or lose its
  logs, because `BUILD_SA_NAME`'s `logging.logWriter` (required by
  `CLOUD_LOGGING_ONLY`) or its staging-bucket access is still propagating.
  Wait about 60 s after bootstrap, or simply re-run the build script (an
  existing tag is not rebuilt; a failed one is retried).
- **BuildKit:** both build paths use BuildKit (`DOCKER_BUILDKIT=1` in
  `scripts/cloudbuild.yaml` and on the local `docker build`), which
  `runner/Dockerfile` needs for `COPY --chmod`.

## 4. Runner job: `scripts/deploy-job.sh`

```bash
scripts/deploy-job.sh --apply [--tag TAG] [--warm-up]
```

- Creates the Cloud Run Job `RUNNER_JOB_NAME`, or updates it in place, from
  the config:
  - `RUNNER_CPU`, `RUNNER_MEMORY`;
  - one task, `--max-retries=0`, and `--task-timeout` from `TASK_TIMEOUT` (in
    seconds);
  - service account `EXEC_SA_NAME`, label `managed-by=cr-ci`;
  - an ephemeral-disk volume of `RUNNER_DISK_GI` GiB mounted at the runner's HOME,
    `/home/runner` (the runner itself is installed at `/opt/actions-runner`; the
    entrypoint creates the work folder and caches on the disk; operations.md,
    "Runner layout switch").
- The GA `gcloud run jobs create` accepts the ephemeral-disk flags (verified
  in the P6 deploy). An update replaces the volume (`--clear-volumes
  --clear-volume-mounts`, then the same `--add-volume`); that clear and add
  combine in one `update` call is still unverified
  ([matrix U9](permissions-matrix.md#5-design-corrections-and-open-items),
  checked by [poc-checklist B2](poc-checklist.md)).
- With `--apply` it refuses to run if the image tag does not exist.
- Grants `MEDIATOR_SA_NAME` `roles/run.developer` on this job only (C10), then
  re-checks that `EXEC_SA_NAME` holds no project role.
- `--warm-up` then runs one no-op execution and waits for it
  (`gcloud run jobs execute --update-env-vars=RUNNER_WARMUP=1 --wait`). The
  entrypoint runs its image, volume (HOME) and work and cache directory checks and
  exits 0 before
  any claim. The first execution after an image change pays the image import (about
  34 s for the 371 MiB image in the P6 PoC); the warm-up takes that hit instead
  of the first real job, and it fails the deploy if the checks fail. A failed
  warm-up leaves the job on the new image, so real jobs would fail the same
  way: roll back with `scripts/deploy-job.sh --apply --tag <previous tag>`
  (list tags with `gcloud artifacts docker tags list
  REGION-docker.pkg.dev/GCP_PROJECT/ARTIFACT_REPO/RUNNER_IMAGE_NAME`), then fix
  the image. The
  mediator ignores the execution (it has no `RUNNER_NAME`); it counts toward
  `MAX_CONCURRENT_RUNNERS` for the few seconds it runs. Executing with an env
  override needs `run.jobs.runWithOverrides` (in `roles/run.developer`).

## 5. Mediator URL and the GitHub App

The mediator needs `GITHUB_APP_ID` to start, and the App needs the webhook
URL. Cloud Run's deterministic URL `https://MEDIATOR_SERVICE_NAME-PROJECT_NUMBER.REGION.run.app`
is known before the first deploy, so:

1. Set `MEDIATOR_URL` to it ([github-setup.md, step 1](github-setup.md#1-fix-the-mediator-url)).
2. Create the App ([step 2](github-setup.md#2-create-the-github-app-from-the-manifest-recommended))
   and put its ID into `GITHUB_APP_ID`.

## 6. Secrets: `scripts/set-secrets.sh`

Keep secret files **outside the checkout**, for example in `~/cr-ci-secrets`
(`set-secrets.sh` refuses files inside `cr-ci/`, which `deploy-mediator.sh`
uploads to Cloud Build):

```bash
mkdir -m 700 -p ~/cr-ci-secrets
chmod 600 ~/cr-ci-secrets/app-key.pem ~/cr-ci-secrets/webhook-secret
scripts/set-secrets.sh --app-key-file ~/cr-ci-secrets/app-key.pem \
  --webhook-secret-file ~/cr-ci-secrets/webhook-secret --apply
# manual App setup without a GitHub-generated webhook secret:
scripts/set-secrets.sh --generate-webhook-secret ~/cr-ci-secrets/new-webhook-secret --apply
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
- Delete the local key and secret files once they are stored
  (`rm -r ~/cr-ci-secrets`). Rotation is in
  [operations.md](operations.md#secret-rotation).

## 7. Mediator: `scripts/deploy-mediator.sh`

```bash
scripts/deploy-mediator.sh --apply [--tag TAG] [--local] [--no-invoker-iam-check]
```

1. Refuses (with `--apply`) while `GITHUB_APP_ID` is still the placeholder
   `1`. Warns if the runner job is missing or runs another runner image than
   the current `runner/` tree: deploy the runner image and job **before** the
   mediator (see the upgrade order in
   [operations.md](operations.md#deploy-and-upgrade)).
2. Builds `mediator/Dockerfile` (pinned base images; static binary on
   distroless, non-root) like the runner image. The default tag is the tree
   hash of `mediator/` plus `go.mod`. Only `go.mod` and `mediator/` (without
   tests) are uploaded: `cr-ci/.gcloudignore` is an allowlist, so no
   `config.env` or stray file leaves the machine.
3. Deploys `MEDIATOR_SERVICE_NAME` as `MEDIATOR_SA_NAME` (C12):
   - `--max-instances=1` (**required**: claims live in the mediator's memory)
     and `--min-instances` from `MEDIATOR_MIN_INSTANCES` (default 0; 1 keeps it warm, see [operations](operations.md#mediator-single-instance)); 1 vCPU, 512 MiB, request timeout 300 s;
   - public ingress;
   - every config key as an env var (`--set-env-vars` with the `^;^`
     delimiter, because values contain commas and spaces). The secrets are
     passed **by name**; the mediator reads them from Secret Manager with its
     own identity.
4. Re-reads the service and stops if max-instances is not 1.
5. Grants `SCHEDULER_SA_NAME` `roles/run.invoker` on the service only (C13).
6. Prints the service URL. The GitHub App's webhook URL is that URL (or
   `MEDIATOR_URL`) **plus `/webhook`**; the bare URL gets 404 on delivery. If `MEDIATOR_URL` is empty, it tells you to set it
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

Two Cloud Run ephemeral-disk quotas apply ([Cloud Run docs](https://docs.cloud.google.com/run/docs/configuring/jobs/ephemeral-disk)):

| Quota (metric) | Default | Consumed as |
| :--- | :--- | :--- |
| Per instance: `run.googleapis.com/max_per_instance_ephemeral_disk` | 10 GiB | `RUNNER_DISK_GI` (one volume per execution) |
| Per region: `run.googleapis.com/ephemeral_disk_allocation` | 100 GiB (granted on first use) | `RUNNER_DISK_GI` x running executions |

`RUNNER_DISK_GI` accepts 1-100 and defaults to 10, which is the per-instance
default. **10 GiB is too small for a warm golangci-lint**: the P6 PoC measured
about 15-20 GiB on disk (the restored Go cache grows from 4.8 GB to about
8-9.5 GB during the lint, plus a 1.1 GB workspace, about 1.3 GB of modules and
up to 2 GB of Go temp dirs). Every lint run with 10 GiB ran out of space. Use
**at least 20 GiB** (P6b peaked at 16.5 GB on 23 GiB), after raising the
per-instance quota, or keep the build cache
in RAM (`RUNNER_BUILD_CACHE_IN_RAM=true`, then `deploy-job.sh --apply`; it costs
about 9.5 GB of the 32 GiB memory, see
[architecture](architecture.md#constraints)). A job above the per-instance quota is rejected at `deploy-job.sh`
("Total size of 'ephemeral-disk' volumes cannot exceed N GiB"), and nothing is
created or changed.

**The limit depends on the region and the grant.** It was 10 GiB in us-central1
and 23 GiB in us-west2 after the quota grant (a request for more was granted as
23), so `check-config.sh` cannot know it. `deploy-job.sh` notes when
`RUNNER_DISK_GI` is above 10, and when the create or update fails it explains the
error: N in the message is the limit in `REGION`. Set `RUNNER_DISK_GI` to at most
N, or ask for more. us-west2 runs with `RUNNER_DISK_GI=23` and
`MAX_CONCURRENT_RUNNERS=4` (92 GiB of the default 100 GiB regional allocation).

**Reading the quotas needs `cloudquotas.quotas.get`** (`roles/cloudquotas.viewer`,
project). A deployer without it (as in the us-west2 deployment) cannot run the
`gcloud beta quotas info` command below; the job create is then the only check
of the per-instance limit, and the regional allocation can only be assumed from
the default. Ask a project admin for the values, or for the viewer role.

To raise a quota (C15, `roles/cloudquotas.admin`; Google approves the request):

1. Find the quota ID (the `gcloud quotas` commands are beta:
   `gcloud components install beta`):

   ```bash
   gcloud beta quotas info list --service=run.googleapis.com --project=GCP_PROJECT \
     --filter='metric:run.googleapis.com/max_per_instance_ephemeral_disk OR metric:run.googleapis.com/ephemeral_disk_allocation' \
     --format='table(quotaId, metric, dimensions)'
   ```

2. Request the new value for `REGION`, for example 25 GiB per instance, and
   enough regional allocation for `MAX_CONCURRENT_RUNNERS` x 25 GiB. With the
   default regional 100 GiB, set `MAX_CONCURRENT_RUNNERS=4` at 25 GiB, or raise
   the regional quota first: otherwise the 5th concurrent runner fails to start
   (`launch_failed`, `RESOURCE_EXHAUSTED`) and its job waits for `/reconcile`.

   ```bash
   gcloud beta quotas preferences create --service=run.googleapis.com --project=GCP_PROJECT \
     --quota-id=QUOTA_ID_FROM_STEP_1 --dimensions=region=REGION --preferred-value=25 \
     --email=YOU@example.com --justification='cr-ci runners: warm golangci-lint needs ~20 GiB of scratch'
   ```

   The value is in the quota's unit as listed by step 1 (GiB for these two).
   **UNVERIFIED:** the exact `quotaId` strings and the `region` dimension key;
   read them from step 1's output. The console works too: IAM & Admin >
   Quotas, filter for the metric, **Edit quotas**.
3. After approval, set `RUNNER_DISK_GI` (and, within the regional allocation,
   `MAX_CONCURRENT_RUNNERS`) in `config.env`, then run
   `scripts/deploy-job.sh --apply` (disk) and `scripts/deploy-mediator.sh --apply`
   (concurrency).

The regional CPU and memory allocation quotas apply too if
`MAX_CONCURRENT_RUNNERS` x `RUNNER_CPU` / `RUNNER_MEMORY` exceed them; request
them the same way.

## 10. Teardown: `scripts/teardown.sh`

```bash
scripts/teardown.sh                    # read the transcript
scripts/teardown.sh --apply            # asks you to type GCP_PROJECT
scripts/teardown.sh --apply --keep-secrets --keep-images
scripts/teardown.sh --apply --regional-only   # one region's stack only
```

It deletes, in reverse order, skipping what is already gone:

1. the scheduler job;
2. the mediator service;
3. the runner job;
4. the Artifact Registry repository with all images (`--keep-images` keeps it
   and removes `BUILD_SA_NAME`'s binding);
5. the build staging bucket (uploaded sources only);
6. both secrets with all versions (`--keep-secrets` keeps them and removes
   `MEDIATOR_SA_NAME`'s access);
7. `BUILD_SA_NAME`'s project-level `logging.logWriter` binding;
8. the four service accounts.

`--regional-only` stops after step 4: it deletes only the scheduler job,
mediator service, runner job and Artifact Registry repository in `REGION`
(`--keep-images` keeps the repository), and keeps the service accounts, secrets,
staging bucket and project binding that a stack in another region still uses.
Use it to remove the old stack after a region move
([operations](operations.md#two-stacks-region-move)); `--keep-secrets` is refused
with it, since the secrets are always kept.

APIs stay enabled, since the project may use them. Uninstall the GitHub App
and clear the routing variable first
([github-setup.md, step 10](github-setup.md#10-removing-cr-ci-from-a-repository)),
so no job waits for a runner that no longer exists.
