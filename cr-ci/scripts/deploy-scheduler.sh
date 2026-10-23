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
[[ -n ${CFG[MEDIATOR_URL]} ]] || crci_die "MEDIATOR_URL is empty; run deploy-mediator.sh and put the printed URL into the config"

P=${CFG[GCP_PROJECT]}
R=${CFG[REGION]}
NAME=${CFG[RECONCILER_JOB_NAME]}
URL=${CFG[MEDIATOR_URL]}
SCHEDULER=$(crci_sa_email "${CFG[SCHEDULER_SA_NAME]}")

FLAGS=(
  --location="$R" --project="$P"
  --schedule="${CFG[RECONCILE_SCHEDULE]}" --time-zone=Etc/UTC
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
