#!/usr/bin/env bash
# teardown.sh: delete what the cr-ci scripts created, in reverse order
# (permissions-matrix C18): scheduler job, mediator service, runner job,
# Artifact Registry repo (with its images), secrets, the project-level log
# binding of BUILD_SA_NAME, and the four service accounts. APIs stay enabled
# (the project may use them). Dry-run by default; --apply asks you to type
# GCP_PROJECT (or pass --confirm GCP_PROJECT). Missing resources are skipped.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

USAGE="Usage: teardown.sh [--keep-secrets] [--keep-images] [--confirm PROJECT] [--config FILE] [--dry-run | --apply]

  --keep-secrets     keep the two secrets (and their versions); MEDIATOR_SA_NAME's
                     access bindings are removed before the account is deleted
  --keep-images      keep the Artifact Registry repo and its images
  --confirm PROJECT  skip the typed confirmation (must equal GCP_PROJECT)

$crci_common_help"

KEEP_SECRETS=0
KEEP_IMAGES=0
CONFIRM=""
while (($#)); do
  case $1 in
    --keep-secrets)
      KEEP_SECRETS=1
      shift
      ;;
    --keep-images)
      KEEP_IMAGES=1
      shift
      ;;
    --confirm)
      crci_need_value "$1" "$@"
      CONFIRM=$2
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

P=${CFG[GCP_PROJECT]}
R=${CFG[REGION]}

if [[ $CRCI_MODE == apply ]]; then
  if [[ -z $CONFIRM ]]; then
    printf 'This deletes the cr-ci resources in project %s. Type the project ID to confirm: ' "$P" >&2
    IFS= read -r CONFIRM || CONFIRM=""
  fi
  [[ $CONFIRM == "$P" ]] || crci_die "confirmation does not match $P; nothing deleted"
fi

# delete_if_exists DESCRIBE_ARGS... -- DELETE_ARGS...
delete_if_exists() {
  local -a check=() del=()
  while [[ $1 != -- ]]; do
    check+=("$1")
    shift
  done
  shift
  del=("$@")
  if crci_exists "${check[@]}" || [[ $CRCI_MODE != apply ]]; then
    crci_run "${del[@]}"
  else
    crci_note "absent: ${check[*]:0:4}"
  fi
}

crci_note "reconciler ${CFG[RECONCILER_JOB_NAME]}"
delete_if_exists gcloud scheduler jobs describe "${CFG[RECONCILER_JOB_NAME]}" --location="$R" --project="$P" --format='value(name)' -- \
  gcloud scheduler jobs delete "${CFG[RECONCILER_JOB_NAME]}" --location="$R" --project="$P" --quiet

crci_note "mediator service ${CFG[MEDIATOR_SERVICE_NAME]}"
delete_if_exists gcloud run services describe "${CFG[MEDIATOR_SERVICE_NAME]}" --region="$R" --project="$P" --format='value(metadata.name)' -- \
  gcloud run services delete "${CFG[MEDIATOR_SERVICE_NAME]}" --region="$R" --project="$P" --quiet

crci_note "runner job ${CFG[RUNNER_JOB_NAME]}"
delete_if_exists gcloud run jobs describe "${CFG[RUNNER_JOB_NAME]}" --region="$R" --project="$P" --format='value(name)' -- \
  gcloud run jobs delete "${CFG[RUNNER_JOB_NAME]}" --region="$R" --project="$P" --quiet

if [[ $KEEP_IMAGES == 1 ]]; then
  crci_note "keeping repository ${CFG[ARTIFACT_REPO]}; removing BUILD_SA_NAME's writer binding"
  crci_run gcloud artifacts repositories remove-iam-policy-binding "${CFG[ARTIFACT_REPO]}" --location="$R" --project="$P" \
    --member="serviceAccount:$(crci_sa_email "${CFG[BUILD_SA_NAME]}")" --role=roles/artifactregistry.writer --format=none
else
  crci_note "repository ${CFG[ARTIFACT_REPO]} and its images"
  delete_if_exists gcloud artifacts repositories describe "${CFG[ARTIFACT_REPO]}" --location="$R" --project="$P" --format='value(name)' -- \
    gcloud artifacts repositories delete "${CFG[ARTIFACT_REPO]}" --location="$R" --project="$P" --quiet
fi

for s in "${CFG[GITHUB_APP_PRIVATE_KEY_SECRET_NAME]}" "${CFG[GITHUB_WEBHOOK_SECRET_NAME]}"; do
  if [[ $KEEP_SECRETS == 1 ]]; then
    crci_note "keeping secret $s; removing MEDIATOR_SA_NAME's access"
    crci_run gcloud secrets remove-iam-policy-binding "$s" --project="$P" \
      --member="serviceAccount:$(crci_sa_email "${CFG[MEDIATOR_SA_NAME]}")" --role=roles/secretmanager.secretAccessor --format=none
  else
    crci_note "secret $s"
    delete_if_exists gcloud secrets describe "$s" --project="$P" --format='value(name)' -- \
      gcloud secrets delete "$s" --project="$P" --quiet
  fi
done

crci_note "project binding of BUILD_SA_NAME (logging.logWriter)"
crci_run gcloud projects remove-iam-policy-binding "$P" \
  --member="serviceAccount:$(crci_sa_email "${CFG[BUILD_SA_NAME]}")" --role=roles/logging.logWriter --condition=None --format=none

for k in EXEC_SA_NAME SCHEDULER_SA_NAME MEDIATOR_SA_NAME BUILD_SA_NAME; do
  email=$(crci_sa_email "${CFG[$k]}")
  crci_note "service account $email"
  delete_if_exists gcloud iam service-accounts describe "$email" --project="$P" --format='value(email)' -- \
    gcloud iam service-accounts delete "$email" --project="$P" --quiet
done
crci_note "done; APIs were left enabled"
