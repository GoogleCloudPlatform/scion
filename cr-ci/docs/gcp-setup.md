# GCP setup (outline)

> **TODO(P4):** this is an outline. P4 adds the scripts and fills in each
> section with the exact commands (their `--dry-run` output). Roles per step are
> already final in [permissions-matrix.md](permissions-matrix.md#2-gcp-steps).

## 1. Prerequisites

- TODO(P4): choose `GCP_PROJECT` and `REGION` (design suggests
  `deploy-demo-test` / `us-central1` for the PoC; a dedicated project for
  production); deployer roles
  ([matrix 2.1](permissions-matrix.md#21-deployer-roles-least-privilege-bundle));
  copy `config.example.env` to `config.env` and fill it in.

## 2. Bootstrap (`scripts/bootstrap-gcp.sh`)

- TODO(P4): enable APIs; create the four service accounts; Artifact Registry
  repo; empty secrets; IAM bindings at resource level (design section 3.5,
  plus `roles/logging.logWriter` for the build SA).

## 3. Secrets

- TODO(P4): add the App private key and webhook secret versions from files or
  stdin (`scripts/put-secret.sh`); never as arguments.

## 4. Runner image (`scripts/build-runner-image.sh`)

- TODO(P4): Cloud Build as `BUILD_SA_NAME` with `CLOUD_LOGGING_ONLY`; tag scheme.

## 5. Runner job (`scripts/deploy-job.sh`)

- TODO(P4): `RUNNER_CPU`, `RUNNER_MEMORY`, ephemeral disk `RUNNER_DISK_GI` at
  `/scratch`, `--max-retries=0`, `--task-timeout=TASK_TIMEOUT`, service account
  `EXEC_SA_NAME`.

## 6. Mediator (`scripts/deploy-mediator.sh`)

- TODO(P4): env from the config contract, secrets by reference; public ingress
  (`allUsers` invoker, or `--no-invoker-iam-check` where org policy forbids
  `allUsers`: matrix item U6); print the service URL for `MEDIATOR_URL`.

## 7. Reconciler (`scripts/deploy-scheduler.sh`)

- TODO(P4): Cloud Scheduler `RECONCILER_JOB_NAME` on `RECONCILE_SCHEDULE`,
  OIDC as `SCHEDULER_SA_NAME` with audience `MEDIATOR_URL`.

## 8. Quotas

- TODO(P4): ephemeral-disk regional quota vs `RUNNER_DISK_GI` x
  `MAX_CONCURRENT_RUNNERS`; CPU and memory quotas; how to request an increase.

## 9. Teardown (`scripts/teardown.sh`)

- TODO(P4): reverse order; what is retained (secrets? images?).
