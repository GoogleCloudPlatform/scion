#!/usr/bin/env bash
# deploy-mediator.sh: build the mediator image (mediator/Dockerfile) and deploy
# the Cloud Run service MEDIATOR_SERVICE_NAME as MEDIATOR_SA_NAME
# (permissions-matrix C12), then grant SCHEDULER_SA_NAME roles/run.invoker on
# it (C13). max-instances is always 1 (claims live in the mediator's memory),
# min-instances MEDIATOR_MIN_INSTANCES (0 or 1). Every config key is passed as an env var; the secrets are
# passed BY NAME (the mediator reads them from Secret Manager itself).
# Prints the service URL for MEDIATOR_URL.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

USAGE="Usage: deploy-mediator.sh [--tag TAG] [--local] [--no-invoker-iam-check] [--config FILE] [--dry-run | --apply]

  --tag TAG               mediator image tag (default: git-<12 hex of mediator/ + go.mod at HEAD>)
  --local                 build with the local docker daemon instead of Cloud Build
  --no-invoker-iam-check  public ingress without the allUsers invoker binding, for
                          projects whose org policy forbids allUsers (permissions-matrix U6)

First deploy: MEDIATOR_URL may be empty; the service then answers 503 on
/webhook and /reconcile. Put the printed URL into MEDIATOR_URL and run again.

$crci_common_help"

TAG=""
LOCAL=0
NO_INVOKER_CHECK=0
while (($#)); do
  case $1 in
    --tag)
      crci_need_value --tag "$@"
      TAG=$2
      shift 2
      ;;
    --local)
      LOCAL=1
      shift
      ;;
    --no-invoker-iam-check)
      NO_INVOKER_CHECK=1
      shift
      ;;
    *)
      crci_parse_common_arg "$@"
      shift "$CRCI_SHIFT"
      ;;
  esac
done
crci_help_exit "$USAGE"
crci_start
[[ -n $TAG ]] || TAG=$(crci_default_tag mediator go.mod)

# GITHUB_APP_ID=1 is the documented placeholder for the steps before the App
# exists; a mediator deployed with it gets 401s from GitHub.
APP_ID=$(_crci_int "${CFG[GITHUB_APP_ID]}")
if [[ $APP_ID == 1 ]]; then
  [[ $CRCI_MODE == apply ]] && crci_die "GITHUB_APP_ID is still the placeholder 1; create the GitHub App and set its ID first"
  crci_note "WARNING: GITHUB_APP_ID is the placeholder 1; --apply refuses to deploy until it is the real App ID"
fi

P=${CFG[GCP_PROJECT]}
R=${CFG[REGION]}
SVC=${CFG[MEDIATOR_SERVICE_NAME]}
IMAGE=$(crci_image_ref "${CFG[MEDIATOR_IMAGE_NAME]}" "$TAG")
MEDIATOR=$(crci_sa_email "${CFG[MEDIATOR_SA_NAME]}")
SCHEDULER=$(crci_sa_email "${CFG[SCHEDULER_SA_NAME]}")

