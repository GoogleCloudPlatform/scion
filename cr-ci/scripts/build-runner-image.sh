#!/usr/bin/env bash
# build-runner-image.sh: build the runner image from runner/ (versions pinned in
# runner/Dockerfile) and push it to ARTIFACT_REPO as BUILD_SA_NAME
# (permissions-matrix C7). Default: Cloud Build; --local: docker build + push.
# Idempotent: an existing tag is not rebuilt.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

USAGE="Usage: build-runner-image.sh [--tag TAG] [--local] [--config FILE] [--dry-run | --apply]

  --tag TAG  image tag (default: git-<12 hex of the runner/ tree at HEAD>)
  --local    build with the local docker daemon and push as BUILD_SA_NAME
             (needs roles/iam.serviceAccountTokenCreator on it), instead of Cloud Build

Pushes REGION-docker.pkg.dev/GCP_PROJECT/ARTIFACT_REPO/RUNNER_IMAGE_NAME:TAG.

$crci_common_help"

TAG=""
LOCAL=0
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
    *)
      crci_parse_common_arg "$@"
      shift "$CRCI_SHIFT"
      ;;
  esac
done
crci_help_exit "$USAGE"
crci_start
[[ -n $TAG ]] || TAG=$(crci_default_tag runner)

crci_note "C7: runner image ${CFG[RUNNER_IMAGE_NAME]}:$TAG"
crci_build_image "${CFG[RUNNER_IMAGE_NAME]}" Dockerfile runner "$TAG" "$LOCAL"
crci_note "done; next: deploy-mediator.sh if the mediator changed too, then deploy-job.sh --tag $TAG"
