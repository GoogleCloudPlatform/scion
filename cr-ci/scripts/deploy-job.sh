#!/usr/bin/env bash
# deploy-job.sh: create or update the Cloud Run Job RUNNER_JOB_NAME that every
# runner execution starts from (permissions-matrix C8), then grant
# MEDIATOR_SA_NAME roles/run.developer on that job only (C10).
# Shape: RUNNER_CPU / RUNNER_MEMORY, one task, no retries, TASK_TIMEOUT, an
# ephemeral-disk volume of RUNNER_DISK_GI mounted at the runner's HOME, /home/runner (the
# entrypoint creates the work folder and the caches on it; an image from before this
# layout refuses to start on such a job, and the current image refuses a job that still
# mounts it at /scratch: update the image and the job together, which this script does),
# identity EXEC_SA_NAME, and
# the env RUNNER_BUILD_CACHE_IN_RAM and RUNNER_IDLE_TIMEOUT_SECONDS (from RUNNER_IDLE_TIMEOUT)
# for the entrypoint (the job's only env vars:
# --set-env-vars replaces the template's env on update).
# --warm-up then runs one no-op execution (RUNNER_WARMUP=1: the entrypoint runs
# its checks and exits 0 before any claim), so the first real job does not pay
# the image import (~34 s on first use in the P6 PoC).
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

USAGE="Usage: deploy-job.sh [--tag TAG] [--warm-up] [--config FILE] [--dry-run | --apply]

  --tag TAG  runner image tag (default: the build-runner-image.sh default)
  --warm-up  after deploying, run one no-op execution (RUNNER_WARMUP=1) and wait
             for it, so the image is imported before the first real job

$crci_common_help"

TAG=""
WARMUP=0
while (($#)); do
  case $1 in
    --warm-up)
      WARMUP=1
      shift
      ;;
    --tag)
      crci_need_value --tag "$@"
      TAG=$2
      shift 2
      ;;
    *)
      crci_parse_common_arg "$@"
      shift "$CRCI_SHIFT"
      ;;
  esac
done
crci_help_exit "$USAGE"
crci_start
[[ -n $TAG ]] || TAG=$(crci_default_tag runner)

P=${CFG[GCP_PROJECT]}
R=${CFG[REGION]}
JOB=${CFG[RUNNER_JOB_NAME]}
IMAGE=$(crci_image_ref "${CFG[RUNNER_IMAGE_NAME]}" "$TAG")
EXEC=$(crci_sa_email "${CFG[EXEC_SA_NAME]}")
MEDIATOR=$(crci_sa_email "${CFG[MEDIATOR_SA_NAME]}")
TIMEOUT_S=$(crci_duration_seconds "${CFG[TASK_TIMEOUT]}")
IDLE_S=$(crci_duration_seconds "${CFG[RUNNER_IDLE_TIMEOUT]}")

crci_note "image must exist (build-runner-image.sh)"
if ! crci_exists gcloud artifacts docker images describe "$IMAGE" --project="$P" --format='value(image_summary.digest)'; then
  [[ $CRCI_MODE == apply ]] && crci_die "image $IMAGE not found; run build-runner-image.sh first"
fi

# The shape flags are the same for create and update. Volumes are replaced as
# a whole on update (UNVERIFIED that --clear-volumes and --add-volume combine
# in one call; permissions-matrix U9, checked in the P6 PoC).
SHAPE=(
  --image="$IMAGE" --region="$R" --project="$P"
  --service-account="$EXEC"
  --cpu="${CFG[RUNNER_CPU]}" --memory="${CFG[RUNNER_MEMORY]}"
  --tasks=1 --parallelism=1 --max-retries=0 --task-timeout="${TIMEOUT_S}s"
  --labels=managed-by=cr-ci
  --set-env-vars="RUNNER_BUILD_CACHE_IN_RAM=${CFG[RUNNER_BUILD_CACHE_IN_RAM]},RUNNER_IDLE_TIMEOUT_SECONDS=$IDLE_S"
)
VOLUME=(
  --add-volume="name=scratch,type=ephemeral-disk,size=${CFG[RUNNER_DISK_GI]}Gi"
  "--add-volume-mount=volume=scratch,mount-path=/home/runner"
)

# The per-job ephemeral-disk limit the server enforces depends on the region and
# its quota grant (10 GiB by default; us-west2 allowed 23 after the grant), and the
# deployer may not be able to read the quota (cloudquotas.quotas.get), so the
# create or update itself is the check.
DISK_HINT="if the error says \"Total size of 'ephemeral-disk' volumes cannot exceed N GiB\", N is the limit in $R: set RUNNER_DISK_GI to at most N, or request a higher per-instance quota (docs/gcp-setup.md, \"Quotas\")"
crci_note "C8: runner job $JOB"
if ((CFG[RUNNER_DISK_GI] > 10)); then
  crci_note "RUNNER_DISK_GI=${CFG[RUNNER_DISK_GI]} is above Cloud Run's 10 GiB per-instance default: it needs the per-instance quota raised in $R (the limit is per region)"
fi
if crci_exists gcloud run jobs describe "$JOB" --region="$R" --project="$P" --format='value(name)'; then
  crci_run_hint "$DISK_HINT" gcloud run jobs update "$JOB" "${SHAPE[@]}" --clear-volumes --clear-volume-mounts "${VOLUME[@]}"
else
  crci_run_hint "$DISK_HINT" gcloud run jobs create "$JOB" "${SHAPE[@]}" "${VOLUME[@]}"
fi

crci_note "C10: MEDIATOR_SA_NAME may run and cancel executions of this job only"
crci_run gcloud run jobs add-iam-policy-binding "$JOB" --region="$R" --project="$P" \
  --member="serviceAccount:$MEDIATOR" --role=roles/run.developer --format=none

crci_note "check: EXEC_SA_NAME holds no project roles"
crci_check_exec_sa_has_no_roles

if ((WARMUP)); then
  # A no-op execution: the entrypoint checks the image, the volume at HOME and the work
  # and cache directories
  # and exits 0 before any claim. The mediator ignores it (it has no RUNNER_NAME);
  # it counts toward MAX_CONCURRENT_RUNNERS for the few seconds it runs.
  crci_note "F-F: warm-up execution (imports the image before the first real job)"
  crci_run gcloud run jobs execute "$JOB" --region="$R" --project="$P" \
    --update-env-vars=RUNNER_WARMUP=1 --wait --format=none
fi
crci_note "done (upgrade order: deploy-mediator.sh goes before this when both changed)"
