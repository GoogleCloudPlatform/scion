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

USAGE="Usage: deploy-mediator.sh [--tag TAG] [--local] [--no-invoker-iam-check] [--yes] [--config FILE] [--dry-run | --apply]

  --tag TAG               mediator image tag (default: git-<12 hex of mediator/ + go.mod at HEAD>)
  --local                 build with the local docker daemon instead of Cloud Build
  --no-invoker-iam-check  public ingress without the allUsers invoker binding, for
                          projects whose org policy forbids allUsers (permissions-matrix U6)
  --yes                   skip the confirmation prompt when --apply detects config changes
                          against the live mediator service

First deploy: MEDIATOR_URL may be empty; the service then answers 503 on
/webhook and /reconcile. Put the printed URL into MEDIATOR_URL and run again.

With --apply on an existing service, the script compares the live service's
environment against config.env, prints any differences, and prompts for
confirmation before building or deploying (pass --yes to skip the prompt).

$crci_common_help"

TAG=""
LOCAL=0
NO_INVOKER_CHECK=0
YES=0
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
    --yes)
      YES=1
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
APP_ID=$(_crci_int "$(crci_cfg GITHUB_APP_ID)")
if [[ $APP_ID == 1 ]]; then
  [[ $CRCI_MODE == apply ]] && crci_die "GITHUB_APP_ID is still the placeholder 1; create the GitHub App and set its ID first"
  crci_note "WARNING: GITHUB_APP_ID is the placeholder 1; --apply refuses to deploy until it is the real App ID"
fi

P=$(crci_cfg GCP_PROJECT)
R=$(crci_cfg REGION)
SVC=$(crci_cfg MEDIATOR_SERVICE_NAME)
RUNNER_JOB=$(crci_cfg RUNNER_JOB_NAME)
MEDIATOR_IMAGE=$(crci_cfg MEDIATOR_IMAGE_NAME)
MEDIATOR_MIN=$(crci_cfg MEDIATOR_MIN_INSTANCES)
MEDIATOR_URL=$(crci_cfg MEDIATOR_URL)
IMAGE=$(crci_image_ref "$MEDIATOR_IMAGE" "$TAG")
MEDIATOR=$(crci_sa_email "$(crci_cfg MEDIATOR_SA_NAME)")
SCHEDULER=$(crci_sa_email "$(crci_cfg SCHEDULER_SA_NAME)")

