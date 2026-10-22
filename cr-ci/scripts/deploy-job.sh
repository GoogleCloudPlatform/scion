#!/usr/bin/env bash
# deploy-job.sh: create or update the Cloud Run Job RUNNER_JOB_NAME that every
# runner execution starts from (permissions-matrix C8), then grant
# MEDIATOR_SA_NAME roles/run.developer on that job only (C10).
# Shape: RUNNER_CPU / RUNNER_MEMORY, one task, no retries, TASK_TIMEOUT, an
# ephemeral-disk volume of RUNNER_DISK_GI at /scratch, identity EXEC_SA_NAME.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

USAGE="Usage: deploy-job.sh [--tag TAG] [--config FILE] [--dry-run | --apply]

  --tag TAG  runner image tag (default: the build-runner-image.sh default)

$crci_common_help"

TAG=""
while (($#)); do
  case $1 in
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
)
VOLUME=(
  --add-volume="name=scratch,type=ephemeral-disk,size=${CFG[RUNNER_DISK_GI]}Gi"
  "--add-volume-mount=volume=scratch,mount-path=/scratch"
)

crci_note "C8: runner job $JOB"
if crci_exists gcloud run jobs describe "$JOB" --region="$R" --project="$P" --format='value(name)'; then
  crci_run gcloud run jobs update "$JOB" "${SHAPE[@]}" --clear-volumes --clear-volume-mounts "${VOLUME[@]}"
else
  crci_run gcloud run jobs create "$JOB" "${SHAPE[@]}" "${VOLUME[@]}"
fi

crci_note "C10: MEDIATOR_SA_NAME may run and cancel executions of this job only"
crci_run gcloud run jobs add-iam-policy-binding "$JOB" --region="$R" --project="$P" \
  --member="serviceAccount:$MEDIATOR" --role=roles/run.developer --format=none

crci_note "check: EXEC_SA_NAME holds no project roles"
crci_check_exec_sa_has_no_roles
crci_note "done; next: deploy-mediator.sh"
