#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Bootstrap helper for Scion's GKE Continuous Delivery pipeline (deploy/cd/).
#
# Idempotently provisions:
#   1. Required GCP APIs (Cloud Build, Cloud Scheduler, Artifact Registry, IAM).
#   2. Hardened GCS Terraform state bucket (<project>-<prefix>-tfstate) with
#      Uniform Bucket-Level Access, Object Versioning, and Public Access Prevention.
#   3. Dedicated least-privilege CD Service Account (<prefix>-cd-deployer) —
#      leaving the default <number>@cloudbuild.gserviceaccount.com untouched —
#      with bucket-scoped, repo-scoped, and IAM-Condition-scoped grants.
#   4. Optional Cloud Build Trigger + dedicated Cloud Scheduler SA + daily job
#      when --repository (2nd-gen) or --github-owner/--github-repo (1st-gen) is passed.
#
# Pass --dry-run to preview every gcloud command without mutating GCP.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
source "${SCRIPT_DIR}/lib.sh"

PROJECT=""
REGION="us-central1"
SHARED_PREFIX=""
HUB_NAME=""
CHANNEL="nightly"
VERSION_TAG=""
AGENT_PLATFORMS="linux/amd64"
REPO_URL="https://github.com/GoogleCloudPlatform/scion.git"
STATE_BUCKET=""
CD_SA_NAME=""
SCHEDULER_SA_NAME=""
TRIGGER_NAME=""
SCHEDULER_JOB_NAME=""
SCHEDULE_CRON="0 5 * * *"
REPOSITORY_2ND_GEN=""
GITHUB_OWNER=""
GITHUB_REPO=""
TRIGGER_BRANCH="main"
PROJECT_NUMBER_OVERRIDE=""
BOOTSTRAP_STORAGE_ADMIN="false"
REVOKE_BOOTSTRAP_STORAGE_ADMIN="false"
DRY_RUN="false"