# Upgrade order: the mediator first, then the runner job (deploy-job.sh). A newer
# mediator only adds launch env that an older runner image ignores (GH_REPOS), while
# a newer runner image can require it and fails closed under an older mediator (exit
# 2, "GH_REPOS is not set"; redeploy #2, 2026-10-27). So this only reports whether
# the job still runs another image than the current runner/ tree: deploy it next.
crci_note "check: which runner image the job runs (upgrade order: the mediator first, then deploy-job.sh)"
if crci_exists gcloud run jobs describe "${CFG[RUNNER_JOB_NAME]}" --region="$R" --project="$P" --format='value(metadata.name)'; then
  crci_read JOB_IMAGE "" gcloud run jobs describe "${CFG[RUNNER_JOB_NAME]}" --region="$R" --project="$P" \
    --format='value(spec.template.spec.template.spec.containers[0].image)'
  WANT_TAG=$(crci_try_default_tag runner)
  if [[ -n $WANT_TAG && -n $JOB_IMAGE && ${JOB_IMAGE##*:} != "$WANT_TAG" ]]; then
    printf '%s: NOTE: job %s runs %s, not the current runner tag %s; after this deploy, run build-runner-image.sh and deploy-job.sh (the mediator goes first).\n' \
      "$CRCI_PROG" "${CFG[RUNNER_JOB_NAME]}" "$JOB_IMAGE" "$WANT_TAG" >&2
  fi
elif [[ $CRCI_MODE == apply ]]; then
  printf '%s: WARNING: runner job %s not found; run deploy-job.sh before any job is routed.\n' "$CRCI_PROG" "${CFG[RUNNER_JOB_NAME]}" >&2
fi

crci_note "mediator image ${CFG[MEDIATOR_IMAGE_NAME]}:$TAG"
crci_build_image "${CFG[MEDIATOR_IMAGE_NAME]}" mediator/Dockerfile . "$TAG" "$LOCAL"

# Env: every key of the config contract, in config.example.env order. Values
# contain commas (TARGET_REPOS) and spaces (RECONCILE_SCHEDULE), so gcloud's
# alternate delimiter ^;^ is used; no config value can contain ';'.
ENV="^;^"
first=1
for k in "${CRCI_KEYS[@]}"; do
  [[ $first == 1 ]] || ENV+=";"
  first=0
  ENV+="$k=${CFG[$k]}"
done

if [[ $NO_INVOKER_CHECK == 1 ]]; then
  ACCESS=(--no-allow-unauthenticated --no-invoker-iam-check)
else
  ACCESS=(--allow-unauthenticated)
fi

crci_note "C12: mediator service $SVC (max-instances=1 is required: claims live in memory)"
crci_run gcloud run deploy "$SVC" --image="$IMAGE" --region="$R" --project="$P" \
  --service-account="$MEDIATOR" \
  --min-instances="${CFG[MEDIATOR_MIN_INSTANCES]}" --max-instances=1 --cpu=1 --memory=512Mi --concurrency=80 --timeout=300 \
  --ingress=all --port=8080 "${ACCESS[@]}" \
  --set-env-vars="$ENV" --labels=managed-by=cr-ci

crci_note "check: max-instances is 1"
crci_read MAXSCALE 1 gcloud run services describe "$SVC" --region="$R" --project="$P" \
  --format='value(spec.template.metadata.annotations."autoscaling.knative.dev/maxScale")'
[[ $MAXSCALE == 1 ]] || crci_die "service $SVC has max-instances '$MAXSCALE'; it must be 1"

crci_note "C13: SCHEDULER_SA_NAME may invoke this service only"
crci_run gcloud run services add-iam-policy-binding "$SVC" --region="$R" --project="$P" \
  --member="serviceAccount:$SCHEDULER" --role=roles/run.invoker --format=none

crci_read URL "<url printed by the command above>" gcloud run services describe "$SVC" \
  --region="$R" --project="$P" --format='value(status.url)'
crci_note "service URL: $URL"
# A service answers on several URLs (status.url, which may be the hash form,
# and the deterministic https://SERVICE-PROJECT_NUMBER.REGION.run.app); any of
# them is a valid MEDIATOR_URL.
crci_read URLS "[]" gcloud run services describe "$SVC" --region="$R" --project="$P" \
  --format='value(metadata.annotations."run.googleapis.com/urls")'
if [[ $CRCI_MODE == apply ]]; then
  if [[ -z ${CFG[MEDIATOR_URL]} ]]; then
    crci_note "MEDIATOR_URL is empty: set MEDIATOR_URL=$URL in $CRCI_CONFIG, re-run deploy-mediator.sh, then run deploy-scheduler.sh"
    crci_note "the GitHub App webhook URL is $URL/webhook"
  elif [[ ${CFG[MEDIATOR_URL]} != "$URL" && $URLS != *"\"${CFG[MEDIATOR_URL]}\""* ]]; then
    printf '%s: WARNING: MEDIATOR_URL=%s is not one of the service URLs (%s %s).\n' "$CRCI_PROG" "${CFG[MEDIATOR_URL]}" "$URL" "$URLS" >&2
    printf '%s: WARNING: update MEDIATOR_URL in %s and the App webhook URL (%s/webhook), then re-run this script and deploy-scheduler.sh.\n' \
      "$CRCI_PROG" "$CRCI_CONFIG" "$URL" >&2
  else
    crci_note "MEDIATOR_URL matches; next: deploy-scheduler.sh"
  fi
fi