# Upgrade order: the mediator first, then the runner job (deploy-job.sh). A newer
# mediator only adds launch env that an older runner image ignores (GH_REPOS), while
# a newer runner image can require it and fails closed under an older mediator (exit
# 2, "GH_REPOS is not set"; redeploy #2, 2026-10-27). So this only reports whether
# the job still runs another image than the current runner/ tree: deploy it next.
crci_note "check: which runner image the job runs (upgrade order: the mediator first, then deploy-job.sh)"
if crci_exists gcloud run jobs describe "$RUNNER_JOB" --region="$R" --project="$P" --format='value(metadata.name)'; then
  JOB_IMAGE=""
  crci_read JOB_IMAGE "" gcloud run jobs describe "$RUNNER_JOB" --region="$R" --project="$P" \
    --format='value(spec.template.spec.template.spec.containers[0].image)'
  WANT_TAG=$(crci_try_default_tag runner)
  if [[ -n $WANT_TAG && -n $JOB_IMAGE && ${JOB_IMAGE##*:} != "$WANT_TAG" ]]; then
    printf '%s: NOTE: job %s runs %s, not the current runner tag %s; after this deploy, run build-runner-image.sh and deploy-job.sh (the mediator goes first).\n' \
      "$CRCI_PROG" "$RUNNER_JOB" "$JOB_IMAGE" "$WANT_TAG" >&2
  fi
elif [[ $CRCI_MODE == apply ]]; then
  printf '%s: WARNING: runner job %s not found; run deploy-job.sh before any job is routed.\n' "$CRCI_PROG" "$RUNNER_JOB" >&2
fi

# Diff config.env against the live mediator service before building or
# deploying: deploy-mediator.sh passes every key via --set-env-vars (replacing
# the full set), so an incomplete or stale config.env would silently overwrite
# live values. Dry-run prints the describe check only.
crci_note "check: diff config.env against live service $SVC (apply only)"
if crci_exists gcloud run services describe "$SVC" --region="$R" --project="$P" --format='value(metadata.name)'; then
  LIVE_ENV=""
  crci_read LIVE_ENV "" gcloud run services describe "$SVC" --region="$R" --project="$P" \
    --flatten='spec.template.spec.containers[0].env[]' \
    --format='value[separator="="](spec.template.spec.containers[0].env.name,spec.template.spec.containers[0].env.value)'

  LIVE_KEYS=()
  LIVE_VALS=()
  LIVE_EXTRA_KEYS=()
  while IFS= read -r line; do
    [[ -n $line ]] || continue
    if [[ $line == *=* ]]; then
      lk=${line%%=*}
      lv=${line#*=}
    else
      lk=$line
      lv=""
    fi
    found=0
    for ((i = 0; i < ${#LIVE_KEYS[@]}; i++)); do
      if [[ ${LIVE_KEYS[i]} == "$lk" ]]; then
        LIVE_VALS[i]=$lv
        found=1
        break
      fi
    done
    if ((found == 0)); then
      LIVE_KEYS+=("$lk")
      LIVE_VALS+=("$lv")
    fi
    if ! crci_is_known_key "$lk"; then
      LIVE_EXTRA_KEYS+=("$lk")
    fi
  done <<<"$LIVE_ENV"

  _live_lookup() {
    local key=$1 i
    for ((i = 0; i < ${#LIVE_KEYS[@]}; i++)); do
      if [[ ${LIVE_KEYS[i]} == "$key" ]]; then
        LIVE_VAL=${LIVE_VALS[i]}
        return 0
      fi
    done
    return 1
  }

  changed=0
  unchanged=0
  DIFF_LINES=()
  for k in "${CRCI_KEYS[@]}"; do
    want=""
    crci_cfg_into want "$k"
    LIVE_VAL=""
    if ! _live_lookup "$k"; then
      DIFF_LINES+=("  + $k=$want (new)")
      changed=$((changed + 1))
    elif [[ $LIVE_VAL != "$want" ]]; then
      DIFF_LINES+=("  - $k=$LIVE_VAL" "  + $k=$want")
      changed=$((changed + 1))
    else
      unchanged=$((unchanged + 1))
    fi
  done
  if ((${#LIVE_EXTRA_KEYS[@]} > 0)); then
    SORTED_EXTRA_KEYS=()
    while IFS= read -r ek; do
      [[ -n $ek ]] && SORTED_EXTRA_KEYS+=("$ek")
    done < <(printf '%s\n' "${LIVE_EXTRA_KEYS[@]}" | LC_ALL=C sort -u)
    for k in "${SORTED_EXTRA_KEYS[@]}"; do
      LIVE_VAL=""
      _live_lookup "$k" || true
      DIFF_LINES+=("  - $k=$LIVE_VAL (removed)")
      changed=$((changed + 1))
    done
  fi

  if ((changed == 0)); then
    crci_note "config matches live service $SVC ($unchanged keys unchanged)"
  else
    crci_note "config diff against live service $SVC ($changed changed, $unchanged unchanged):"
    for dl in "${DIFF_LINES[@]}"; do
      crci_note "$dl"
    done
    if [[ $YES == 1 ]]; then
      crci_note "--yes set; applying config changes without prompting"
    else
      CONFIRM=""
      printf 'Apply these config changes to %s in %s (%s)? [y/N] ' "$SVC" "$R" "$P" >&2
      IFS= read -r CONFIRM || CONFIRM=""
      case $CONFIRM in
        [yY] | [yY][eE][sS]) ;;
        *) crci_die "aborted; nothing deployed" ;;
      esac
    fi
  fi
elif [[ $CRCI_MODE == apply ]]; then
  crci_note "service $SVC does not exist yet; skipping live config diff"
fi

crci_note "mediator image $MEDIATOR_IMAGE:$TAG"
crci_build_image "$MEDIATOR_IMAGE" mediator/Dockerfile . "$TAG" "$LOCAL"

# Env: every key of the config contract, in config.example.env order. Values
# contain commas (TARGET_REPOS) and spaces (RECONCILE_SCHEDULE), so gcloud's
# alternate delimiter ^;^ is used; no config value can contain ';'.
ENV="^;^"
first=1
for k in "${CRCI_KEYS[@]}"; do
  [[ $first == 1 ]] || ENV+=";"
  first=0
  val=""
  crci_cfg_into val "$k"
  ENV+="$k=$val"
done

if [[ $NO_INVOKER_CHECK == 1 ]]; then
  ACCESS=(--no-allow-unauthenticated --no-invoker-iam-check)
else
  ACCESS=(--allow-unauthenticated)
fi

crci_note "C12: mediator service $SVC (max-instances=1 is required: claims live in memory)"
crci_run gcloud run deploy "$SVC" --image="$IMAGE" --region="$R" --project="$P" \
  --service-account="$MEDIATOR" \
  --min-instances="$MEDIATOR_MIN" --max-instances=1 --cpu=1 --memory=512Mi --concurrency=80 --timeout=300 \
  --ingress=all --port=8080 "${ACCESS[@]}" \
  --set-env-vars="$ENV" --labels=managed-by=cr-ci

crci_note "check: max-instances is 1"
MAXSCALE=""
crci_read MAXSCALE 1 gcloud run services describe "$SVC" --region="$R" --project="$P" \
  --format='value(spec.template.metadata.annotations."autoscaling.knative.dev/maxScale")'
[[ $MAXSCALE == 1 ]] || crci_die "service $SVC has max-instances '$MAXSCALE'; it must be 1"

crci_note "C13: SCHEDULER_SA_NAME may invoke this service only"
crci_run gcloud run services add-iam-policy-binding "$SVC" --region="$R" --project="$P" \
  --member="serviceAccount:$SCHEDULER" --role=roles/run.invoker --format=none

URL=""
crci_read URL "<url printed by the command above>" gcloud run services describe "$SVC" \
  --region="$R" --project="$P" --format='value(status.url)'
crci_note "service URL: $URL"
# A service answers on several URLs (status.url, which may be the hash form,
# and the deterministic https://SERVICE-PROJECT_NUMBER.REGION.run.app); any of
# them is a valid MEDIATOR_URL.
URLS=""
crci_read URLS "[]" gcloud run services describe "$SVC" --region="$R" --project="$P" \
  --format='value(metadata.annotations."run.googleapis.com/urls")'
if [[ $CRCI_MODE == apply ]]; then
  if [[ -z $MEDIATOR_URL ]]; then
    crci_note "MEDIATOR_URL is empty: set MEDIATOR_URL=$URL in $CRCI_CONFIG, re-run deploy-mediator.sh, then run deploy-scheduler.sh"
    crci_note "the GitHub App webhook URL is $URL/webhook"
  elif [[ $MEDIATOR_URL != "$URL" && $URLS != *"\"$MEDIATOR_URL\""* ]]; then
    printf '%s: WARNING: MEDIATOR_URL=%s is not one of the service URLs (%s %s).\n' "$CRCI_PROG" "$MEDIATOR_URL" "$URL" "$URLS" >&2
    printf '%s: WARNING: update MEDIATOR_URL in %s and the App webhook URL (%s/webhook), then re-run this script and deploy-scheduler.sh.\n' \
      "$CRCI_PROG" "$CRCI_CONFIG" "$URL" >&2
  else
    crci_note "MEDIATOR_URL matches; next: deploy-scheduler.sh"
  fi
fi