usage() {
  cat <<EOF
Usage: $(basename "$0") --project <id> --shared-prefix <prefix> --hub-name <name> [options]

Bootstrap the GCS state bucket, least-privilege CD service account, and optional
Cloud Build Trigger + Cloud Scheduler daily job for deploy/cd/cloudbuild-gke-cd.yaml.

Required:
  --project <id>             Target GCP project ID
  --shared-prefix <prefix>   shared-infra name_prefix (2-8 lowercase alphanumeric chars, no hyphens; e.g. cdha)
  --hub-name <name>          Hub name starting with <shared-prefix>- (e.g. cdha-gke-1)

Options:
  --region <region>          GCP region matching shared-infra (default: us-central1)
  --state-bucket <bucket>    GCS Terraform state bucket (default: <project>-<shared-prefix>-tfstate)
  --channel <ch>             Release channel: nightly | preview | stable | '' (default: nightly)
  --version-tag <tag>        Optional pinned release tag override (default: '')
  --agent-platforms <plats>  Agent image platforms (default: linux/amd64)
  --repo-url <url>           Git repository URL cloned at tag time (default: https://github.com/GoogleCloudPlatform/scion.git)
  --cd-sa-name <id>          Dedicated CD service account ID (default: <shared-prefix>-cd-deployer)
  --scheduler-sa-name <id>   Dedicated Cloud Scheduler SA ID (default: <shared-prefix>-cd-scheduler)
  --trigger-name <name>      Cloud Build trigger name (default: <hub-name>-cd)
  --scheduler-job-name <n>   Cloud Scheduler job name (default: <hub-name>-cd-daily)
  --schedule <cron>          UTC cron schedule (default: '0 5 * * *' — 2h after 03:00 UTC nightly cut)
  --repository <res>         2nd-gen Cloud Build repository resource path to create trigger automatically
                             (projects/<p>/locations/<r>/connections/<c>/repositories/<repo>)
  --github-owner <owner>     1st-gen Cloud Build GitHub App owner (use with --github-repo)
  --github-repo <repo>       1st-gen Cloud Build GitHub App repo name (use with --github-owner)
  --trigger-branch <branch>  Branch hosting deploy/cd/cloudbuild-gke-cd.yaml (default: main)
  --bootstrap-storage-admin  Temporarily grant unconditioned roles/storage.admin so Pass 1 can run
                             project-level storage.buckets.create for <project>-<hub-name>-artifacts
  --revoke-bootstrap-storage-admin
                             Remove temporary unconditioned roles/storage.admin after Pass 1 creates
                             <project>-<hub-name>-artifacts (leaving only the bucket-prefix condition)
  --project-number <num>     Explicit GCP project number (used in --dry-run without live gcloud lookup)
  --dry-run                  Print commands without executing any GCP mutations
  -h, --help                 Show this help text
EOF
  exit "${1:-0}"
}

need_arg() {
  [[ "$2" -ge 2 ]] || die "Option $1 requires a value"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)            need_arg "$1" $#; PROJECT="$2"; shift 2 ;;
    --region)             need_arg "$1" $#; REGION="$2"; shift 2 ;;
    --shared-prefix)      need_arg "$1" $#; SHARED_PREFIX="$2"; shift 2 ;;
    --hub-name)           need_arg "$1" $#; HUB_NAME="$2"; shift 2 ;;
    --state-bucket)       need_arg "$1" $#; STATE_BUCKET="$2"; shift 2 ;;
    --channel)            need_arg "$1" $#; CHANNEL="$2"; shift 2 ;;
    --version-tag)        need_arg "$1" $#; VERSION_TAG="$2"; shift 2 ;;
    --agent-platforms)    need_arg "$1" $#; AGENT_PLATFORMS="$2"; shift 2 ;;
    --repo-url)           need_arg "$1" $#; REPO_URL="$2"; shift 2 ;;
    --cd-sa-name)         need_arg "$1" $#; CD_SA_NAME="$2"; shift 2 ;;
    --scheduler-sa-name)  need_arg "$1" $#; SCHEDULER_SA_NAME="$2"; shift 2 ;;
    --trigger-name)       need_arg "$1" $#; TRIGGER_NAME="$2"; shift 2 ;;
    --scheduler-job-name) need_arg "$1" $#; SCHEDULER_JOB_NAME="$2"; shift 2 ;;
    --schedule)           need_arg "$1" $#; SCHEDULE_CRON="$2"; shift 2 ;;
    --repository)         need_arg "$1" $#; REPOSITORY_2ND_GEN="$2"; shift 2 ;;
    --github-owner)       need_arg "$1" $#; GITHUB_OWNER="$2"; shift 2 ;;
    --github-repo)        need_arg "$1" $#; GITHUB_REPO="$2"; shift 2 ;;
    --trigger-branch)     need_arg "$1" $#; TRIGGER_BRANCH="$2"; shift 2 ;;
    --bootstrap-storage-admin)        BOOTSTRAP_STORAGE_ADMIN="true"; shift ;;
    --revoke-bootstrap-storage-admin) REVOKE_BOOTSTRAP_STORAGE_ADMIN="true"; shift ;;
    --project-number)     need_arg "$1" $#; PROJECT_NUMBER_OVERRIDE="$2"; shift 2 ;;
    --dry-run)            DRY_RUN="true"; shift ;;
    -h|--help)            usage 0 ;;
    *)                    die "Unknown option: $1 (run with --help for usage)" ;;
  esac
done

[[ -n "${PROJECT}" ]] || die "--project is required"
[[ "${PROJECT}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] \
  || die "--project '${PROJECT}' is not a valid GCP project ID"

if [[ "${BOOTSTRAP_STORAGE_ADMIN}" == "true" && "${REVOKE_BOOTSTRAP_STORAGE_ADMIN}" == "true" ]]; then
  die "Specify at most one of --bootstrap-storage-admin or --revoke-bootstrap-storage-admin"
fi

if [[ -z "${STATE_BUCKET}" ]]; then
  STATE_BUCKET="${PROJECT}-${SHARED_PREFIX}-tfstate"
fi

_STATE_BUCKET="${STATE_BUCKET}"
_SHARED_PREFIX="${SHARED_PREFIX}"
_HUB_NAME="${HUB_NAME}"
export _STATE_BUCKET _SHARED_PREFIX _HUB_NAME
validate_cd_common_inputs
validate_agent_platforms "${AGENT_PLATFORMS}"

[[ -n "${TRIGGER_BRANCH}" && ! "${TRIGGER_BRANCH}" =~ [[:space:][:cntrl:]] ]] \
  || die "--trigger-branch must be a non-empty branch name without whitespace or control characters"

case "${CHANNEL}" in
  nightly|preview|stable|"") ;;
  *) die "--channel must be nightly, preview, stable, or ''" ;;
