#!/usr/bin/env bash
# deploy-scheduler.sh: create or update the Cloud Scheduler job
# RECONCILER_JOB_NAME that POSTs MEDIATOR_URL/reconcile on RECONCILE_SCHEDULE
# with an OIDC token for SCHEDULER_SA_NAME, audience MEDIATOR_URL
# (permissions-matrix C14). The mediator verifies that token in code.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

USAGE="Usage: deploy-scheduler.sh [--config FILE] [--dry-run | --apply]

Needs MEDIATOR_URL (printed by deploy-mediator.sh).

$crci_common_help"

while (($#)); do
  crci_parse_common_arg "$@"
  shift "$CRCI_SHIFT"
done
crci_help_exit "$USAGE"
crci_start
URL=$(crci_cfg MEDIATOR_URL)
[[ -n $URL ]] || crci_die "MEDIATOR_URL is empty; run deploy-mediator.sh and put the printed URL into the config"

P=$(crci_cfg GCP_PROJECT)
R=$(crci_cfg REGION)
NAME=$(crci_cfg RECONCILER_JOB_NAME)
SCHEDULER=$(crci_sa_email "$(crci_cfg SCHEDULER_SA_NAME)")
SCHEDULE=$(crci_cfg RECONCILE_SCHEDULE)

FLAGS=(
  --location="$R" --project="$P"
  --schedule="$SCHEDULE" --time-zone=Etc/UTC
  --uri="$URL/reconcile" --http-method=POST
  --oidc-service-account-email="$SCHEDULER" --oidc-token-audience="$URL"
  --attempt-deadline=300s
)

crci_note "C14: reconciler $NAME"
if crci_exists gcloud scheduler jobs describe "$NAME" --location="$R" --project="$P" --format='value(name)'; then
  crci_run gcloud scheduler jobs update http "$NAME" "${FLAGS[@]}"
else
  crci_run gcloud scheduler jobs create http "$NAME" "${FLAGS[@]}" --description="cr-ci: backfill and reap (POST /reconcile)"
fi
crci_note "done"
