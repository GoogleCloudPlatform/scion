#!/usr/bin/env bash
# bootstrap-gcp.sh: one-time GCP setup for cr-ci (permissions-matrix C1-C7,
# C11, C19). Enables the APIs, creates the four service accounts, the Artifact
# Registry repo, the Cloud Build staging bucket and the two secret containers (no versions: set-secrets.sh adds
# them), and binds the resource-scoped IAM of matrix 2.2 that does not need the
# runner job or the mediator service to exist (deploy-job.sh binds C10,
# deploy-mediator.sh binds C13). Idempotent.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

USAGE="Usage: bootstrap-gcp.sh [--config FILE] [--dry-run | --apply]

Creates (if missing): APIs, service accounts BUILD/MEDIATOR/SCHEDULER/EXEC_SA_NAME,
Artifact Registry repo ARTIFACT_REPO, the build staging bucket
(BUILD_STAGING_BUCKET), secrets GITHUB_APP_PRIVATE_KEY_SECRET_NAME and
GITHUB_WEBHOOK_SECRET_NAME (empty), and the IAM bindings C4, C6, C7, C11, C19.

$crci_common_help"

while (($#)); do
  crci_parse_common_arg "$@"
  shift "$CRCI_SHIFT"
done
crci_help_exit "$USAGE"
crci_start

P=${CFG[GCP_PROJECT]}
R=${CFG[REGION]}
BUILD=$(crci_sa_email "${CFG[BUILD_SA_NAME]}")
MEDIATOR=$(crci_sa_email "${CFG[MEDIATOR_SA_NAME]}")
EXEC=$(crci_sa_email "${CFG[EXEC_SA_NAME]}")

crci_note "C1: APIs (only the missing ones are enabled, so serviceusage.services.enable is needed only then)"
APIS=(run.googleapis.com artifactregistry.googleapis.com secretmanager.googleapis.com
  cloudscheduler.googleapis.com cloudbuild.googleapis.com iam.googleapis.com storage.googleapis.com)
# Listing needs only serviceusage.services.list; a list error stops the script.
crci_read ENABLED_APIS "" gcloud services list --enabled --project="$P" --format='value(config.name)'
MISSING_APIS=()
for api in "${APIS[@]}"; do
  grep -qxF -- "$api" <<<"$ENABLED_APIS" || MISSING_APIS+=("$api")
done
if ((${#MISSING_APIS[@]} == 0)); then
  crci_note "all required APIs are enabled"
else
  [[ $CRCI_MODE == apply ]] || crci_note "dry-run lists every API; --apply enables only those the check above does not report"
  crci_run gcloud services enable "${MISSING_APIS[@]}" --project="$P"
fi

crci_note "C2: service accounts (EXEC_SA_NAME must stay at zero roles)"
create_sa() { # NAME DESCRIPTION
  local email
  email=$(crci_sa_email "$1")
  if crci_exists gcloud iam service-accounts describe "$email" --project="$P" --format='value(email)'; then
    crci_note "service account $email exists"
  else
    crci_run gcloud iam service-accounts create "$1" --project="$P" \
      --display-name="cr-ci $2" --description="cr-ci: $2"
  fi
}
create_sa "${CFG[BUILD_SA_NAME]}" "image builds (Cloud Build)"
create_sa "${CFG[MEDIATOR_SA_NAME]}" "mediator service"
create_sa "${CFG[SCHEDULER_SA_NAME]}" "reconcile scheduler"
create_sa "${CFG[EXEC_SA_NAME]}" "runner executions (no roles)"

crci_note "C3: Artifact Registry repository"
if crci_exists gcloud artifacts repositories describe "${CFG[ARTIFACT_REPO]}" --location="$R" --project="$P" --format='value(name)'; then
  crci_note "repository ${CFG[ARTIFACT_REPO]} exists"
else
  crci_run gcloud artifacts repositories create "${CFG[ARTIFACT_REPO]}" --repository-format=docker \
    --location="$R" --project="$P" --description="cr-ci runner and mediator images"
fi

BUCKET="gs://$(crci_staging_bucket)"
crci_note "C19: Cloud Build staging bucket (sources only; deleted after 7 days)"
if crci_exists gcloud storage buckets describe "$BUCKET" --project="$P" --format='value(name)'; then
  crci_run gcloud storage buckets update "$BUCKET" --project="$P" \
    --uniform-bucket-level-access --public-access-prevention --lifecycle-file=scripts/staging-lifecycle.json
else
  crci_run gcloud storage buckets create "$BUCKET" --project="$P" --location="$R" \
    --uniform-bucket-level-access --public-access-prevention --lifecycle-file=scripts/staging-lifecycle.json
fi

crci_note "C5: secret containers (versions come from set-secrets.sh)"
for s in "${CFG[GITHUB_APP_PRIVATE_KEY_SECRET_NAME]}" "${CFG[GITHUB_WEBHOOK_SECRET_NAME]}"; do
  if crci_exists gcloud secrets describe "$s" --project="$P" --format='value(name)'; then
    crci_note "secret $s exists"
  else
    crci_run gcloud secrets create "$s" --replication-policy=automatic --project="$P"
  fi
done

crci_note "C4: BUILD_SA_NAME may push to ARTIFACT_REPO only"
crci_run gcloud artifacts repositories add-iam-policy-binding "${CFG[ARTIFACT_REPO]}" --location="$R" --project="$P" \
  --member="serviceAccount:$BUILD" --role=roles/artifactregistry.writer --format=none
crci_note "C19: BUILD_SA_NAME reads the uploaded build sources in the staging bucket only"
crci_run gcloud storage buckets add-iam-policy-binding "$BUCKET" --project="$P" \
  --member="serviceAccount:$BUILD" --role=roles/storage.objectViewer --format=none
crci_note "C7: BUILD_SA_NAME writes build logs (CLOUD_LOGGING_ONLY)"
crci_run gcloud projects add-iam-policy-binding "$P" \
  --member="serviceAccount:$BUILD" --role=roles/logging.logWriter --condition=None --format=none

crci_note "C6: MEDIATOR_SA_NAME reads the two secrets only"
for s in "${CFG[GITHUB_APP_PRIVATE_KEY_SECRET_NAME]}" "${CFG[GITHUB_WEBHOOK_SECRET_NAME]}"; do
  crci_run gcloud secrets add-iam-policy-binding "$s" --project="$P" \
    --member="serviceAccount:$MEDIATOR" --role=roles/secretmanager.secretAccessor --format=none
done

crci_note "C11: MEDIATOR_SA_NAME may act as EXEC_SA_NAME only"
crci_run gcloud iam service-accounts add-iam-policy-binding "$EXEC" --project="$P" \
  --member="serviceAccount:$MEDIATOR" --role=roles/iam.serviceAccountUser --format=none

crci_note "check: EXEC_SA_NAME holds no project roles"
crci_check_exec_sa_has_no_roles
crci_note "done; next: set-secrets.sh, build-runner-image.sh"