esac
if [[ -z "${CHANNEL}" && -z "${VERSION_TAG}" ]]; then
  die "Either --channel or --version-tag must be non-empty"
fi
if [[ -n "${VERSION_TAG}" ]]; then
  is_valid_release_tag "${VERSION_TAG}" || die "--version-tag '${VERSION_TAG}' is not a valid Scion release tag"
fi
if [[ -n "${REPOSITORY_2ND_GEN}" && ( -n "${GITHUB_OWNER}" || -n "${GITHUB_REPO}" ) ]]; then
  die "Specify either --repository (2nd-gen) or --github-owner/--github-repo (1st-gen), not both"
fi
if [[ ( -n "${GITHUB_OWNER}" && -z "${GITHUB_REPO}" ) || ( -z "${GITHUB_OWNER}" && -n "${GITHUB_REPO}" ) ]]; then
  die "--github-owner and --github-repo must be provided together"
fi

AR_REPO="${SHARED_PREFIX}-scion"
REGISTRY="${REGION}-docker.pkg.dev/${PROJECT}/${AR_REPO}"
validate_registry_path "${REGISTRY}"

[[ -n "${CD_SA_NAME}" ]] || CD_SA_NAME="${SHARED_PREFIX}-cd-deployer"
[[ -n "${SCHEDULER_SA_NAME}" ]] || SCHEDULER_SA_NAME="${SHARED_PREFIX}-cd-scheduler"
[[ -n "${TRIGGER_NAME}" ]] || TRIGGER_NAME="${HUB_NAME}-cd"
[[ -n "${SCHEDULER_JOB_NAME}" ]] || SCHEDULER_JOB_NAME="${HUB_NAME}-cd-daily"

CD_SA_EMAIL="${CD_SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
SCHEDULER_SA_EMAIL="${SCHEDULER_SA_NAME}@${PROJECT}.iam.gserviceaccount.com"

run_cmd() {
  printf '+ '
  local arg
  for arg in "$@"; do
    printf '%q ' "${arg}"
  done
  printf '\n'
  if [[ "${DRY_RUN}" != "true" ]]; then
    "$@"
  fi
}

if [[ "${REVOKE_BOOTSTRAP_STORAGE_ADMIN}" == "true" ]]; then
  log_info "Revoking temporary unconditioned roles/storage.admin from ${CD_SA_EMAIL} (keeping bucket-prefix IAM Condition)..."
  run_cmd gcloud projects remove-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${CD_SA_EMAIL}" \
    --role="roles/storage.admin" \
    --condition=None \
    --quiet
  log_info "Unconditioned roles/storage.admin removed from ${CD_SA_EMAIL}."
  exit 0
fi

PROJECT_NUMBER="${PROJECT_NUMBER_OVERRIDE}"
if [[ -z "${PROJECT_NUMBER}" ]]; then
  if [[ "${DRY_RUN}" == "true" ]]; then
    PROJECT_NUMBER="PROJECT_NUMBER"
  else
    PROJECT_NUMBER="$(gcloud projects describe "${PROJECT}" --format='value(projectNumber)')"
    [[ "${PROJECT_NUMBER}" =~ ^[0-9]+$ ]] || die "Could not resolve numeric projectNumber for ${PROJECT}"
  fi
fi

log_info "Bootstrapping Scion GKE CD infrastructure (dry_run=${DRY_RUN})"
log_info "  Project:        ${PROJECT} (number: ${PROJECT_NUMBER})"
log_info "  Region:         ${REGION}"
log_info "  Shared prefix:  ${SHARED_PREFIX}"
log_info "  Hub name:       ${HUB_NAME}"
log_info "  State bucket:   gs://${STATE_BUCKET}"
log_info "  Registry:       ${REGISTRY}"
log_info "  CD SA:          ${CD_SA_EMAIL}"

# Read-only operator permission preflight (skipped in --dry-run) so missing
# permissions fail fast before mutating APIs, buckets, service accounts, or IAM.
run_operator_permission_preflight() {
  log_info "Preflight: verifying operator IAM permissions before mutating GCP..."
  local token=""
  token="$(gcloud auth print-access-token 2>/dev/null || true)"

  # 1. Check core project-level permissions via Resource Manager testIamPermissions
  if [[ -n "${token}" ]]; then
    local crm_resp=""
    crm_resp="$(curl -sf -X POST \
      "https://cloudresourcemanager.googleapis.com/v1/projects/${PROJECT}:testIamPermissions" \
      -H "Authorization: Bearer ${token}" \
      -H "Content-Type: application/json" \
      -d '{"permissions":["serviceusage.services.enable","iam.serviceAccounts.create","resourcemanager.projects.setIamPolicy"]}' \
      2>/dev/null || true)"
    if [[ -n "${crm_resp}" ]]; then
      local missing_proj=""
      missing_proj="$(python3 - "${crm_resp}" <<'PY' 2>/dev/null
import json, sys
try:
    data = json.loads(sys.argv[1])
except Exception:
    sys.exit(0)
if not isinstance(data, dict):
    sys.exit(0)
held = set(data.get("permissions") or [])
required = [
    "serviceusage.services.enable",
    "iam.serviceAccounts.create",
    "resourcemanager.projects.setIamPolicy",
]
missing = [p for p in required if p not in held]
if missing:
    print(", ".join(missing))
PY
      )" || true
      if [[ -n "${missing_proj}" ]]; then
        die "Operator lacks required project-level IAM permission(s) on '${PROJECT}': ${missing_proj}. Grant roles/serviceusage.serviceUsageAdmin, roles/iam.serviceAccountAdmin, and roles/resourcemanager.projectIamAdmin (see deploy/cd/README.md) before running setup-cd.sh."
      fi
    else
      log_warn "Could not verify project-level IAM permissions via testIamPermissions; proceeding."
    fi
  else
    log_warn "Could not obtain gcloud access token for IAM preflight; proceeding."
  fi

  # 2. Check Artifact Registry repo-level setIamPolicy when <shared_prefix>-scion already exists
  local ar_err_file="" ar_rc=0 ar_err=""
  ar_err_file="$(mktemp "${TMPDIR:-/tmp}/setup-cd-ar-err.XXXXXX")"
  gcloud artifacts repositories describe "${AR_REPO}" \
    --location="${REGION}" --project="${PROJECT}" \
    --format="value(name)" >/dev/null 2>"${ar_err_file}" || ar_rc=$?
  ar_err="$(cat "${ar_err_file}")"
  rm -f -- "${ar_err_file}"

  if [[ ${ar_rc} -ne 0 ]]; then
    if grep -qiE 'SERVICE_DISABLED|has not been used|it is disabled|API .* not enabled' <<<"${ar_err}"; then
      log_info "Preflight: Artifact Registry API is not enabled yet in '${PROJECT}'; skipping repository IAM check (Step 1 will enable it, and Step 4b will be skipped until configurations/shared-infra is applied)."
      return 0
    fi
    if grep -qiE 'PERMISSION_DENIED|HTTPError 40[13]|unauthori[sz]ed|forbidden|does not have permission|permission .* denied' <<<"${ar_err}"; then
      printf '%s\n' "${ar_err}" >&2
      die "Operator lacks permission to access Artifact Registry repository '${AR_REPO}' in '${REGION}'. Grant the operator roles/artifactregistry.admin (or repository-level artifactregistry.repositories.get + setIamPolicy) before running setup-cd.sh."
    fi
    if grep -qiE 'NOT_FOUND|was not found|does not exist' <<<"${ar_err}"; then
      log_info "Preflight: Artifact Registry repository '${AR_REPO}' does not exist yet (Step 4b will be skipped until configurations/shared-infra is applied)."
      return 0
    fi
    log_warn "Preflight: could not inspect Artifact Registry repository '${AR_REPO}' (${ar_err}); proceeding."
    return 0
  fi

  if [[ -n "${token}" ]]; then
    local ar_parent="projects/${PROJECT}/locations/${REGION}/repositories/${AR_REPO}"
    local ar_resp=""
    ar_resp="$(curl -sf -X POST \
      "https://artifactregistry.googleapis.com/v1/${ar_parent}:testIamPermissions" \
      -H "Authorization: Bearer ${token}" \
      -H "Content-Type: application/json" \
      -d '{"permissions":["artifactregistry.repositories.setIamPolicy"]}' \
      2>/dev/null || true)"
    if [[ -n "${ar_resp}" ]]; then
      local has_ar_set_iam=""
      has_ar_set_iam="$(python3 - "${ar_resp}" <<'PY' 2>/dev/null
import json, sys
try:
    data = json.loads(sys.argv[1])
except Exception:
    print("unknown")
    sys.exit(0)
if not isinstance(data, dict):
    print("unknown")
    sys.exit(0)
held = set(data.get("permissions") or [])
print("yes" if "artifactregistry.repositories.setIamPolicy" in held else "no")
PY
      )" || true
      if [[ "${has_ar_set_iam}" == "no" ]]; then
        die "Operator lacks 'artifactregistry.repositories.setIamPolicy' on Artifact Registry repository '${AR_REPO}' (${REGION}). Note: roles/editor and roles/resourcemanager.projectIamAdmin do NOT include repository setIamPolicy. Grant the operator roles/artifactregistry.admin (project-level or on '${AR_REPO}') before running setup-cd.sh so Step 4b can bind roles/artifactregistry.writer for ${CD_SA_EMAIL} without leaving partial state."
      elif [[ "${has_ar_set_iam}" != "yes" ]]; then
        log_warn "Could not parse Artifact Registry repository testIamPermissions response; proceeding."
      fi
    else
      log_warn "Could not verify Artifact Registry repository setIamPolicy via testIamPermissions; proceeding."
    fi
  fi
}

if [[ "${DRY_RUN}" != "true" ]]; then
  run_operator_permission_preflight
fi

# 1. Enable required CD control-plane APIs
log_info "Step 1: Enabling Cloud Build, Cloud Scheduler, Artifact Registry, and IAM APIs..."
run_cmd gcloud services enable \
  cloudbuild.googleapis.com \
  cloudscheduler.googleapis.com \
  artifactregistry.googleapis.com \
  iam.googleapis.com \
  iamcredentials.googleapis.com \
  --project="${PROJECT}"

# 2. Bootstrap hardened GCS state bucket (UBLA + Versioning + Public Access Prevention)
log_info "Step 2: Ensuring hardened GCS Terraform state bucket gs://${STATE_BUCKET}..."
if [[ "${DRY_RUN}" == "true" ]] || ! gcloud storage buckets describe "gs://${STATE_BUCKET}" --project="${PROJECT}" >/dev/null 2>&1; then
  run_cmd gcloud storage buckets create "gs://${STATE_BUCKET}" \
    --project="${PROJECT}" \
    --location="${REGION}" \
    --uniform-bucket-level-access \
    --public-access-prevention
fi
run_cmd gcloud storage buckets update "gs://${STATE_BUCKET}" \
  --versioning \
  --uniform-bucket-level-access \
  --public-access-prevention

# 3. Create dedicated least-privilege CD Service Account
log_info "Step 3: Ensuring dedicated CD service account ${CD_SA_EMAIL}..."
if [[ "${DRY_RUN}" == "true" ]] || ! gcloud iam service-accounts describe "${CD_SA_EMAIL}" --project="${PROJECT}" >/dev/null 2>&1; then
  run_cmd gcloud iam service-accounts create "${CD_SA_NAME}" \
    --project="${PROJECT}" \
    --display-name="Scion GKE CD deployer (${HUB_NAME})"
fi

# 4. Resource-scoped & IAM-Condition-scoped grants for CD SA
log_info "Step 4: Granting least-privilege IAM bindings to ${CD_SA_EMAIL}..."

# 4a. Bucket-scoped objectAdmin on state bucket ONLY (not project-wide storage.objectAdmin)
run_cmd gcloud storage buckets add-iam-policy-binding "gs://${STATE_BUCKET}" \
  --member="serviceAccount:${CD_SA_EMAIL}" \
  --role="roles/storage.objectAdmin" \
  --quiet

# 4b. Repository-scoped writer on <shared_prefix>-scion Artifact Registry repo (if shared-infra already applied)
if [[ "${DRY_RUN}" == "true" ]] || gcloud artifacts repositories describe "${AR_REPO}" --location="${REGION}" --project="${PROJECT}" >/dev/null 2>&1; then
  run_cmd gcloud artifacts repositories add-iam-policy-binding "${AR_REPO}" \
    --project="${PROJECT}" \
    --location="${REGION}" \
    --member="serviceAccount:${CD_SA_EMAIL}" \
    --role="roles/artifactregistry.writer" \
    --quiet
else
  log_warn "Artifact Registry repository '${AR_REPO}' does not exist yet. Apply configurations/shared-infra first, then re-run setup-cd.sh to bind roles/artifactregistry.writer on ${AR_REPO}."
fi

# 4c. Conditioned Secret Manager admin scoped strictly to <hub_name>-* Terraform-managed DB secrets.
#     Secret Manager evaluates resource.name on secrets.create against the new child secret path
#     (empirically verified against the live API in deploy/terraform/modules/hub-identity/main.tf).
SM_CONDITION="expression=resource.name.startsWith(\"projects/${PROJECT_NUMBER}/secrets/${HUB_NAME}-\"),title=${HUB_NAME}-cd-secrets,description=Terraform-managed secrets for ${HUB_NAME} only"
run_cmd gcloud projects add-iam-policy-binding "${PROJECT}" \
  --member="serviceAccount:${CD_SA_EMAIL}" \
  --role="roles/secretmanager.admin" \
  --condition="${SM_CONDITION}" \
  --quiet

# 4d. Conditioned Storage admin scoped strictly to <project>-<hub_name>-* artifacts bucket.
#     NOTE: Cloud Storage evaluates storage.buckets.create on the parent project resource rather
#     than projects/_/buckets/<new-bucket>. Pass --bootstrap-storage-admin before Pass 1 so the
#     CD SA can create gs://<project>-<hub_name>-artifacts, then run --revoke-bootstrap-storage-admin
#     after Pass 1 so steady-state runs keep only this bucket-prefix condition.
GCS_CONDITION="expression=resource.name.startsWith(\"projects/_/buckets/${PROJECT}-${HUB_NAME}-\"),title=${HUB_NAME}-cd-buckets,description=Artifacts bucket for ${HUB_NAME} only"
run_cmd gcloud projects add-iam-policy-binding "${PROJECT}" \
  --member="serviceAccount:${CD_SA_EMAIL}" \
  --role="roles/storage.admin" \
  --condition="${GCS_CONDITION}" \
  --quiet

ARTIFACTS_BUCKET="${PROJECT}-${HUB_NAME}-artifacts"
if [[ "${BOOTSTRAP_STORAGE_ADMIN}" == "true" ]]; then
  log_info "Granting temporary unconditioned roles/storage.admin for Pass 1 bucket creation (gs://${ARTIFACTS_BUCKET})..."
  run_cmd gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${CD_SA_EMAIL}" \
    --role="roles/storage.admin" \
    --condition=None \
    --quiet
  log_warn "After Pass 1 creates gs://${ARTIFACTS_BUCKET}, revoke temporary project-wide storage.admin with:"
  log_warn "  $(basename "$0") --project ${PROJECT} --shared-prefix ${SHARED_PREFIX} --hub-name ${HUB_NAME} --revoke-bootstrap-storage-admin"
elif [[ "${DRY_RUN}" != "true" ]] && ! gcloud storage buckets describe "gs://${ARTIFACTS_BUCKET}" --project="${PROJECT}" >/dev/null 2>&1; then
  log_warn "Artifacts bucket gs://${ARTIFACTS_BUCKET} does not exist yet. Because storage.buckets.create is checked on the parent project rather than projects/_/buckets/..., re-run with --bootstrap-storage-admin before Pass 1 (and --revoke-bootstrap-storage-admin afterward)."
fi

# 4e. Steady-state project-level roles required by configurations/hub-gke
PROJECT_ROLES=(
  "roles/container.admin"
  "roles/cloudsql.admin"
  "roles/iam.serviceAccountAdmin"
  "roles/resourcemanager.projectIamAdmin"
  "roles/compute.loadBalancerAdmin"
  "roles/compute.securityAdmin"
  "roles/compute.networkViewer"
  "roles/file.viewer"
  "roles/iap.admin"
  "roles/logging.logWriter"
)

for role in "${PROJECT_ROLES[@]}"; do
  run_cmd gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${CD_SA_EMAIL}" \
    --role="${role}" \
    --condition=None \
    --quiet
done

# 5. Cloud Build Trigger & Cloud Scheduler daily job
# Use gcloud's ^|^ alternate delimiter (gcloud topic escaping) so comma-separated
# values such as _AGENT_PLATFORMS=linux/amd64,linux/arm64 and colons in _REPO_URL
# parse cleanly inside --substitutions.
for sub_val in "${CHANNEL}" "${VERSION_TAG}" "${STATE_BUCKET}" "${SHARED_PREFIX}" "${HUB_NAME}" "${REGISTRY}" "${AGENT_PLATFORMS}" "${REPO_URL}"; do
  [[ "${sub_val}" != *"|"* ]] || die "Substitution value '${sub_val}' must not contain '|'"
done
SUBSTITUTIONS="^|^_CHANNEL=${CHANNEL}|_VERSION_TAG=${VERSION_TAG}|_STATE_BUCKET=${STATE_BUCKET}|_SHARED_PREFIX=${SHARED_PREFIX}|_HUB_NAME=${HUB_NAME}|_REGISTRY=${REGISTRY}|_AGENT_PLATFORMS=${AGENT_PLATFORMS}|_FORCE_REDEPLOY=false|_FORCE_APPLY=false|_ALLOW_DOWNGRADE=false|_ALLOW_BELOW_DOWNGRADE_FLOOR=false|_REPO_URL=${REPO_URL}"
CD_SA_RESOURCE="projects/${PROJECT}/serviceAccounts/${CD_SA_EMAIL}"

if [[ -n "${REPOSITORY_2ND_GEN}" || -n "${GITHUB_OWNER}" ]]; then
  log_info "Step 5: Configuring Cloud Build Trigger (${TRIGGER_NAME}) and Cloud Scheduler (${SCHEDULER_JOB_NAME})..."

  if [[ "${DRY_RUN}" == "true" ]] || ! gcloud builds triggers describe "${TRIGGER_NAME}" --region="${REGION}" --project="${PROJECT}" >/dev/null 2>&1; then
    if [[ -n "${REPOSITORY_2ND_GEN}" ]]; then
      run_cmd gcloud builds triggers create manual \
        --project="${PROJECT}" \
        --region="${REGION}" \
        --name="${TRIGGER_NAME}" \
        --repository="${REPOSITORY_2ND_GEN}" \
        --branch="${TRIGGER_BRANCH}" \
        --build-config="deploy/cd/cloudbuild-gke-cd.yaml" \
        --service-account="${CD_SA_RESOURCE}" \
        --substitutions="${SUBSTITUTIONS}"
    else
      run_cmd gcloud builds triggers create manual \
        --project="${PROJECT}" \
        --region="${REGION}" \
        --name="${TRIGGER_NAME}" \
        --repo="https://github.com/${GITHUB_OWNER}/${GITHUB_REPO}" \
        --repo-type="GITHUB" \
        --branch="${TRIGGER_BRANCH}" \
        --build-config="deploy/cd/cloudbuild-gke-cd.yaml" \
        --service-account="${CD_SA_RESOURCE}" \
        --substitutions="${SUBSTITUTIONS}"
    fi
  fi

  # Dedicated Scheduler SA with least-privilege trigger invocation
  if [[ "${DRY_RUN}" == "true" ]] || ! gcloud iam service-accounts describe "${SCHEDULER_SA_EMAIL}" --project="${PROJECT}" >/dev/null 2>&1; then
    run_cmd gcloud iam service-accounts create "${SCHEDULER_SA_NAME}" \
      --project="${PROJECT}" \
      --display-name="Scion CD Cloud Scheduler invoker (${HUB_NAME})"
  fi

  run_cmd gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${SCHEDULER_SA_EMAIL}" \
    --role="roles/cloudbuild.builds.editor" \
    --condition=None \
    --quiet

  run_cmd gcloud iam service-accounts add-iam-policy-binding "${CD_SA_EMAIL}" \
    --project="${PROJECT}" \
    --member="serviceAccount:${SCHEDULER_SA_EMAIL}" \
    --role="roles/iam.serviceAccountUser" \
    --quiet

  TRIGGER_ID="TRIGGER_ID"
  if [[ "${DRY_RUN}" != "true" ]]; then
    TRIGGER_ID="$(gcloud builds triggers describe "${TRIGGER_NAME}" --region="${REGION}" --project="${PROJECT}" --format='value(id)')"
  fi

  # Regional projects.locations.triggers.run expects RunBuildTriggerRequest
  # ({projectId, triggerId, source: RepoSource}), NOT bare RepoSource:
  #   - 2nd-gen (--repository): empty {} runs the manual trigger's configured sourceToBuild branch.
  #   - 1st-gen (--github-owner/--github-repo): {"source":{"branchName":"<branch>"}}.
  SCHEDULER_MESSAGE_BODY="{}"
  if [[ -z "${REPOSITORY_2ND_GEN}" ]]; then
    SCHEDULER_MESSAGE_BODY="$(python3 - "${TRIGGER_BRANCH}" <<'PY'
import json
import sys

print(json.dumps({"source": {"branchName": sys.argv[1]}}, separators=(",", ":")))
PY
    )"
  fi

  TRIGGER_RUN_URI="https://cloudbuild.googleapis.com/v1/projects/${PROJECT}/locations/${REGION}/triggers/${TRIGGER_ID}:run"
  if [[ "${DRY_RUN}" == "true" ]] || ! gcloud scheduler jobs describe "${SCHEDULER_JOB_NAME}" --location="${REGION}" --project="${PROJECT}" >/dev/null 2>&1; then
    run_cmd gcloud scheduler jobs create http "${SCHEDULER_JOB_NAME}" \
      --project="${PROJECT}" \
      --location="${REGION}" \
      --schedule="${SCHEDULE_CRON}" \
      --time-zone="Etc/UTC" \
      --uri="${TRIGGER_RUN_URI}" \
      --http-method=POST \
      --headers="Content-Type=application/json" \
      --message-body="${SCHEDULER_MESSAGE_BODY}" \
      --oauth-service-account-email="${SCHEDULER_SA_EMAIL}" \
      --oauth-token-scope="https://www.googleapis.com/auth/cloud-platform"
  fi
else
  log_info "Step 5: Skipping Cloud Build Trigger creation (pass --repository or --github-owner/--github-repo to create automatically)."
  log_info "  Substitutions for manual trigger setup:"
  log_info "    --substitutions=\"${SUBSTITUTIONS}\""
fi

log_info "Bootstrap complete. Next steps:"
log_info "  1. Apply deploy/terraform/configurations/shared-infra once (if not already applied)."
log_info "  2. Copy deploy/cd/hub.tfvars.example -> hub.tfvars, fill in values, and upload to:"
log_info "       gs://${STATE_BUCKET}/${SHARED_PREFIX}/hubs/${HUB_NAME}/hub.tfvars"
log_info "  3. Trigger the pipeline via Cloud Build (${TRIGGER_NAME}) and follow deploy/cd/README.md."
