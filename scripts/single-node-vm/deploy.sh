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

# scripts/single-node-vm/deploy.sh — Wizard-style deployment for a single-node
# Scion Hub on a GCE VM with IAP proxy authentication.
#
# This script provisions a GCE VM, downloads the scion binary from GitHub
# Releases, starts the hub via systemd, deploys a Cloud Run IAP reverse proxy,
# enables IAP, configures the hub for proxy auth, and prints the access URL.
#
# The VM has no public IP; authenticated access is via the Cloud Run IAP proxy.
# Agents running on the VM reach the hub directly on the VM's port 8080 over
# the Docker bridge (hostname scion-hub.internal), bypassing IAP.
#
# The script is idempotent: re-running converges without duplication.
#
# Usage:
#   ./deploy.sh [--version VERSION] [--config CONFIG_FILE] [--rebuild-images]
#   ./deploy.sh --delete
#
# Options:
#   --version VERSION   Scion release version to install (e.g. v0.5.0).
#                       If omitted, the latest release is fetched from GitHub.
#   --config FILE       Path to a JSON config file that pre-answers interactive
#                       prompts. When provided with all required fields, the
#                       script runs headlessly (no interactive input needed).
#                       See deploy-config.example.json for the format.
#   --rebuild-images    Force a rebuild of container images on the VM even if
#                       the version marker and all 3 expected images already
#                       match VERSION. Equivalent to container_images.
#                       force_rebuild: true in the config file; either one
#                       forces a rebuild.
#   --delete            Tear down all resources created by a previous deploy.
#
# Config file fields (all optional; see deploy-config.example.json):
#   hub_name                     Resource name suffix (scion-hub-<hub_name>).
#                                 <= 20 chars, lowercase letters/digits/hyphens,
#                                 must start with a lowercase letter.
#   project_id                   GCP project ID. Empty = current gcloud project.
#   region                       GCP region for the VM and Cloud Run proxy.
#   machine_size                 "small" (e2-standard-4, ~10 agents) or
#                                 "medium" (n2-standard-16, ~50 agents).
#   disk_size_gb                 Boot disk size in GB.
#   chat_plugins                 List of: telegram, discord, slack, teams.
#   container_images.source      "registry" (pre-built images) or "build"
#                                 (build on the VM). "build" is refused
#                                 when gke_target.name is set: GKE nodes
#                                 cannot pull from the VM's local Docker
#                                 store, which is what "build" uses.
#   container_images.registry    Registry path; required when source is
#                                 "registry" (e.g. us-docker.pkg.dev/PROJECT/scion).
#   container_images.force_rebuild
#                                 Force a rebuild even if the VM's version
#                                 marker and images already match VERSION.
#   admin_email                  Granted super-admin on first login. Empty =
#                                 active gcloud account.
#   update_policy                auto, notify, or disabled. Requires the
#                                 binary auto-update feature.
#   release_channel              stable, preview, or nightly. Defaults to
#                                 nightly if not specified.
#   gke_target.name              Name of an existing GKE cluster to attach
#                                 as a second, Kubernetes-based runtime
#                                 (hybrid tier). Optional; when absent (the
#                                 default), the hybrid tier is off. The
#                                 cluster is a manual prerequisite: this
#                                 script only ever attaches to it, never
#                                 creates or deletes it.
#   gke_target.location          Zone or region of the cluster. Required
#                                 when gke_target.name is set.
#   gke_target.project           Project the cluster lives in. Defaults to
#                                 project_id. A cluster in a different
#                                 project is not supported yet.
#   gke_target.namespace         Kubernetes namespace for the shared-tree
#                                 PersistentVolumeClaim. Defaults to
#                                 scion-hub-<hub_name>. An existing
#                                 namespace without this deployment's
#                                 marker is used as-is, never adopted or
#                                 relabeled, and never deleted on teardown.
#   gke_target.pvc_name           Name of the PersistentVolumeClaim in that
#                                 namespace. Defaults to
#                                 scion-hub-<hub_name>-shared.
#   user_access_mode             Hybrid tier only. The tier configures
#                                 restricted user access: invite_only
#                                 (the default when unset) or
#                                 domain_restricted. "open" and an empty
#                                 value are refused. admin_email must be
#                                 set to a user account; it can always
#                                 sign in and invites other users.
#   authorized_domains           Hybrid tier only. Optional list of email
#                                 domains ("example.com" or
#                                 "*.example.com"); required for
#                                 domain_restricted. Service account
#                                 domains (gserviceaccount.com) are
#                                 refused.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Python interpreter used to parse the JSON config file. Override with
# PYTHON=/path/to/python3 if python3 is not on PATH.
PYTHON="${PYTHON:-python3}"

# ---------------------------------------------------------------------------
# Color helpers
# ---------------------------------------------------------------------------
BOLD='\033[1m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
RED='\033[0;31m'
RESET='\033[0m'

info()    { echo -e "${BOLD}${GREEN}==>${RESET} ${BOLD}$*${RESET}"; }
warn()    { echo -e "${YELLOW}WARNING:${RESET} $*" >&2; }
err()     { echo -e "${RED}ERROR:${RESET} $*" >&2; }
section() { echo ""; echo -e "${BOLD}--- $* ---${RESET}"; }

# hub_health_status BODY -- prints the top-level status of a hub /healthz
# body: healthy, degraded, unhealthy, or unknown (no answer / not a scion
# body). The hub always encodes "status" first on a single line, so matching
# the start of the body reads the top-level status only, never a nested
# "hub"/"broker" status.
hub_health_status() {
  case "$1" in
    '{"status":"healthy"'*) echo healthy ;;
    '{"status":"degraded"'*) echo degraded ;;
    '{"status":"unhealthy"'*) echo unhealthy ;;
    *) echo unknown ;;
  esac
}

# wait_for_hub_health LABEL -- polls the hub's /healthz on the VM until it
# reports healthy, up to HEALTH_CHECK_MAX_ATTEMPTS times. /healthz always
# returns HTTP 200, so the body's status decides (ptone/scion#1094):
#   - healthy: pass.
#   - degraded at the last attempt: the hub is up but a non-critical check
#     (e.g. colocated_broker) is failing -- pass with a visible warning and
#     the response, so the failing checks are named.
#   - unhealthy (a critical check such as the database failed), or no
#     answer: fail (returns 1).
# Keeps polling for healthy while degraded, since the co-located broker
# registers just after startup.
wait_for_hub_health() {
  local label="$1" body="" status="unknown" i
  for i in $(seq 1 "$HEALTH_CHECK_MAX_ATTEMPTS"); do
    body="$(gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="curl -s http://localhost:8080/healthz" \
      2>/dev/null || true)"
    status="$(hub_health_status "$body")"
    if [[ "$status" == "healthy" ]]; then
      echo ""
      echo -e "${GREEN}  ${label} passed.${RESET}"
      return 0
    fi
    if [[ "$i" -lt "$HEALTH_CHECK_MAX_ATTEMPTS" ]]; then
      echo "  Attempt ${i}/${HEALTH_CHECK_MAX_ATTEMPTS} (status: ${status}) - waiting ${HEALTH_CHECK_RETRY_SECS}s..."
      sleep "$HEALTH_CHECK_RETRY_SECS"
    fi
  done
  if [[ "$status" == "degraded" ]]; then
    echo ""
    warn "${label}: the hub is up but DEGRADED (a non-critical check is failing). Response:"
    echo "  ${body}" >&2
    echo "  Check the service logs:" >&2
    echo "  gcloud compute ssh ${INSTANCE_NAME} --zone=${ZONE} --project=${PROJECT_ID} \\" >&2
    echo "    --command='sudo journalctl -u scion-hub.service --no-pager -n 50'" >&2
    return 0
  fi
  err "${label} did not pass within $((HEALTH_CHECK_MAX_ATTEMPTS * HEALTH_CHECK_RETRY_SECS))s (last status: ${status})."
  if [[ -n "$body" ]]; then
    echo "  Last /healthz response: ${body}" >&2
  fi
  echo "  Check the service logs:" >&2
  echo "  gcloud compute ssh ${INSTANCE_NAME} --zone=${ZONE} --project=${PROJECT_ID} \\" >&2
  echo "    --command='sudo journalctl -u scion-hub.service --no-pager -n 50'" >&2
  return 1
}

# print_gcloud_error TEXT -- prints TEXT (gcloud's captured stderr) to
# stderr, each line indented by two spaces. Prints nothing when TEXT is
# empty.
print_gcloud_error() {
  if [[ -n "$1" ]]; then
    printf '%s\n' "$1" | sed 's/^/  /' >&2
  fi
}

# ---------------------------------------------------------------------------
# Retry / timing budgets
# ---------------------------------------------------------------------------
# Tuned for a cold VM on first boot: guest-agent start, OS Login / metadata
# key propagation, sshd host-key generation, and IAP tunnel setup all stack
# on top of package_upgrade: true restarting services.
readonly SSH_MAX_ATTEMPTS=30           # ~5 min ceiling (see backoff below)
readonly SSH_BACKOFF_FAST_ATTEMPTS=5   # attempts 1..N-1 use the fast backoff
readonly SSH_BACKOFF_FAST_SECS=5
readonly SSH_BACKOFF_SLOW_SECS=10
readonly CLOUD_INIT_MAX_ATTEMPTS=6
readonly CLOUD_INIT_RETRY_SECS=15
readonly HEALTH_CHECK_MAX_ATTEMPTS=12
readonly HEALTH_CHECK_RETRY_SECS=5
readonly BUILD_POLL_MAX_ATTEMPTS=180
readonly BUILD_POLL_INTERVAL_SECS=15
# Overridable via env so a test can drive this deploy without waiting
# out the real wait for real IAP enforcement to activate.
readonly IAP_ENFORCEMENT_WAIT_SECS="${IAP_ENFORCEMENT_WAIT_SECS:-60}"
# The default VPC network's creation (triggered by enabling
# compute.googleapis.com in an ordinary org) is asynchronous but usually
# visible within a few seconds; ~60s total gives real propagation room to
# clear (including a brand-new project's compute.googleapis.com itself
# still settling right after being enabled -- see the SERVICE_DISABLED
# retry below) without making a genuinely-missing-network failure (the
# hardened-org case) too slow to report. The interval is overridable via
# SCION_TEST_NETWORK_RETRY_SECS so the test suite doesn't have to sleep
# through the real budget; unset (the normal case) it's just 5.
readonly NETWORK_CHECK_MAX_ATTEMPTS=12
readonly NETWORK_CHECK_DEFAULT_RETRY_SECS=5
# Validated, not trusted verbatim: this is the one place a non-numeric,
# negative, or implausibly large override (a typo, a stray shell fragment
# in the environment, or a fat-fingered value) would otherwise reach
# `sleep` directly -- a non-numeric one kills the deploy with a bare
# "invalid time interval" under set -e, on the retry path only, which
# would be a confusing way to fail; a huge one is effectively a hang on
# that same path. Capped at 60s (already well beyond what any legitimate
# test needs) rather than left unbounded. The integer-part length check
# runs before any arithmetic, so an arbitrarily long digit string can't
# reach bash's 64-bit `-gt` and silently wrap around into something
# small enough to be accepted; the `10#` prefix on that comparison forces
# base 10, so a leading zero (e.g. "08") isn't misread as octal, which
# `-gt` would otherwise reject with a bash error of its own.
if [[ -n "${SCION_TEST_NETWORK_RETRY_SECS:-}" ]]; then
  if [[ ! "${SCION_TEST_NETWORK_RETRY_SECS}" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
    warn "SCION_TEST_NETWORK_RETRY_SECS='${SCION_TEST_NETWORK_RETRY_SECS}' is not a valid non-negative number; using the default."
    SCION_TEST_NETWORK_RETRY_SECS=""
  else
    NETWORK_RETRY_SECS_INT_PART="${SCION_TEST_NETWORK_RETRY_SECS%%.*}"
    if [[ ${#NETWORK_RETRY_SECS_INT_PART} -gt 2 || "$((10#$NETWORK_RETRY_SECS_INT_PART))" -gt 60 ]]; then
      warn "SCION_TEST_NETWORK_RETRY_SECS='${SCION_TEST_NETWORK_RETRY_SECS}' is out of range; using the default."
      SCION_TEST_NETWORK_RETRY_SECS=""
    fi
    unset NETWORK_RETRY_SECS_INT_PART
  fi
fi
readonly NETWORK_CHECK_RETRY_SECS="${SCION_TEST_NETWORK_RETRY_SECS:-$NETWORK_CHECK_DEFAULT_RETRY_SECS}"

# ---------------------------------------------------------------------------
# Parse flags
# ---------------------------------------------------------------------------
VERSION=""
DELETE_MODE=false
CONFIG_FILE=""
CLI_REBUILD_IMAGES=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --config) CONFIG_FILE="$2"; shift 2 ;;
    --delete) DELETE_MODE=true; shift ;;
    --rebuild-images) CLI_REBUILD_IMAGES=true; shift ;;
    --help|-h)
      sed -n '/^# scripts\/single-node-vm/,/^[^#]/{ /^#/s/^# \?//p }' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    *) err "Unknown flag: $1"; exit 1 ;;
  esac
done

# ---------------------------------------------------------------------------
# Config file helper
# ---------------------------------------------------------------------------
# Reads a value from the JSON config file using dot-separated keys.
# Falls back to the provided default when the key is missing or the config
# file is not set.  Lists are returned as space-separated strings.
config_get() {
  local key="$1"
  local default="${2:-}"
  if [[ -n "$CONFIG_FILE" && -f "$CONFIG_FILE" ]]; then
    local val
    val="$("$PYTHON" -c "
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
keys = sys.argv[2].split('.')
v = d
for k in keys:
    if isinstance(v, dict):
        v = v.get(k)
    else:
        v = None
        break
if v is not None:
    if isinstance(v, list):
        print(' '.join(str(i) for i in v))
    elif isinstance(v, bool):
        print(str(v).lower())
    else:
        print(v)
" "$CONFIG_FILE" "$key" 2>/dev/null)" || true
    if [[ -n "$val" ]]; then
      echo "$val"
      return
    fi
  fi
  echo "$default"
}

# Helper: prompt the user for input, or error in non-interactive mode.
# Usage: config_prompt VAR "Prompt text" "default_value"
config_prompt() {
  local varname="$1"
  local prompt="$2"
  local default="$3"
  if [[ -n "$CONFIG_FILE" && ! -t 0 ]]; then
    if [[ -n "$default" ]]; then
      printf -v "$varname" '%s' "$default"
    else
      err "Required value for '${varname}' is missing from config and stdin is not a terminal."
      exit 1
    fi
  else
    local input=""
    read -rp "$prompt" input
    printf -v "$varname" '%s' "${input:-$default}"
  fi
}

# proxy_sa_name HUB_NAME — derives the Cloud Run proxy's service-account
# ID from the hub name. A single helper shared by both the create path
# (Phase 2) and teardown, so they can never compute two different names
# for the same hub -- the two paths used to duplicate this logic, and a
# mutation that only broke one of the two copies passed the full test
# suite (see the fix for that: a test exercising create-then-delete with
# a long, truncation-triggering hub name).
#
# Truncates "scion-hub-${HUB_NAME}-proxy" to the 30-char GCP
# service-account ID limit when needed. A plain positional truncation
# (keep the first N characters and drop the rest) would map any two hub
# names sharing a long-enough prefix to the *same* truncated name --
# e.g. "engineering-team-a" and "engineering-team-b" both under a naive
# 14-char cut -- so `--delete` on one would silently delete the SA the
# other hub's Cloud Run proxy is still running as. Appending a short hash
# of the *full* hub name (not just the truncated prefix) keeps two such
# names distinct.
proxy_sa_name() {
  local hub_name="$1" name="scion-hub-${1}-proxy"
  if [[ ${#name} -le 30 ]]; then
    printf '%s' "$name"
    return
  fi
  local hash
  hash="$(printf '%s' "$hub_name" | openssl dgst -sha256 -r | cut -d' ' -f1 | cut -c1-4)"
  name="scion-hub-${hub_name:0:9}-${hash}-proxy"
  warn "Proxy service-account name truncated to 30 chars: ${name}"
  printf '%s' "$name"
}

# Validate config file exists if specified
if [[ -n "$CONFIG_FILE" ]]; then
  if [[ ! -f "$CONFIG_FILE" ]]; then
    err "Config file not found: $CONFIG_FILE"
    exit 1
  fi
  if ! command -v "$PYTHON" &>/dev/null; then
    err "Python interpreter '${PYTHON}' is required to parse the config file but was not found."
    exit 1
  fi
  if ! json_err=$("$PYTHON" -c "import json, sys; json.load(open(sys.argv[1], encoding='utf-8'))" "$CONFIG_FILE" 2>&1); then
    err "Invalid JSON syntax in config file: $CONFIG_FILE"
    echo "$json_err" >&2
    exit 1
  fi
  info "Using config file: $CONFIG_FILE"
fi

# Hybrid tier (optional GKE attach target): see hybrid-tier.sh. Sourced here,
# after config_get/config_prompt/info/warn/err are defined, since it uses
# all of them. HYBRID_ENABLED defaults to false until hybrid_read_config
# runs below.
HYBRID_ENABLED=false
# shellcheck source=scripts/single-node-vm/hybrid-tier.sh
source "${SCRIPT_DIR}/hybrid-tier.sh"

# One EXIT trap, registered once, removes these three temp files on every
# exit path, including SIGINT and SIGTERM: SSH_STDERR_FILE,
# HYBRID_KUBECONFIG and ALLUSERS_STDERR_FILE. Other temp files are not
# covered by this trap. The three start empty, and
# `rm -f ""` is a harmless no-op, so setting the trap here before any
# file exists is safe. `trap`
# replaces rather than stacks, so a second, later `trap ... EXIT` call
# would silently drop this one -- there must only ever be this single
# registration.
SSH_STDERR_FILE=""
HYBRID_KUBECONFIG=""
ALLUSERS_STDERR_FILE=""
trap 'rm -f "$SSH_STDERR_FILE" "$HYBRID_KUBECONFIG" "$ALLUSERS_STDERR_FILE"' EXIT

# ---------------------------------------------------------------------------
# Teardown flow (--delete)
# ---------------------------------------------------------------------------
if [[ "$DELETE_MODE" == "true" ]]; then
  section "Teardown: Delete Single-Node-VM Resources"

  PROJECT_ID="$(config_get 'project_id' '')"
  if [[ -z "$PROJECT_ID" ]]; then
    info "Detecting GCP project..."
    PROJECT_ID="$(gcloud config get-value project 2>/dev/null)" || true
  fi
  if [[ -z "$PROJECT_ID" ]]; then
    err "No GCP project configured. Set project_id in config or run: gcloud config set project PROJECT_ID"
    exit 1
  fi
  echo "  Project: ${PROJECT_ID}"

  HUB_NAME="$(config_get 'hub_name' '')"
  if [[ -z "$HUB_NAME" ]]; then
    config_prompt HUB_NAME "Hub name [my-hub]: " "my-hub"
  fi
  if [[ ! "$HUB_NAME" =~ ^[a-z][a-z0-9-]*$ ]]; then
    err "Hub name '${HUB_NAME}' is invalid. It must start with a lowercase letter and contain only lowercase letters, numbers, and hyphens."
    exit 1
  fi

  REGION="$(config_get 'region' '')"
  if [[ -z "$REGION" ]]; then
    config_prompt REGION "GCP region [us-central1]: " "us-central1"
  fi

  INSTANCE_NAME="scion-hub-${HUB_NAME}"
  PROXY_SERVICE="${INSTANCE_NAME}-iap-proxy"
  # Deliberately our own fixed names only -- never a router/NAT discovered by
  # the reuse check in the create path. If deploy.sh reused someone else's
  # NAT, our own scion-hub-${HUB_NAME}-router/-nat were never created, so the
  # deletes below simply no-op ("not found or already deleted") and the
  # reused resource is left untouched.
  ROUTER_NAME="scion-hub-${HUB_NAME}-router"
  NAT_NAME="scion-hub-${HUB_NAME}-nat"
  FW_RULE_NAME="scion-hub-${HUB_NAME}-allow-iap-ssh"
  FW_8080_RULE_NAME="scion-hub-${HUB_NAME}-allow-proxy"

  # Discover the actual zone of the instance (if it still exists)
  ZONE="$(gcloud compute instances list \
    --filter="name=${INSTANCE_NAME}" \
    --format="value(zone)" \
    --project="${PROJECT_ID}" 2>/dev/null | head -1)" || true
  if [[ -z "$ZONE" ]]; then
    # Instance already deleted; discover an available zone in the region
    ZONE="$(gcloud compute zones list \
      --filter="region=${REGION}" \
      --limit=1 \
      --format="value(name)" \
      --project="${PROJECT_ID}" 2>/dev/null)" || true
  fi
  if [[ -z "$ZONE" ]]; then
    ZONE="${REGION}-b"
    warn "Could not discover zone dynamically; defaulting to ${ZONE}"
  fi
  SA_NAME="scion-hub-${HUB_NAME}"
  SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
  # proxy_sa_name is shared with the create path (Phase 2) precisely so
  # this can never drift from what create actually named the SA.
  PROXY_SA_NAME="$(proxy_sa_name "$HUB_NAME")"
  PROXY_SA_EMAIL="${PROXY_SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

  echo ""
  echo "Checking hybrid-tier firewall rule ownership:"
  if ! hybrid_teardown_preflight "$HUB_NAME" "$PROJECT_ID"; then
    exit 1
  fi
  TEARDOWN_HAD_FAILURE=false

  # Hybrid tier: the static internal IP reservation, checked the same
  # way as the firewall rules -- unconditionally, even with the tier off
  # in the current config, and aborting the whole teardown before any
  # delete on an unmarked same-name match.
  echo ""
  echo "Checking hybrid-tier internal IP reservation ownership:"
  hybrid_internal_ip_teardown_check "$HUB_NAME" "$PROJECT_ID" "$REGION"
  if [[ "$HYBRID_INTERNAL_IP_TEARDOWN_FAILED" == "true" ]]; then
    exit 1
  fi

  # Hybrid tier (Kubernetes objects): gke_target.name is read directly,
  # never via the interactive hybrid_read_config, since a --delete run
  # must never prompt to enable the tier. Absent gke_target.name means
  # the tier was never configured for this hub's teardown, so none of
  # the k8s objects are ever touched -- as inert as the tier-off create
  # path. A cluster that's genuinely gone (a positive NOT_FOUND) means
  # its k8s objects went with it, so this prints that and continues;
  # any other describe failure -- unknown, not confirmed gone -- aborts
  # before any delete, the same "uncertainty counts as not gone" rule
  # used throughout this tier's other checks.
  HYBRID_K8S_TEARDOWN_READY=false
  K8S_GKE_NAME="$(config_get 'gke_target.name' '')"
  if [[ -z "$K8S_GKE_NAME" && -z "$CONFIG_FILE" ]]; then
    echo ""
    warn "No --config given, so this teardown cannot tell whether a hybrid tier was ever configured for hub '${HUB_NAME}'. If it was, its Kubernetes objects (default names: namespace $(hybrid_k8s_default_namespace "$HUB_NAME"), PVC $(hybrid_k8s_default_pvc_name "$HUB_NAME"), PV $(hybrid_k8s_pv_name "$HUB_NAME") -- or whatever gke_target.namespace/pvc_name were customized to) will NOT be checked or deleted by this run. Re-run with --config pointing at the same config file used to deploy, to have them checked."
  fi
  if [[ -n "$K8S_GKE_NAME" ]]; then
    GKE_LOCATION="$(config_get 'gke_target.location' '')"
    GKE_PROJECT_RAW="$(config_get 'gke_target.project' '')"
    GKE_NAMESPACE_RAW="$(config_get 'gke_target.namespace' "$(hybrid_k8s_default_namespace "$HUB_NAME")")"
    GKE_PVC_NAME_RAW="$(config_get 'gke_target.pvc_name' "$(hybrid_k8s_default_pvc_name "$HUB_NAME")")"
    GKE_NAME="$K8S_GKE_NAME"
    GKE_PROJECT="${GKE_PROJECT_RAW:-$PROJECT_ID}"
    # Same field-syntax and project-match validation the create path
    # applies via hybrid_read_config -- a malformed or foreign-project
    # gke_target must be refused here too, not just on create.
    _hybrid_validate_target_fields "$GKE_NAME" "$GKE_LOCATION" "$GKE_PROJECT" "$GKE_NAMESPACE_RAW" "$GKE_PVC_NAME_RAW" "$PROJECT_ID"
    GKE_NAMESPACE="$GKE_NAMESPACE_RAW"
    GKE_PVC_NAME="$GKE_PVC_NAME_RAW"
    echo ""
    echo "Checking hybrid-tier Kubernetes object ownership:"
    CLUSTER_DESCRIBE_ERR="$(mktemp)"
    if gcloud container clusters describe "$GKE_NAME" --location="$GKE_LOCATION" \
        --project="$GKE_PROJECT" --quiet >/dev/null 2>"${CLUSTER_DESCRIBE_ERR}"; then
      hybrid_k8s_setup_kubeconfig
      hybrid_k8s_teardown_check "$HUB_NAME"
      if [[ "$HYBRID_K8S_TEARDOWN_FAILED" == "true" ]]; then
        rm -f "${CLUSTER_DESCRIBE_ERR}"
        exit 1
      fi
      HYBRID_K8S_TEARDOWN_READY=true
    elif _hybrid_gcloud_not_found "$(cat "${CLUSTER_DESCRIBE_ERR}")"; then
      echo "  GKE cluster ${GKE_NAME} not found; its Kubernetes objects went with it."
    else
      err "Could not confirm whether GKE cluster ${GKE_NAME} still exists:"
      err "  $(cat "${CLUSTER_DESCRIBE_ERR}")"
      rm -f "${CLUSTER_DESCRIBE_ERR}"
      exit 1
    fi
    rm -f "${CLUSTER_DESCRIBE_ERR}"
  fi

  # _hybrid_k8s_display KIND — "kind/name" for the kind hybrid_k8s_
  # teardown_check queued, using the names it resolved (HYBRID_K8S_PVC_
  # NAME/HYBRID_K8S_PV_NAME/HYBRID_K8S_NAMESPACE), for both the
  # will-delete list below and the final summary.
  _hybrid_k8s_display() {
    case "$1" in
      pvc) echo "persistentvolumeclaim/${HYBRID_K8S_PVC_NAME}" ;;
      pv) echo "persistentvolume/${HYBRID_K8S_PV_NAME}" ;;
      namespace) echo "namespace/${HYBRID_K8S_NAMESPACE}" ;;
    esac
  }

  echo ""
  echo "The following resources will be deleted:"
  if [[ "$HYBRID_K8S_TEARDOWN_READY" == "true" ]]; then
    for k8s_kind in ${HYBRID_K8S_TEARDOWN_DELETE[@]+"${HYBRID_K8S_TEARDOWN_DELETE[@]}"}; do
      echo "  Kubernetes object: $(_hybrid_k8s_display "$k8s_kind") (hybrid tier)"
    done
  fi
  echo "  Cloud Run service: ${PROXY_SERVICE} (region: ${REGION})"
  echo "  GCE VM:            ${INSTANCE_NAME} (zone: ${ZONE})"
  echo "  Cloud NAT:         ${NAT_NAME} (router: ${ROUTER_NAME})"
  echo "  Cloud Router:      ${ROUTER_NAME} (region: ${REGION})"
  echo "  Service account:   ${SA_EMAIL}"
  echo "  Proxy SA:          ${PROXY_SA_EMAIL}"
  echo "  Service account:   $(hybrid_transport_sa_name "${HUB_NAME}")@${PROJECT_ID}.iam.gserviceaccount.com (hybrid tier agent transport; if present and marked)"
  echo "  Firewall rule:     ${FW_RULE_NAME}"
  echo "  Firewall rule:     ${FW_8080_RULE_NAME}"
  for name in ${HYBRID_TEARDOWN_DELETE[@]+"${HYBRID_TEARDOWN_DELETE[@]}"}; do
    echo "  Firewall rule:     ${name} (hybrid tier)"
  done
  if [[ "${HYBRID_INTERNAL_IP_TEARDOWN_READY:-false}" == "true" ]]; then
    echo "  Internal IP:       ${HYBRID_INTERNAL_IP_TEARDOWN_NAME} (hybrid tier; deleted only after the VM is confirmed gone)"
  fi
  echo ""
  if [[ -n "$CONFIG_FILE" && ! -t 0 ]]; then
    info "Non-interactive mode: proceeding with teardown."
  else
    read -rp "Continue? [y/N]: " CONFIRM
    if [[ "$(echo "$CONFIRM" | tr '[:upper:]' '[:lower:]')" != "y" ]]; then
      echo "Aborted."
      exit 0
    fi
  fi

  K8S_TEARDOWN_OK=true
  PROXY_SERVICE_DELETED=false
  PROXY_SERVICE_NOT_FOUND=false
  VM_DELETED=false
  VM_NOT_FOUND=false
  VM_GONE=false
  NAT_DELETED=false
  NAT_NOT_FOUND=false
  ROUTER_DELETED=false
  ROUTER_NOT_FOUND=false
  SA_DELETED=false
  SA_NOT_FOUND=false
  PROXY_SA_DELETED=false
  PROXY_SA_NOT_FOUND=false
  FW_RULE_DELETED=false
  FW_RULE_NOT_FOUND=false
  FW_8080_RULE_DELETED=false
  FW_8080_RULE_NOT_FOUND=false
  HYBRID_TEARDOWN_DELETED=()
  # Declared here, unconditionally, so the internal-IP delete gate below
  # can check its count even on a run where hybrid_teardown_delete (the
  # only place that normally populates it) never gets called at all --
  # for example, no hybrid firewall rules were ever created for this hub
  # but the internal IP reservation still was.
  HYBRID_TEARDOWN_DELETE_FAILED=()
  # Set whenever hybrid_teardown_delete is never even attempted for a
  # queued rule (a k8s failure or an unconfirmed VM stop everything
  # before it's reached), so the summary can tell "never attempted,
  # here's why" apart from "attempted and failed" the same way the
  # internal-IP reservation already does.
  HYBRID_RULES_DELETE_SKIP_REASON=""

  if [[ "$HYBRID_K8S_TEARDOWN_READY" == "true" ]]; then
    info "Deleting hybrid-tier Kubernetes objects..."
    hybrid_k8s_teardown_delete
    if [[ ${#HYBRID_K8S_TEARDOWN_DELETE_FAILED[@]} -gt 0 ]]; then
      TEARDOWN_HAD_FAILURE=true
      K8S_TEARDOWN_OK=false
      err "Stopping teardown here: a hybrid-tier Kubernetes object failed to delete. Cloud Run, the VM, and every other resource below are being kept untouched; re-run teardown once the failure above is resolved."
    fi
  fi

  if [[ "$K8S_TEARDOWN_OK" == "true" ]]; then
    info "Deleting Cloud Run IAP proxy service..."
    PROXY_SERVICE_DELETE_ERR="$(mktemp)"
    if gcloud run services delete "${PROXY_SERVICE}" \
        --region="${REGION}" --project="${PROJECT_ID}" --quiet 2>"${PROXY_SERVICE_DELETE_ERR}"; then
      echo "  Deleted: ${PROXY_SERVICE}"
      PROXY_SERVICE_DELETED=true
    elif _hybrid_gcloud_not_found "$(cat "${PROXY_SERVICE_DELETE_ERR}")"; then
      warn "Cloud Run service ${PROXY_SERVICE} not found or already deleted."
      PROXY_SERVICE_NOT_FOUND=true
    else
      # Not a confirmed not-found -- warn and continue, never fail the run
      # over an ambiguous base-resource delete outcome.
      warn "Cloud Run service ${PROXY_SERVICE} could not be deleted: $(cat "${PROXY_SERVICE_DELETE_ERR}")"
    fi
    rm -f "${PROXY_SERVICE_DELETE_ERR}"

    # The hybrid firewall rules and the static internal IP
    # reservation are only safe to delete once this VM is confirmed gone
    # (the reservation is still attached to the VM's NIC until the VM
    # itself is deleted, so deleting it earlier would fail anyway). With
    # the tier off (nothing hybrid-tier queued), a VM delete failure
    # warns and continues -- nothing downstream
    # depends on the VM's fate. With the tier on, "gone" must be a
    # positive, project-wide answer rather than inferred from a
    # `describe` in a possibly-wrong zone (ZONE above falls back to a
    # guess when its own discovery call fails): a non-zero exit from the
    # check itself means unknown, and unknown is never treated as gone.
    info "Deleting GCE VM..."
    VM_DELETE_ERR="$(mktemp)"
    if gcloud compute instances delete "${INSTANCE_NAME}" \
        --zone="${ZONE}" --project="${PROJECT_ID}" --quiet 2>"${VM_DELETE_ERR}"; then
      echo "  Deleted: ${INSTANCE_NAME}"
      VM_GONE=true
      VM_DELETED=true
    elif [[ ${#HYBRID_TEARDOWN_DELETE[@]} -eq 0 && "${HYBRID_INTERNAL_IP_TEARDOWN_READY:-false}" != "true" ]]; then
      # Tier off: warn and continue, never fail the run over this -- but
      # reads the delete's own error text, as every other base resource
      # does, instead of assuming "not found" from any failure. A
      # permission or API error must be reported as kept, not as a false
      # "not found" that could hide a still-existing VM.
      if _hybrid_gcloud_not_found "$(cat "${VM_DELETE_ERR}")"; then
        warn "GCE VM ${INSTANCE_NAME} not found or already deleted."
        VM_GONE=true
        VM_NOT_FOUND=true
      else
        warn "GCE VM ${INSTANCE_NAME} could not be confirmed deleted: $(cat "${VM_DELETE_ERR}")"
      fi
    else
      VM_LIST_ERR_FILE="$(mktemp)"
      # This list has no --zones, so it's a project-wide AggregatedList.
      # The SDK's default compute/allow_partial_error=true downgrades an
      # UNREACHABLE zone to a stderr warning and exit 0 with that zone's
      # instances silently missing from the output -- which would read as
      # "gone" even when the VM's own delete just failed because that
      # same zone is down. Setting this to false makes a partial result
      # raise instead, landing in the "could not confirm" branch below
      # rather than being misread as "gone".
      if VM_LIST_OUTPUT="$(CLOUDSDK_COMPUTE_ALLOW_PARTIAL_ERROR=false gcloud compute instances list --project="${PROJECT_ID}" \
          --filter="name=${INSTANCE_NAME}" --format="value(name)" 2>"${VM_LIST_ERR_FILE}")"; then
        if [[ -z "$VM_LIST_OUTPUT" ]]; then
          warn "GCE VM ${INSTANCE_NAME} not found or already deleted."
          VM_GONE=true
          VM_NOT_FOUND=true
        else
          err "Failed to delete GCE VM ${INSTANCE_NAME}; it still exists."
          TEARDOWN_HAD_FAILURE=true
        fi
      else
        err "Failed to delete GCE VM ${INSTANCE_NAME}, and could not confirm whether it still exists:"
        err "  $(cat "${VM_LIST_ERR_FILE}")"
        TEARDOWN_HAD_FAILURE=true
      fi
      rm -f "${VM_LIST_ERR_FILE}"
    fi
    rm -f "${VM_DELETE_ERR}"

    info "Deleting Cloud NAT..."
    NAT_DELETE_ERR="$(mktemp)"
    if gcloud compute routers nats delete "${NAT_NAME}" \
        --router="${ROUTER_NAME}" \
        --region="${REGION}" --project="${PROJECT_ID}" --quiet 2>"${NAT_DELETE_ERR}"; then
      echo "  Deleted: ${NAT_NAME}"
      NAT_DELETED=true
    elif _hybrid_gcloud_not_found "$(cat "${NAT_DELETE_ERR}")"; then
      warn "Cloud NAT ${NAT_NAME} not found or already deleted."
      NAT_NOT_FOUND=true
    else
      warn "Cloud NAT ${NAT_NAME} could not be deleted: $(cat "${NAT_DELETE_ERR}")"
    fi
    rm -f "${NAT_DELETE_ERR}"

    info "Deleting Cloud Router..."
    ROUTER_DELETE_ERR="$(mktemp)"
    if gcloud compute routers delete "${ROUTER_NAME}" \
        --region="${REGION}" --project="${PROJECT_ID}" --quiet 2>"${ROUTER_DELETE_ERR}"; then
      echo "  Deleted: ${ROUTER_NAME}"
      ROUTER_DELETED=true
    elif _hybrid_gcloud_not_found "$(cat "${ROUTER_DELETE_ERR}")"; then
      warn "Cloud Router ${ROUTER_NAME} not found or already deleted."
      ROUTER_NOT_FOUND=true
    else
      warn "Cloud Router ${ROUTER_NAME} could not be deleted: $(cat "${ROUTER_DELETE_ERR}")"
    fi
    rm -f "${ROUTER_DELETE_ERR}"

    info "Deleting service account..."
    SA_DELETE_ERR="$(mktemp)"
    if gcloud iam service-accounts delete "${SA_EMAIL}" \
        --project="${PROJECT_ID}" --quiet 2>"${SA_DELETE_ERR}"; then
      echo "  Deleted: ${SA_EMAIL}"
      SA_DELETED=true
    elif _hybrid_gcloud_not_found "$(cat "${SA_DELETE_ERR}")"; then
      warn "Service account ${SA_EMAIL} not found or already deleted."
      SA_NOT_FOUND=true
    else
      warn "Service account ${SA_EMAIL} could not be deleted: $(cat "${SA_DELETE_ERR}")"
    fi
    rm -f "${SA_DELETE_ERR}"

    info "Deleting proxy service account..."
    PROXY_SA_DELETE_ERR="$(mktemp)"
    if gcloud iam service-accounts delete "${PROXY_SA_EMAIL}" \
        --project="${PROJECT_ID}" --quiet 2>"${PROXY_SA_DELETE_ERR}"; then
      echo "  Deleted: ${PROXY_SA_EMAIL}"
      PROXY_SA_DELETED=true
    elif _hybrid_gcloud_not_found "$(cat "${PROXY_SA_DELETE_ERR}")"; then
      warn "Proxy service account ${PROXY_SA_EMAIL} not found or already deleted."
      PROXY_SA_NOT_FOUND=true
    else
      warn "Proxy service account ${PROXY_SA_EMAIL} could not be deleted: $(cat "${PROXY_SA_DELETE_ERR}")"
    fi
    rm -f "${PROXY_SA_DELETE_ERR}"

    info "Deleting agent transport service account (if present)..."
    PROXY_SERVICE_GONE=false
    if [[ "$PROXY_SERVICE_DELETED" == "true" || "${PROXY_SERVICE_NOT_FOUND:-false}" == "true" ]]; then
      PROXY_SERVICE_GONE=true
    fi
    hybrid_teardown_transport_sa "${HUB_NAME}" "${PROJECT_ID}" "${PROXY_SERVICE}" "${REGION}" "${PROXY_SERVICE_GONE}"
    if [[ "$HYBRID_TRANSPORT_SA_DELETE_FAILED" == "true" ]]; then
      TEARDOWN_HAD_FAILURE=true
    fi

    # Note: We intentionally do NOT revoke roles/iap.tunnelResourceAccessor from
    # the deployer. This role is bound to the operator (not a service account) and
    # may be used for IAP SSH access to other VMs in the project. Revoking it here
    # would silently break access to those other resources.
    info "Skipping IAP tunnel role cleanup (operator may use it for other VMs)."

    info "Deleting IAP SSH firewall rule..."
    FW_RULE_DELETE_ERR="$(mktemp)"
    if gcloud compute firewall-rules delete "${FW_RULE_NAME}" \
        --project="${PROJECT_ID}" --quiet 2>"${FW_RULE_DELETE_ERR}"; then
      echo "  Deleted: ${FW_RULE_NAME}"
      FW_RULE_DELETED=true
    elif _hybrid_gcloud_not_found "$(cat "${FW_RULE_DELETE_ERR}")"; then
      warn "Firewall rule ${FW_RULE_NAME} not found or already deleted."
      FW_RULE_NOT_FOUND=true
    else
      warn "Firewall rule ${FW_RULE_NAME} could not be deleted: $(cat "${FW_RULE_DELETE_ERR}")"
    fi
    rm -f "${FW_RULE_DELETE_ERR}"

    info "Deleting proxy-to-VM firewall rule..."
    FW_8080_RULE_DELETE_ERR="$(mktemp)"
    if gcloud compute firewall-rules delete "${FW_8080_RULE_NAME}" \
        --project="${PROJECT_ID}" --quiet 2>"${FW_8080_RULE_DELETE_ERR}"; then
      echo "  Deleted: ${FW_8080_RULE_NAME}"
      FW_8080_RULE_DELETED=true
    elif _hybrid_gcloud_not_found "$(cat "${FW_8080_RULE_DELETE_ERR}")"; then
      warn "Firewall rule ${FW_8080_RULE_NAME} not found or already deleted."
      FW_8080_RULE_NOT_FOUND=true
    else
      warn "Firewall rule ${FW_8080_RULE_NAME} could not be deleted: $(cat "${FW_8080_RULE_DELETE_ERR}")"
    fi
    rm -f "${FW_8080_RULE_DELETE_ERR}"

    if [[ ${#HYBRID_TEARDOWN_DELETE[@]} -gt 0 ]]; then
      if [[ "$VM_GONE" == "true" ]]; then
        info "Deleting hybrid-tier firewall rules..."
        hybrid_teardown_delete "$PROJECT_ID"
        if [[ ${#HYBRID_TEARDOWN_DELETE_FAILED[@]} -gt 0 ]]; then
          TEARDOWN_HAD_FAILURE=true
        fi
      else
        err "Keeping the hybrid-tier firewall rules and internal IP reservation because GCE VM ${INSTANCE_NAME} still exists; the pod-range deny and NFS access rules and the reserved address stay in place. Re-run teardown after the VM is deleted."
        HYBRID_RULES_DELETE_SKIP_REASON="VM not confirmed gone"
        TEARDOWN_HAD_FAILURE=true
      fi
    fi

    if [[ "$HYBRID_INTERNAL_IP_TEARDOWN_READY" == "true" ]]; then
      if [[ ${#HYBRID_TEARDOWN_DELETE_FAILED[@]} -eq 0 ]]; then
        info "Deleting hybrid-tier internal IP reservation..."
        hybrid_internal_ip_teardown_delete "$PROJECT_ID" "$REGION" "$VM_GONE"
        if [[ "$HYBRID_INTERNAL_IP_DELETE_FAILED" == "true" ]]; then
          TEARDOWN_HAD_FAILURE=true
        fi
      else
        warn "Keeping internal IP reservation ${HYBRID_INTERNAL_IP_TEARDOWN_NAME}: a hybrid-tier firewall rule failed to delete; re-run teardown once that's resolved."
        HYBRID_INTERNAL_IP_DELETED=false
        HYBRID_INTERNAL_IP_DELETE_FAILED=true
        HYBRID_INTERNAL_IP_DELETE_SKIP_REASON="a hybrid-tier firewall rule failed to delete"
        TEARDOWN_HAD_FAILURE=true
      fi
    fi
  else
    # A hybrid-tier Kubernetes object failed to delete: nothing below
    # this point ever ran, so any queued firewall rules and the internal
    # IP reservation were never even attempted, not just kept after a
    # failed attempt.
    HYBRID_RULES_DELETE_SKIP_REASON="a hybrid-tier Kubernetes object failed to delete"
    if [[ "$HYBRID_INTERNAL_IP_TEARDOWN_READY" == "true" ]]; then HYBRID_INTERNAL_IP_DELETE_SKIP_REASON="a hybrid-tier Kubernetes object failed to delete"; fi
    TEARDOWN_HAD_FAILURE=true
  fi

  echo ""
  echo -e "${BOLD}=== Teardown Complete ===${RESET}"
  echo ""
  if [[ "$HYBRID_K8S_TEARDOWN_READY" == "true" ]]; then
    for k8s_kind in ${HYBRID_K8S_TEARDOWN_DELETED[@]+"${HYBRID_K8S_TEARDOWN_DELETED[@]}"}; do
      echo "  Deleted Kubernetes object:  $(_hybrid_k8s_display "$k8s_kind")"
    done
    for k8s_kind in ${HYBRID_K8S_TEARDOWN_DELETE_FAILED[@]+"${HYBRID_K8S_TEARDOWN_DELETE_FAILED[@]}"}; do
      echo "  Kept Kubernetes object:     $(_hybrid_k8s_display "$k8s_kind") (delete failed or not attempted)"
    done
  fi
  if [[ -n "$K8S_GKE_NAME" ]]; then
    echo "  Kubernetes cluster:         ${K8S_GKE_NAME} -- never deleted (this tier never creates or deletes the cluster itself)"
  fi
  if [[ "$PROXY_SERVICE_DELETED" == "true" ]]; then
    echo "  Deleted Cloud Run service:  ${PROXY_SERVICE}"
  elif [[ "${PROXY_SERVICE_NOT_FOUND:-false}" == "true" ]]; then
    echo "  Not found Cloud Run service: ${PROXY_SERVICE}"
  else
    echo "  Kept Cloud Run service:     ${PROXY_SERVICE}"
  fi
  if [[ "$VM_DELETED" == "true" ]]; then
    echo "  Deleted GCE VM:             ${INSTANCE_NAME}"
  elif [[ "${VM_NOT_FOUND:-false}" == "true" ]]; then
    echo "  Not found GCE VM:           ${INSTANCE_NAME}"
  else
    echo "  Kept GCE VM:                ${INSTANCE_NAME}"
  fi
  if [[ "$NAT_DELETED" == "true" ]]; then
    echo "  Deleted Cloud NAT:          ${NAT_NAME}"
  elif [[ "${NAT_NOT_FOUND:-false}" == "true" ]]; then
    echo "  Not found Cloud NAT:        ${NAT_NAME}"
  else
    echo "  Kept Cloud NAT:             ${NAT_NAME}"
  fi
  if [[ "$ROUTER_DELETED" == "true" ]]; then
    echo "  Deleted Cloud Router:       ${ROUTER_NAME}"
  elif [[ "${ROUTER_NOT_FOUND:-false}" == "true" ]]; then
    echo "  Not found Cloud Router:     ${ROUTER_NAME}"
  else
    echo "  Kept Cloud Router:          ${ROUTER_NAME}"
  fi
  if [[ "$SA_DELETED" == "true" ]]; then
    echo "  Deleted service account:    ${SA_EMAIL}"
  elif [[ "${SA_NOT_FOUND:-false}" == "true" ]]; then
    echo "  Not found service account:  ${SA_EMAIL}"
  else
    echo "  Kept service account:       ${SA_EMAIL}"
  fi
  if [[ "$PROXY_SA_DELETED" == "true" ]]; then
    echo "  Deleted proxy SA:           ${PROXY_SA_EMAIL}"
  elif [[ "${PROXY_SA_NOT_FOUND:-false}" == "true" ]]; then
    echo "  Not found proxy SA:         ${PROXY_SA_EMAIL}"
  else
    echo "  Kept proxy SA:              ${PROXY_SA_EMAIL}"
  fi
  if [[ -n "${HYBRID_TRANSPORT_SA_TEARDOWN_EMAIL:-}" ]]; then
    if [[ "${HYBRID_TRANSPORT_SA_DELETED:-false}" == "true" ]]; then
      echo "  Deleted transport SA:       ${HYBRID_TRANSPORT_SA_TEARDOWN_EMAIL}"
    elif [[ "${HYBRID_TRANSPORT_SA_NOT_FOUND:-false}" == "true" ]]; then
      echo "  Not found transport SA:     ${HYBRID_TRANSPORT_SA_TEARDOWN_EMAIL}"
    else
      echo "  Kept transport SA:          ${HYBRID_TRANSPORT_SA_TEARDOWN_EMAIL} (${HYBRID_TRANSPORT_SA_KEPT_REASON:-not attempted})"
    fi
    case "${HYBRID_TRANSPORT_SA_BINDING_STATE:-not-attempted}" in
      removed) echo "  Removed transport SA IAP access on: ${PROXY_SERVICE}" ;;
      absent)  echo "  No transport SA IAP access left on: ${PROXY_SERVICE}" ;;
      failed)  echo "  Kept transport SA IAP access on:    ${PROXY_SERVICE} (removal failed)" ;;
    esac
  fi
  if [[ "$FW_RULE_DELETED" == "true" ]]; then
    echo "  Deleted firewall rule:      ${FW_RULE_NAME}"
  elif [[ "${FW_RULE_NOT_FOUND:-false}" == "true" ]]; then
    echo "  Not found firewall rule:    ${FW_RULE_NAME}"
  else
    echo "  Kept firewall rule:         ${FW_RULE_NAME}"
  fi
  if [[ "$FW_8080_RULE_DELETED" == "true" ]]; then
    echo "  Deleted firewall rule:      ${FW_8080_RULE_NAME}"
  elif [[ "${FW_8080_RULE_NOT_FOUND:-false}" == "true" ]]; then
    echo "  Not found firewall rule:    ${FW_8080_RULE_NAME}"
  else
    echo "  Kept firewall rule:         ${FW_8080_RULE_NAME}"
  fi
  for name in ${HYBRID_TEARDOWN_DELETE[@]+"${HYBRID_TEARDOWN_DELETE[@]}"}; do
    HYBRID_RULE_WAS_DELETED=false
    for hd in ${HYBRID_TEARDOWN_DELETED[@]+"${HYBRID_TEARDOWN_DELETED[@]}"}; do
      [[ "$hd" == "$name" ]] && HYBRID_RULE_WAS_DELETED=true
    done
    if [[ "$HYBRID_RULE_WAS_DELETED" == "true" ]]; then
      echo "  Deleted firewall rule:      ${name}"
      continue
    fi
    HYBRID_RULE_DELETE_FAILED=false
    for hf in ${HYBRID_TEARDOWN_DELETE_FAILED[@]+"${HYBRID_TEARDOWN_DELETE_FAILED[@]}"}; do
      [[ "$hf" == "$name" ]] && HYBRID_RULE_DELETE_FAILED=true
    done
    if [[ "$HYBRID_RULE_DELETE_FAILED" == "true" ]]; then
      echo "  Kept firewall rule:         ${name} (delete failed, or not attempted after an earlier rule's delete failed)"
    elif [[ -n "${HYBRID_RULES_DELETE_SKIP_REASON:-}" ]]; then
      echo "  SKIPPED firewall rule:      ${name} (${HYBRID_RULES_DELETE_SKIP_REASON})"
    else
      echo "  Kept firewall rule:         ${name}"
    fi
  done
  if [[ -n "${HYBRID_INTERNAL_IP_TEARDOWN_NAME:-}" ]]; then
    if [[ "${HYBRID_INTERNAL_IP_DELETED:-false}" == "true" ]]; then
      echo "  Deleted internal IP:       ${HYBRID_INTERNAL_IP_TEARDOWN_NAME}"
    elif [[ -n "${HYBRID_INTERNAL_IP_DELETE_SKIP_REASON:-}" ]]; then
      echo "  SKIPPED internal IP:       ${HYBRID_INTERNAL_IP_TEARDOWN_NAME} (${HYBRID_INTERNAL_IP_DELETE_SKIP_REASON})"
    elif [[ "${HYBRID_INTERNAL_IP_TEARDOWN_READY:-false}" == "true" ]]; then
      echo "  Kept internal IP:          ${HYBRID_INTERNAL_IP_TEARDOWN_NAME} (delete failed: ${HYBRID_INTERNAL_IP_DELETE_ERR:-unknown error})"
    fi
  fi
  if [[ "$TEARDOWN_HAD_FAILURE" == "true" ]]; then
    err "Teardown completed with at least one failure reported above."
    exit 1
  fi
  exit 0
fi

# ===================================================================
# Phase 1: Prerequisites
# ===================================================================
section "Phase 1: Prerequisites"

# jq is required later (Phase 2) to safely detect an existing Cloud NAT
# before creating any resources -- see the Cloud NAT reuse check below.
# Checked here, with the other prerequisites, so a missing jq is caught
# before any prompts, API-enable calls, or release detection run, not after
# most of Phase 2 has already happened.
if ! command -v jq &>/dev/null; then
  err "jq is required (used to safely detect an existing Cloud NAT before creating resources; see docs/deploy/agent-runbook-single-node-vm.md, Cloud NAT reuse). Install jq and re-run."
  exit 1
fi

# --- GCP project ---
PROJECT_ID="$(config_get 'project_id' '')"
if [[ -z "$PROJECT_ID" ]]; then
  info "Detecting GCP project..."
  PROJECT_ID="$(gcloud config get-value project 2>/dev/null)" || true
fi
if [[ -z "$PROJECT_ID" ]]; then
  err "No GCP project configured. Set project_id in config or run: gcloud config set project PROJECT_ID"
  exit 1
fi
echo "  Project: ${PROJECT_ID}"

# --- Interactive prompts (or config file values) ---
HUB_NAME="$(config_get 'hub_name' '')"
if [[ -n "$HUB_NAME" ]]; then
  # Validate config-provided hub name
  if [[ ${#HUB_NAME} -gt 20 ]]; then
    err "Hub name '${HUB_NAME}' from config is ${#HUB_NAME} chars; max is 20."
    exit 1
  fi
  if [[ ! "$HUB_NAME" =~ ^[a-z][a-z0-9-]*$ ]]; then
    err "Hub name '${HUB_NAME}' from config is invalid. It must start with a lowercase letter and contain only lowercase letters, numbers, and hyphens."
    exit 1
  fi
else
  if [[ -n "$CONFIG_FILE" && ! -t 0 ]]; then
    err "Required config value 'hub_name' is missing and stdin is not a terminal."
    exit 1
  fi
  while true; do
    read -rp "Hub name [my-hub]: " HUB_NAME
    HUB_NAME="${HUB_NAME:-my-hub}"
    if [[ ${#HUB_NAME} -gt 20 ]]; then
      warn "Hub name '${HUB_NAME}' is ${#HUB_NAME} chars; max is 20 (GCP service-account ID limit)."
      echo "  Please choose a shorter name."
      continue
    fi
    if [[ ! "$HUB_NAME" =~ ^[a-z][a-z0-9-]*$ ]]; then
      warn "Hub name '${HUB_NAME}' is invalid. It must start with a lowercase letter and contain only lowercase letters, numbers, and hyphens."
      echo "  Please choose a valid name."
      continue
    fi
    break
  done
fi

REGION="$(config_get 'region' '')"
if [[ -z "$REGION" ]]; then
  config_prompt REGION "GCP region [us-central1]: " "us-central1"
fi

CFG_MACHINE_SIZE="$(config_get 'machine_size' '')"
if [[ -n "$CFG_MACHINE_SIZE" ]]; then
  case "$CFG_MACHINE_SIZE" in
    small)  MACHINE_TYPE="e2-standard-4" ;;
    medium) MACHINE_TYPE="n2-standard-16" ;;
    *) err "Invalid machine_size in config: '$CFG_MACHINE_SIZE' (expected: small, medium)"; exit 1 ;;
  esac
else
  echo "Machine size:"
  echo "  1) Small  (e2-standard-4,  4 vCPU,  16GB) - up to ~10 agents"
  echo "  2) Medium (n2-standard-16, 16 vCPU, 64GB) - up to ~50 agents"
  config_prompt SIZE_CHOICE "Select [1]: " "1"

  case "$SIZE_CHOICE" in
    1) MACHINE_TYPE="e2-standard-4" ;;
    2) MACHINE_TYPE="n2-standard-16" ;;
    *) err "Invalid selection: $SIZE_CHOICE"; exit 1 ;;
  esac
fi

CFG_DISK_SIZE="$(config_get 'disk_size_gb' '')"
if [[ -n "$CFG_DISK_SIZE" ]]; then
  if ! [[ "$CFG_DISK_SIZE" =~ ^[0-9]+$ ]]; then
    err "Invalid disk_size_gb in config: '$CFG_DISK_SIZE' (must be a number)"
    exit 1
  fi
  DISK_SIZE="${CFG_DISK_SIZE}GB"
else
  echo "Disk size:"
  echo "  1) 200 GB (default)"
  echo "  2) 500 GB"
  echo "  3) Custom"
  config_prompt DISK_CHOICE "Select [1]: " "1"

  case "$DISK_CHOICE" in
    1) DISK_SIZE="200GB" ;;
    2) DISK_SIZE="500GB" ;;
    3)
      config_prompt CUSTOM_DISK "Enter disk size in GB: " ""
      if [[ -z "$CUSTOM_DISK" ]] || ! [[ "$CUSTOM_DISK" =~ ^[0-9]+$ ]]; then
        err "Invalid disk size: $CUSTOM_DISK"
        exit 1
      fi
      DISK_SIZE="${CUSTOM_DISK}GB"
      ;;
    *) err "Invalid selection: $DISK_CHOICE"; exit 1 ;;
  esac
fi

CFG_CHAT_PLUGINS="$(config_get 'chat_plugins' '')"
CHAT_PLUGINS=()
if [[ -n "$CONFIG_FILE" && -f "$CONFIG_FILE" ]]; then
  # Parse chat_plugins from config (space-separated list from config_get)
  if [[ -n "$CFG_CHAT_PLUGINS" ]]; then
    for plugin in $CFG_CHAT_PLUGINS; do
      case "$plugin" in
        telegram|discord|slack|teams) CHAT_PLUGINS+=("$plugin") ;;
        *) warn "Ignoring unknown chat plugin in config: $plugin" ;;
      esac
    done
  fi
  # Empty list or missing key = no plugins (this is fine)
else
  echo "Chat integrations to install:"
  echo "  1) Telegram"
  echo "  2) Discord"
  echo "  3) Slack"
  echo "  4) Teams"
  echo "  5) None"
  config_prompt CHAT_CHOICE "Select (comma-separated) [5]: " "5"

  IFS=',' read -ra CHAT_SELECTIONS <<< "$CHAT_CHOICE"
  for sel in "${CHAT_SELECTIONS[@]}"; do
    sel="$(echo "$sel" | tr -d ' ')"
    case "$sel" in
      1) CHAT_PLUGINS+=("telegram") ;;
      2) CHAT_PLUGINS+=("discord") ;;
      3) CHAT_PLUGINS+=("slack") ;;
      4) CHAT_PLUGINS+=("teams") ;;
      5) ;;  # None
      *) warn "Ignoring unknown chat selection: $sel" ;;
    esac
  done
fi

CFG_IMAGE_SOURCE="$(config_get 'container_images.source' '')"
if [[ -n "$CFG_IMAGE_SOURCE" ]]; then
  case "$CFG_IMAGE_SOURCE" in
    registry)
      IMAGE_SOURCE="registry"
      IMAGE_REGISTRY="$(config_get 'container_images.registry' '')"
      if [[ -z "$IMAGE_REGISTRY" ]]; then
        err "container_images.source is 'registry' but container_images.registry is missing from config."
        exit 1
      fi
      ;;
    build)
      IMAGE_SOURCE="build"
      IMAGE_REGISTRY="localhost/scion"
      ;;
    *) err "Invalid container_images.source in config: '$CFG_IMAGE_SOURCE' (expected: registry, build)"; exit 1 ;;
  esac
else
  echo "Container images:"
  echo "  1) Provide a registry path (images already pushed)"
  echo "  2) Build images on the VM (requires 10-15 min, ~30GB disk)"
  config_prompt IMAGE_CHOICE "Select [2]: " "2"

  case "$IMAGE_CHOICE" in
    1)
      IMAGE_SOURCE="registry"
      config_prompt IMAGE_REGISTRY "Registry path (e.g. us-docker.pkg.dev/my-project/scion): " ""
      if [[ -z "$IMAGE_REGISTRY" ]]; then
        err "Registry path cannot be empty."
        exit 1
      fi
      ;;
    2)
      IMAGE_SOURCE="build"
      IMAGE_REGISTRY="localhost/scion"
      ;;
    *) err "Invalid selection: $IMAGE_CHOICE"; exit 1 ;;
  esac
fi

# Force a rebuild even if the version marker and all 3 images already match
# the requested VERSION. Defaults to false: normally a matching marker means
# Phase 3b can skip the 10-15 min build entirely. Either the config key or
# the --rebuild-images CLI flag forces a rebuild.
CFG_FORCE_REBUILD="$(config_get 'container_images.force_rebuild' 'false')"
if [[ "$CLI_REBUILD_IMAGES" == "true" ]]; then
  CFG_FORCE_REBUILD="true"
fi

# --- Admin email ---
ADMIN_EMAIL="$(config_get 'admin_email' '')"
if [[ -z "$ADMIN_EMAIL" ]]; then
  DEPLOYER_DEFAULT="$(gcloud auth list --filter=status:ACTIVE --format='value(account)' 2>/dev/null | head -1)" || true
  if [[ -n "$CONFIG_FILE" && ! -t 0 ]]; then
    # Non-interactive: use deployer identity as default
    ADMIN_EMAIL="${DEPLOYER_DEFAULT:-}"
  else
    echo ""
    echo "Hub admin email (will be granted super-admin access):"
    if [[ -n "$DEPLOYER_DEFAULT" ]]; then
      read -rp "Admin email [${DEPLOYER_DEFAULT}]: " ADMIN_EMAIL
      ADMIN_EMAIL="${ADMIN_EMAIL:-$DEPLOYER_DEFAULT}"
    else
      read -rp "Admin email: " ADMIN_EMAIL
    fi
  fi
fi
if [[ -z "$ADMIN_EMAIL" ]]; then
  warn "No admin email provided. You can add one later in settings.yaml under server.hub.admin_emails."
fi

# --- Update policy ---
CFG_UPDATE_POLICY="$(config_get 'update_policy' '')"
if [[ -n "$CFG_UPDATE_POLICY" ]]; then
  case "$CFG_UPDATE_POLICY" in
    auto|notify|disabled) UPDATE_POLICY="$CFG_UPDATE_POLICY" ;;
    *) err "Invalid update_policy in config: '$CFG_UPDATE_POLICY' (expected: auto, notify, disabled)"; exit 1 ;;
  esac
else
  echo ""
  echo "Automatic update policy:"
  echo "  1) Auto - automatically install new releases (recommended)"
  echo "  2) Notify - check for updates, notify admin only"
  echo "  3) Disabled - no automatic update checking"
  config_prompt UPDATE_CHOICE "Select [1]: " "1"
  case "${UPDATE_CHOICE:-1}" in
    1) UPDATE_POLICY="auto" ;;
    2) UPDATE_POLICY="notify" ;;
    3) UPDATE_POLICY="disabled" ;;
    *) UPDATE_POLICY="auto" ;;
  esac
fi

# --- Hybrid tier (optional GKE attach target) ---
hybrid_read_config "$PROJECT_ID" "$HUB_NAME"
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  echo "  Hybrid tier: enabled (GKE cluster: ${GKE_NAME}, location: ${GKE_LOCATION})"
  if [[ "$IMAGE_SOURCE" == "build" ]]; then
    err "The hybrid tier is on, but container_images.source is 'build'. GKE nodes cannot pull images from the VM's local Docker store."
    err "Set container_images.source to 'registry' and container_images.registry to a registry the cluster's node service account can read (for example an Artifact Registry repository with artifactregistry.reader granted to that service account)."
    exit 1
  fi
  if _hybrid_registry_is_loopback "$IMAGE_REGISTRY"; then
    err "The hybrid tier is on, but container_images.registry ('${IMAGE_REGISTRY}') names this VM itself (a loopback address). GKE nodes cannot reach it there."
    err "Set container_images.registry to a registry the cluster's node service account can read (for example an Artifact Registry repository with artifactregistry.reader granted to that service account)."
    exit 1
  fi
  # The hybrid tier configures restricted user access (invite-only by
  # default): validated here, before any create, and spliced into both
  # settings.yaml writes below as HYBRID_USER_ACCESS_YAML.
  hybrid_resolve_user_access "$ADMIN_EMAIL"
  echo "  User access:  ${HYBRID_USER_ACCESS_MODE}"
else
  # Empty when the tier is off, so both settings.yaml writes render
  # exactly as they do without the tier.
  HYBRID_USER_ACCESS_YAML=""
  if hybrid_user_access_config_present; then
    warn "user_access_mode / authorized_domains in the config file are applied only when the hybrid tier is on; ignoring them."
  fi
fi

# Derived values
info "Selecting zone in ${REGION}..."
ZONE="$(gcloud compute zones list \
  --filter="region=${REGION}" \
  --limit=1 \
  --format="value(name)" \
  --project="${PROJECT_ID}" 2>/dev/null)" || true
if [[ -z "$ZONE" ]]; then
  ZONE="${REGION}-b"
  warn "Could not discover zone dynamically; defaulting to ${ZONE}"
fi
INSTANCE_NAME="scion-hub-${HUB_NAME}"
HUB_TAG="scion-hub-${HUB_NAME}"
SA_NAME="scion-hub-${HUB_NAME}"
# GCP service-account IDs must be 6-30 chars; truncate as a safety net
if [[ ${#SA_NAME} -gt 30 ]]; then
  SA_NAME="${SA_NAME:0:30}"
  warn "Service-account name truncated to 30 chars: ${SA_NAME}"
fi
SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

# Dedicated Cloud Run proxy service account -- deliberately never SA_EMAIL
# (the hub VM's own SA), which holds aiplatform.user and
# artifactregistry.writer. The proxy only reverse-proxies HTTP to the VM's
# internal IP; it calls no GCP API and needs no project role (see Phase 2,
# "Proxy service account"). See proxy_sa_name (defined near the top of
# this script, shared with teardown) for how the name itself is derived.
PROXY_SA_NAME="$(proxy_sa_name "$HUB_NAME")"
PROXY_SA_EMAIL="${PROXY_SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

echo ""
echo "  Hub name:     ${HUB_NAME}"
echo "  Region:       ${REGION}"
echo "  Zone:         ${ZONE}"
echo "  Machine type: ${MACHINE_TYPE}"
echo "  Disk size:    ${DISK_SIZE}"
echo "  Instance:     ${INSTANCE_NAME}"
if [[ ${#CHAT_PLUGINS[@]} -gt 0 ]]; then
  echo "  Chat plugins: ${CHAT_PLUGINS[*]}"
else
  echo "  Chat plugins: (none)"
fi
if [[ "$IMAGE_SOURCE" == "registry" ]]; then
  echo "  Images:       registry (${IMAGE_REGISTRY})"
else
  echo "  Images:       build on VM"
fi
if [[ -n "$ADMIN_EMAIL" ]]; then
  echo "  Admin:        ${ADMIN_EMAIL}"
fi
echo "  Update policy: ${UPDATE_POLICY}"

# --- Release version ---
if [[ -z "$VERSION" ]]; then
  info "Detecting latest Scion release..."
  # Try /releases/latest first (excludes pre-releases), fall back to
  # /releases (includes pre-releases) when no full release exists yet.
  RELEASE_JSON="$(curl -fsSL https://api.github.com/repos/GoogleCloudPlatform/scion/releases/latest 2>/dev/null)" || true
  if [[ -z "$RELEASE_JSON" ]]; then
    warn "/releases/latest returned no data (pre-releases only?); querying /releases..."
    RELEASE_JSON="$(curl -fsSL 'https://api.github.com/repos/GoogleCloudPlatform/scion/releases?per_page=1')" \
      || { err "Could not fetch releases from GitHub API."; exit 1; }
  fi
  # jq is a hard prerequisite (checked in Phase 1), so no text-based fallback
  # is needed here. Here-string, not `echo | jq` -- see the NAT_ROWS comment
  # below for why.
  VERSION="$(jq -r 'select(. != null) | if type == "array" then .[0].tag_name else .tag_name end // empty' <<< "$RELEASE_JSON")" || true
  if [[ -z "$VERSION" ]]; then
    err "Could not detect latest release. Use --version to specify."
    exit 1
  fi
fi
echo "  Scion version: ${VERSION}"

# Validate VERSION strictly. It is interpolated into several remote-command
# strings sent over `gcloud compute ssh --command=...` (git clone/checkout,
# the Phase 3b idempotency check, and the marker write inside a nested
# `bash -c '...'` body). A version containing shell metacharacters would
# otherwise be executed on the VM — git check-ref-format happily accepts a
# tag name like "v1;id", and this script's own --version flag or the GitHub
# release auto-detect could pass one through unchecked. Restrict to the
# characters a real release tag needs. Require an alphanumeric first
# character (after the optional 'v') so a leading '-' can never be mistaken
# for a flag by a downstream command.
if [[ ! "$VERSION" =~ ^v?[0-9A-Za-z][0-9A-Za-z._-]*$ ]]; then
  err "Invalid version: '${VERSION}' (expected characters: letters, digits, '.', '_', '-', optionally prefixed with 'v'; must not start with '-')."
  exit 1
fi

# --- Release channel ---
CFG_RELEASE_CHANNEL="$(config_get 'release_channel' '')"
if [[ -n "$CFG_RELEASE_CHANNEL" ]]; then
  case "$CFG_RELEASE_CHANNEL" in
    stable|preview|nightly) RELEASE_CHANNEL="$CFG_RELEASE_CHANNEL" ;;
    *) err "Invalid release_channel in config: '$CFG_RELEASE_CHANNEL' (expected: stable, preview, nightly)"; exit 1 ;;
  esac
else
  # Default to nightly. When running from a git clone (the common agent
  # path), there is no release artifact to detect a channel from. Nightly
  # is the appropriate default for latest-code deployments.
  RELEASE_CHANNEL="nightly"
fi
echo "  Release channel: ${RELEASE_CHANNEL}"

# --- Validate gcloud auth ---
info "Validating gcloud authentication..."
# Try gcloud auth list first (works with service accounts and CI),
# fall back to gcloud config get account.
ACCOUNT="$(gcloud auth list --filter=status:ACTIVE --format='value(account)' 2>/dev/null | head -1)" || true
if [[ -z "$ACCOUNT" ]]; then
  ACCOUNT="$(gcloud config get-value account 2>/dev/null)" || true
fi
if [[ -z "$ACCOUNT" ]]; then
  err "Not authenticated with gcloud. Run: gcloud auth login"
  exit 1
fi
echo "  Authenticated as: ${ACCOUNT}"

# --- Release checksum preflight ---
# Phase 3 below downloads the scion binary and any chat plugins and verifies
# each against a SHA256SUMS checksums asset published alongside the release
# (see .github/workflows/build-release.yml) before installing it. Check here,
# before any GCP resource is created, that the chosen release actually
# publishes one -- a release built before checksum publishing existed would
# otherwise fail deep into Phase 3, after a VM, network, and firewall rules
# already exist. ALLOW_UNVERIFIED_RELEASE=true is an explicit, loud opt-out
# for exactly that transition period; there is no silent fallback.
RELEASE_URL="https://github.com/GoogleCloudPlatform/scion/releases/download/${VERSION}"
# Normalize to exactly "true" or "false": this value is interpolated into
# single-quoted tests in the Phase 3 --command strings below (e.g.
# [ '${ALLOW_UNVERIFIED_RELEASE}' = 'true' ]), so a value containing a
# single quote would alter the remote command text. Operator-controlled
# either way, not a privilege boundary, but VERSION gets the same strict
# treatment a few lines up for the same reason -- keep the remote command
# text predictable regardless of what the caller's environment set.
if [[ "${ALLOW_UNVERIFIED_RELEASE:-false}" == "true" ]]; then
  ALLOW_UNVERIFIED_RELEASE=true
else
  ALLOW_UNVERIFIED_RELEASE=false
fi
info "Checking that release ${VERSION} publishes checksums..."
SHA256SUMS_PREFLIGHT_RC=0
curl -fsSLI -o /dev/null "${RELEASE_URL}/SHA256SUMS" || SHA256SUMS_PREFLIGHT_RC=$?
if [[ "$SHA256SUMS_PREFLIGHT_RC" -eq 0 ]]; then
  echo "  SHA256SUMS found for ${VERSION}; downloads will be verified."
elif [[ "$SHA256SUMS_PREFLIGHT_RC" -ne 22 ]]; then
  # curl -f maps any HTTP error response to exit 22; anything else (DNS
  # failure, TLS error, connection refused/timed out, ...) is a
  # connectivity problem, not evidence the release lacks SHA256SUMS.
  # Sending an operator to ALLOW_UNVERIFIED_RELEASE for a transient network
  # blip would be actively wrong, so don't even mention it here.
  err "Could not reach ${RELEASE_URL}/SHA256SUMS (curl exit ${SHA256SUMS_PREFLIGHT_RC})."
  echo "  This looks like a network or GitHub-availability problem, not a release that lacks" >&2
  echo "  checksums. Check connectivity and retry." >&2
  exit 1
elif [[ "$ALLOW_UNVERIFIED_RELEASE" == "true" ]]; then
  warn "Release ${VERSION} does not publish a SHA256SUMS checksums asset."
  warn "Proceeding WITHOUT checksum verification because ALLOW_UNVERIFIED_RELEASE=true."
else
  err "Release ${VERSION} does not publish a SHA256SUMS checksums asset, so its downloads cannot be verified."
  echo "  This applies to any release published before checksum publishing was added." >&2
  echo "  Choose a release that publishes SHA256SUMS, or set ALLOW_UNVERIFIED_RELEASE=true to install this" >&2
  echo "  release anyway WITHOUT checksum verification (not recommended)." >&2
  exit 1
fi

# ===================================================================
# Phase 2: GCP Resources
# ===================================================================
section "Phase 2: GCP Resources"

# --- Enable APIs ---
info "Enabling required APIs..."
REQUIRED_APIS=(
  compute.googleapis.com
  run.googleapis.com
  iap.googleapis.com
  cloudbuild.googleapis.com
  artifactregistry.googleapis.com
  aiplatform.googleapis.com
  iam.googleapis.com
)
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  REQUIRED_APIS+=(container.googleapis.com)
fi
API_LIST_ERR_FILE="$(mktemp)"
if ENABLED_APIS="$(gcloud services list --enabled --project="${PROJECT_ID}" \
    --format="value(config.name)" 2>"${API_LIST_ERR_FILE}")"; then
  rm -f "${API_LIST_ERR_FILE}"
  MISSING_APIS=()
  for api in "${REQUIRED_APIS[@]}"; do
    if ! grep -qx "$api" <<< "$ENABLED_APIS"; then
      MISSING_APIS+=("$api")
    fi
  done
  if [[ ${#MISSING_APIS[@]} -gt 0 ]]; then
    gcloud services enable "${MISSING_APIS[@]}" --project="${PROJECT_ID}" --quiet
  fi
elif [[ "$HYBRID_ENABLED" == "true" ]]; then
  err "Could not list enabled APIs for project ${PROJECT_ID}, so it's unknown whether container.googleapis.com (needed for the hybrid tier) is already enabled. Refusing to guess: enabling it unconditionally would fail on a runner without serviceusage.services.enable if it's already on, and skipping it would fail later if it's not."
  err "  $(cat "${API_LIST_ERR_FILE}")"
  rm -f "${API_LIST_ERR_FILE}"
  exit 1
else
  rm -f "${API_LIST_ERR_FILE}"
  gcloud services enable "${REQUIRED_APIS[@]}" --project="${PROJECT_ID}" --quiet
fi

# --- IAP service agent identity (idempotent) ---
# Ensures service-PROJECT_NUMBER@gcp-sa-iap.iam.gserviceaccount.com exists
# before Phase 4 grants it roles/run.invoker on the proxy. On a project
# where IAP has never been used before, that agent may not have been
# provisioned yet; without this, the first-ever deploy could reach Phase 4
# with the proxy already live but no agent to grant the role to, and the
# explicit grant there is a hard failure (see Phase 4). A no-op if the
# agent already exists.
gcloud beta services identity create \
  --service=iap.googleapis.com \
  --project="${PROJECT_ID}" --quiet 2>/dev/null || true

# --- Default VPC network must already exist ---
# Organizations enforcing constraints/compute.skipDefaultNetworkCreation
# get no "default" network on new projects, and this script relies on
# --network=default / --subnet=default throughout (Cloud Router, Cloud
# NAT, the hub VM, both firewall rules below, and the Cloud Run proxy's
# Direct VPC egress). Creating a VPC network is a network-design decision
# for the operator to make explicitly, not something to do silently on
# their behalf -- so this fails fast, before any resource in this script
# is created, rather than creating one itself. See the hardened-org
# addendum doc for the exact commands to create it -- either an auto-mode
# network, or (for organizations that block those) a custom-mode network
# with an explicitly-created "default" subnet in this region. Either
# shape works; this script only needs a subnet named "default" to exist
# in REGION.
#
# Runs AFTER "Enable APIs" above, not before: on a brand-new project in an
# ordinary (non-hardened) org, compute.googleapis.com has never been
# enabled yet, and enabling it (just above) is what triggers the default
# network's creation. Checking beforehand would report a false "missing
# network" on every fresh project, hardened org or not. That creation is
# asynchronous, so this polls for a bounded time instead of failing on
# the very first check. Enabling APIs is not itself a resource this
# script would need to tear down, so the "before any resource is created"
# contract still holds for everything from here on.
info "Checking for the default VPC network..."
NETWORK_FOUND=false
NETWORK_CHECK_ERROR=""
for attempt in $(seq 1 "$NETWORK_CHECK_MAX_ATTEMPTS"); do
  # --quiet is required here, not cosmetic: without it, gcloud may ask on
  # stderr whether to enable the API and then read stdin. stderr is
  # captured here, so on an interactive terminal the deploy appears to
  # freeze at this step while waiting for an answer the operator cannot
  # see. An open, non-TTY stdin (a pipe held without EOF) blocks the same
  # way. --quiet takes the default answer ("no") immediately, so the
  # SERVICE_DISABLED error reaches the retry check below instead.
  if NETWORK_CHECK_ERROR="$(gcloud compute networks describe default \
      --project="${PROJECT_ID}" --quiet 2>&1 >/dev/null)"; then
    NETWORK_FOUND=true
    break
  fi
  # A genuine "not found" is worth retrying -- the network can still be
  # propagating from the services-enable call above. So is
  # SERVICE_DISABLED -- on a truly fresh project, compute.googleapis.com
  # itself can still be settling right after being enabled, and describe
  # calls against it fail with the same "has not been used in project"
  # error enabling the API is meant to fix. Matched on both the
  # machine-readable reason (present in the error's details block) and
  # the message fragment (present even when the API-enablement prompt is
  # disabled and gcloud never renders a details block at all), since
  # either form can be all that's available depending on gcloud's
  # configuration. Any other error (permission denied, a genuinely
  # transient API error, ...) is something a retry can't fix, so it's
  # reported immediately instead of spending the whole budget on it.
  if [[ "$NETWORK_CHECK_ERROR" != *"was not found"* \
        && "$NETWORK_CHECK_ERROR" != *"SERVICE_DISABLED"* \
        && "$NETWORK_CHECK_ERROR" != *"has not been used in project"* ]]; then
    break
  fi
  if [[ "$attempt" -lt "$NETWORK_CHECK_MAX_ATTEMPTS" ]]; then
    sleep "$NETWORK_CHECK_RETRY_SECS"
  fi
done
if [[ "$NETWORK_FOUND" != "true" ]]; then
  if [[ "$NETWORK_CHECK_ERROR" == *"was not found"* ]]; then
    err "No 'default' VPC network found in project ${PROJECT_ID}."
    err "Organizations with the compute.skipDefaultNetworkCreation org policy do not get one automatically, and this script does not create one on your behalf."
    err "See https://googlecloudplatform.github.io/scion/hosted/single-node/hub-setup-gce-hardened-org/ (docs-site/src/content/docs/hosted/single-node/hub-setup-gce-hardened-org.md in a checkout) for the one command needed to create it."
  else
    err "Could not verify the default VPC network in project ${PROJECT_ID}:"
    err "$NETWORK_CHECK_ERROR"
  fi
  exit 1
fi
echo "  Default VPC network found."

# --- Hybrid tier: discovery ---
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  info "Discovering GKE cluster network and node tag..."
  hybrid_discover "default"
  echo "  Node network tag: ${GKE_NODE_TAG}"

  # Everything about the hybrid tier's Kubernetes objects that doesn't
  # depend on the VM's own IP address -- including the marker-refusal
  # check for existing, differently-owned objects -- runs here,
  # alongside discovery and before the VM, NFS export or firewall rules
  # are created. See hybrid_k8s_preflight's own comment for exactly what
  # it does and doesn't cover; the remaining, IP-dependent part of this
  # check happens later, in Phase 4.
  info "Checking hybrid-tier Kubernetes object ownership..."
  hybrid_k8s_preflight "$HUB_NAME"
fi

# --- Cross-org IAP warning (best-effort; never blocks the deploy) ---
# IAP's default (Google-managed) OAuth client only covers same-organization
# use. A custom OAuth client is required when: the deployer is outside the
# project's organization, OR the project is not in a GCP organization at all
# (Google-managed clients don't support no-org projects, full stop -- see
# https://cloud.google.com/iap/docs/custom-oauth-configuration). The no-org
# case is the common first-deploy shape (personal account, OSS project), and
# unlike the cross-org case it is a *certain* answer, not a heuristic -- warn
# on it, don't skip it. See docs/deploy/agent-runbook-single-node-vm.md
# Section 7 (Troubleshooting) for what to do about either case.
#
# The domain comparison itself is a heuristic: it compares the deployer's
# email domain against the org's `displayName`, which the Resource Manager
# API documents as the organization's *primary* Workspace domain. A deployer
# on a secondary or alias domain of the same Workspace org, or a subdomain,
# is legitimately in-org but will not match `displayName` -- that's a known
# false-positive mode, not something this comparison can currently
# distinguish from an actual cross-org deployer. Any inability to *read* the
# data -- the get-ancestors call itself failing, or organizations describe
# failing on a project that does have an org -- degrades to "cannot
# determine, skip the check" rather than guessing; nothing in this block
# ever fails the script, and it only detects and warns -- it never creates
# or configures an OAuth client.
info "Checking for cross-organization IAP mismatch..."
# A successful get-ancestors call always returns at least the project's
# own row, so empty output means the call failed (permissions, etc.), not
# "no organization" -- capture the raw output before parsing so those two
# cases stay distinguishable. The no-org check below depends only on the
# project, not on who's deploying, so it runs for every identity, including
# service accounts.
ANCESTORS="$(gcloud projects get-ancestors "${PROJECT_ID}" \
  --format='value(id,type)' 2>/dev/null)" || ANCESTORS=""
if [[ -z "$ANCESTORS" ]]; then
  echo "  Could not read ancestry for ${PROJECT_ID}; skipping cross-org IAP check."
else
  ORG_ID="$(awk '$2=="organization"{print $1; exit}' <<<"$ANCESTORS")" || true
  if [[ -z "$ORG_ID" ]]; then
    warn "Project ${PROJECT_ID} is not in a GCP organization. IAP's Google-managed OAuth client does not support no-org projects -- a custom OAuth client is required."
    warn "This deploy will continue. See docs/deploy/agent-runbook-single-node-vm.md Section 7 (Troubleshooting, Cross-org IAP) for what to do."
  elif [[ "$ACCOUNT" == *.gserviceaccount.com ]]; then
    # An org was found, so the only remaining step is comparing the
    # deployer's email domain against it -- not a meaningful comparison for
    # a service account, so skip just that step.
    echo "  Deployer is a service account; skipping cross-org domain comparison."
  else
    ORG_DOMAIN="$(gcloud organizations describe "${ORG_ID}" \
      --format='value(displayName)' 2>/dev/null)" || true
    if [[ -z "$ORG_DOMAIN" ]]; then
      echo "  Could not read metadata for organization ${ORG_ID} (likely a permissions gap); skipping cross-org IAP check."
    else
      DEPLOYER_DOMAIN="${ACCOUNT##*@}"
      if [[ "$(echo "$ORG_DOMAIN" | tr '[:upper:]' '[:lower:]')" != "$(echo "$DEPLOYER_DOMAIN" | tr '[:upper:]' '[:lower:]')" ]]; then
        warn "Deployer account (${ACCOUNT}) does not appear to belong to project ${PROJECT_ID}'s organization (${ORG_DOMAIN})."
        warn "Cross-org IAP typically requires a custom OAuth consent screen / OAuth client -- the default consent screen will block authentication."
        warn "(This can also be a false positive if the deployer is on a secondary or alias domain of the same organization.)"
        warn "This deploy will continue. If IAP authentication fails later, or shows an unexpected consent screen, see"
        warn "docs/deploy/agent-runbook-single-node-vm.md Section 7 (Troubleshooting) for the cross-org IAP scenario."
      else
        echo "  Deployer domain matches organization domain (${ORG_DOMAIN}); no cross-org IAP concern detected."
      fi
    fi
  fi
fi

# --- Detect existing Cloud NAT to reuse (network default, region ${REGION}) ---
# A Cloud NAT gateway in ALL_SUBNETWORKS_* mode (all subnets, or all subnets'
# primary IP ranges) cannot coexist with any other NAT gateway on the same
# network+region -- GCP rejects creating one while another gateway is
# already there, regardless of what that other gateway covers. So there are
# two cases where our own `--nat-all-subnet-ip-ranges` NAT create in the
# Cloud Router + Cloud NAT step below would otherwise be rejected:
#   1. Some other router's NAT already covers our VM's subnet ("default") --
#      reuse it instead of creating our own.
#   2. Some other router has a NAT that does NOT cover "default" -- our own
#      NAT still can't use ALL_SUBNETWORKS_* mode alongside it, so it's
#      created scoped to just subnet "default" instead
#      (--nat-custom-subnet-ip-ranges), which coexists fine.
# Either way, by the time the plain `nats create` call failed, the service
# account, IAM bindings, and possibly an orphan router would already exist
# with no clean way to unwind them -- so this runs first, before any
# resource below is created. See docs/deploy/agent-runbook-single-node-vm.md
# (Cloud NAT reuse).
#
# This has to inspect every router's NAT config on this network+region, not
# just our own name -- idempotent re-runs that recognize OUR OWN router/NAT
# are still handled below, unchanged, by the existing describe-by-name
# checks. jq (checked as a Phase 1 prerequisite) is required: NAT coverage
# is nested JSON (sourceSubnetworkIpRangesToNat / subnetworks[]), and a
# text-based fallback risks silently misjudging reuse.
info "Checking for an existing Cloud NAT covering this network/region..."
ROUTER_NAME="scion-hub-${HUB_NAME}-router"
NAT_NAME="scion-hub-${HUB_NAME}-nat"

# No `2>/dev/null` here or on the jq call below: on failure, gcloud's/jq's
# own error (permission denied, API disabled, bad JSON, ...) prints to
# stderr right above our `err`, instead of being silently discarded.
ROUTERS_JSON="$(gcloud compute routers list \
  --project="${PROJECT_ID}" \
  --filter="region:(${REGION}) AND network:(default)" \
  --format=json)" || {
  err "Could not list Cloud Routers in ${REGION} on network 'default' (see gcloud error above). Aborting before creating any resources."
  exit 1
}
if [[ -z "$ROUTERS_JSON" ]]; then
  # A successful `list` always prints at least "[]"; empty output means
  # something went wrong upstream of gcloud's own exit code. Treat it as a
  # failure rather than silently matching "no routers" (fail closed).
  err "Cloud Router list returned no output for ${REGION} on network 'default' (expected at least '[]'). Aborting before creating any resources."
  exit 1
fi

# Emits one "<router><TAB><nat><TAB>covers<TAB>foreign" row per NAT, for
# routers that are actually on network "default" in exactly this region
# (re-checked here with endswith -- gcloud's `--filter` ':' operator is a
# word/substring match, not equality, so e.g. a router on network
# "default-vpc" would otherwise slip through). A NAT with type PRIVATE
# (Private NAT, for NCC/hybrid connectivity -- not internet egress) never
# counts as coverage: it can't provide the VM's internet egress no matter
# what it covers. It DOES still count as a foreign gateway, though --
# GCP's ALL_SUBNETWORKS exclusivity rule ("there should not be any other
# Router.Nat section in any Router for this network in this region") is
# not qualified by type, so an all-subnets create is rejected next to a
# foreign Private NAT exactly like next to any other foreign gateway.
# Excluding Private NATs from "foreign" as well as "covers" would let
# deploy.sh attempt that same rejected all-subnets create -- the #2003
# failure again, after the SA/IAM already exist. So a foreign Private NAT
# still routes to the scoped (--nat-custom-subnet-ip-ranges=default)
# create below, same as any other non-covering foreign gateway; our own
# router is never treated as foreign; a hand-added extra NAT on it is out
# of scope. "covers" is true when the NAT is PUBLIC (the
# default when `type` is absent) AND already provides egress for subnet
# "default": ALL_SUBNETWORKS_* mode (all ranges, or all primary ranges),
# or a LIST_OF_SUBNETWORKS entry for "default" whose sourceIpRangesToNat
# actually includes the primary range (ALL_IP_RANGES or PRIMARY_IP_RANGE
# -- an entry that only forwards secondary ranges does NOT give the VM's
# primary IP egress). "foreign" is true when the router isn't the one
# we'd create ourselves, regardless of NAT type.
# A here-string avoids piping through `echo`, which can misbehave on
# option-like leading hyphens in $ROUTERS_JSON (e.g. a value starting
# with "-n" or "-e").
NAT_ROWS="$(jq -r \
  --arg region "$REGION" \
  --arg own "$ROUTER_NAME" '
  .[]
  | select((.network // "") | endswith("/global/networks/default"))
  | select((.region // "") | endswith("/regions/" + $region))
  | . as $r
  | ($r.nats // [])[]
  | . as $n
  | (($n.type // "PUBLIC") == "PUBLIC") as $public
  | ( ($n.subnetworks // [])
      | any(
          (.name // "" | endswith("/subnetworks/default"))
          and ( ( .sourceIpRangesToNat // ["ALL_IP_RANGES"] )
                | any(. == "ALL_IP_RANGES" or . == "PRIMARY_IP_RANGE") )
        )
    ) as $listCovers
  | ( ($n.sourceSubnetworkIpRangesToNat // "") | startswith("ALL_SUBNETWORKS_") ) as $allCovers
  | [$r.name, $n.name, (($public and ($allCovers or $listCovers)) | tostring), (($r.name != $own) | tostring)]
  | @tsv
' <<< "$ROUTERS_JSON")" || {
  err "Could not parse Cloud Router/NAT config in ${REGION} (see jq error above). Aborting before creating any resources."
  exit 1
}

REUSE_ROUTER=""
REUSE_NAT=""
FOREIGN_NAT_EXISTS="false"
if [[ -n "$NAT_ROWS" ]]; then
  while IFS=$'\t' read -r RTR NAT COVERS FOREIGN; do
    [[ -z "$RTR" ]] && continue
    if [[ "$FOREIGN" == "true" ]]; then
      FOREIGN_NAT_EXISTS="true"
      if [[ -z "$REUSE_NAT" && "$COVERS" == "true" ]]; then
        REUSE_ROUTER="$RTR"
        REUSE_NAT="$NAT"
      fi
    fi
  done <<< "$NAT_ROWS"
fi

REUSE_EXISTING_NAT="false"
NAT_CREATE_MODE="all-subnets"
if [[ -n "$REUSE_NAT" ]]; then
  REUSE_EXISTING_NAT="true"
  ROUTER_NAME="$REUSE_ROUTER"
  NAT_NAME="$REUSE_NAT"
  info "Found an existing Cloud NAT that already covers this network/region; reusing it instead of creating our own."
  echo "  Reusing Cloud Router: ${ROUTER_NAME}"
  echo "  Reusing Cloud NAT:    ${NAT_NAME}"
elif [[ "$FOREIGN_NAT_EXISTS" == "true" ]]; then
  NAT_CREATE_MODE="custom-default-subnet"
  info "A Cloud NAT gateway already exists on this network/region but doesn't provide internet egress for subnet 'default'; scoping our own NAT to that subnet only (an all-subnets NAT can't coexist with another gateway)."
fi

# --- Service account ---
info "Creating service account (if needed)..."
if gcloud iam service-accounts describe "${SA_EMAIL}" \
    --project="${PROJECT_ID}" &>/dev/null; then
  echo "  Service account already exists: ${SA_EMAIL}"
else
  gcloud iam service-accounts create "${SA_NAME}" \
    --display-name="Scion Hub VM (${HUB_NAME})" \
    --description="scion-deployment=${HUB_NAME}" \
    --project="${PROJECT_ID}"
  echo "  Created service account: ${SA_EMAIL}"
fi

# --- Hybrid tier: agent transport auth setup ---
# GKE agent pods reach the hub through its public IAP URL, authenticating
# the transport hop with a Google OIDC ID token minted by impersonating a
# dedicated service account -- gated on the hybrid tier, not on IAP being
# on in general, because only GKE-dispatched agents ever leave the hub VM
# to reach it; a Docker-dispatched agent on the VM itself never traverses
# IAP. Runs here, right after the hub's own runtime SA exists (needed for
# the grant below), and after the cross-organization IAP check above:
# both the client-ID discovery and that check depend on IAP already
# being configured for this project. The Cloud Run resource-level grant
# (hybrid_grant_transport_sa_iap_access) happens later, in Phase 4,
# since the Cloud Run service and its own IAP enablement don't exist
# yet at this point.
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  info "Setting up agent transport auth..."
  hybrid_discover_iap_client_id "${PROJECT_ID}"
  hybrid_ensure_transport_sa "${HUB_NAME}" "${PROJECT_ID}"
  hybrid_grant_transport_token_creator "${HYBRID_TRANSPORT_SA_EMAIL}" "${SA_EMAIL}" "${PROJECT_ID}"
  # Rendered once, here, and spliced into both settings.yaml writes below
  # (dev mode in Phase 3, proxy mode in Phase 5), the same pattern
  # HYBRID_SHARED_DIR_STORAGE_YAML uses. IAM changes (the grants above,
  # and the Cloud Run accessor grant in Phase 4) can take on the order of
  # a minute to propagate; the first agent dispatched immediately after
  # this deploy finishes may see a transient 403 minting or using its
  # transport token. deploy.sh has nothing that blocks on this
  # propagation itself -- it never mints or uses a transport token -- so
  # it's documented here and in the runbook rather than covered with a
  # blind sleep.
  HYBRID_AUTH_TRANSPORT_YAML="$(hybrid_settings_auth_transport_yaml "${HYBRID_IAP_CLIENT_ID}" "${HYBRID_TRANSPORT_SA_EMAIL}")"
else
  HYBRID_AUTH_TRANSPORT_YAML=""
fi

# Bind minimal IAM roles (idempotent)
# artifactregistry.writer lets the VM build and push the Cloud Run IAP proxy
# image directly to Artifact Registry (see Phase 4).
info "Binding IAM roles..."
for ROLE in roles/logging.logWriter roles/monitoring.metricWriter roles/cloudtrace.agent roles/artifactregistry.writer roles/aiplatform.user; do
  if ! BIND_ERR="$(gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member="serviceAccount:${SA_EMAIL}" \
    --role="${ROLE}" \
    --condition=None \
    --quiet 2>&1 >/dev/null)"; then
    err "Failed to grant ${ROLE} to ${SA_EMAIL} on project ${PROJECT_ID}:"
    print_gcloud_error "$BIND_ERR"
    exit 1
  fi
done
echo "  Roles bound: logging.logWriter, monitoring.metricWriter, cloudtrace.agent, artifactregistry.writer, aiplatform.user"

# --- Proxy service account ---
# A separate, minimally-privileged identity for the Cloud Run IAP proxy
# (see Phase 4). It never calls a GCP API -- it only reverse-proxies HTTP
# to the hub VM's internal IP -- so, unlike SA_EMAIL above, it gets no
# project IAM roles at all. In organizations where the default Compute
# Engine service account is disabled, deploying the proxy with no
# --service-account would otherwise fall back to that disabled account
# and fail; this SA is what Phase 4 passes via --service-account instead.
#
# Deploying a Cloud Run service with --service-account=X requires the
# deployer to already hold iam.serviceAccounts.actAs on X -- the same
# requirement SA_EMAIL above has always had for the VM create in this
# script, which has never granted it automatically either. Owner and
# Editor both include this permission; a more narrowly-scoped deployer
# needs roles/iam.serviceAccountUser on the SA. See the hardened-org
# addendum doc.
info "Creating proxy service account (if needed)..."
if gcloud iam service-accounts describe "${PROXY_SA_EMAIL}" \
    --project="${PROJECT_ID}" &>/dev/null; then
  echo "  Proxy service account already exists: ${PROXY_SA_EMAIL}"
else
  gcloud iam service-accounts create "${PROXY_SA_NAME}" \
    --display-name="Scion Hub IAP Proxy (${HUB_NAME})" \
    --description="scion-deployment=${HUB_NAME}" \
    --project="${PROJECT_ID}"
  echo "  Created proxy service account: ${PROXY_SA_EMAIL}"
fi

# --- Grant deployer IAP tunnel access (required for SSH to --no-address VMs) ---
info "Granting IAP tunnel access to deployer..."
DEPLOYER_EMAIL="$(gcloud auth list --filter=status:ACTIVE --format='value(account)' 2>/dev/null | head -1)" || true
if [[ -z "$DEPLOYER_EMAIL" ]]; then
  DEPLOYER_EMAIL="$(gcloud config get-value account 2>/dev/null)" || true
fi
if [[ -n "$DEPLOYER_EMAIL" ]]; then
  if [[ "$DEPLOYER_EMAIL" == *.gserviceaccount.com ]]; then
    DEPLOYER_MEMBER="serviceAccount:${DEPLOYER_EMAIL}"
  else
    DEPLOYER_MEMBER="user:${DEPLOYER_EMAIL}"
  fi
  if TUNNEL_BIND_ERR="$(gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member="${DEPLOYER_MEMBER}" \
    --role="roles/iap.tunnelResourceAccessor" \
    --condition=None \
    --quiet 2>&1 >/dev/null)"; then
    echo "  IAP tunnel access granted to: ${DEPLOYER_EMAIL}"
  else
    warn "Failed to grant roles/iap.tunnelResourceAccessor to ${DEPLOYER_EMAIL}:"
    print_gcloud_error "$TUNNEL_BIND_ERR"
    warn "SSH to the VM may fail if you do not have this role. Please ensure it is granted manually."
  fi
else
  warn "Could not determine deployer identity; skipping IAP tunnel role grant."
  warn "SSH to the VM may fail. Grant roles/iap.tunnelResourceAccessor manually."
fi

# --- Cloud Router + Cloud NAT ---
# The VM has no public IP (--no-address).  Cloud NAT gives it outbound internet
# access so cloud-init can install packages, download binaries, and pull images.
# ROUTER_NAME/NAT_NAME were set above during the reuse check -- either our
# own names, or an existing router/NAT we're reusing (REUSE_EXISTING_NAT).
if [[ "$REUSE_EXISTING_NAT" == "true" ]]; then
  echo "  Skipping Cloud Router create: reusing ${ROUTER_NAME}"
  echo "  Skipping Cloud NAT create: reusing ${NAT_NAME}"
else
  info "Creating Cloud Router (if needed)..."
  if gcloud compute routers describe "${ROUTER_NAME}" \
      --region="${REGION}" --project="${PROJECT_ID}" &>/dev/null; then
    echo "  Cloud Router already exists: ${ROUTER_NAME}"
  else
    gcloud compute routers create "${ROUTER_NAME}" \
      --region="${REGION}" \
      --project="${PROJECT_ID}" \
      --network=default \
      --description="scion-deployment=${HUB_NAME}" \
      --quiet
    echo "  Created Cloud Router: ${ROUTER_NAME}"
  fi

  info "Creating Cloud NAT (if needed)..."
  if gcloud compute routers nats describe "${NAT_NAME}" \
      --router="${ROUTER_NAME}" \
      --region="${REGION}" --project="${PROJECT_ID}" &>/dev/null; then
    echo "  Cloud NAT already exists: ${NAT_NAME}"
  elif [[ "$NAT_CREATE_MODE" == "custom-default-subnet" ]]; then
    # A foreign NAT gateway exists on this network+region but doesn't cover
    # us (see the reuse check above) -- ALL_SUBNETWORKS_* mode can't
    # coexist with it, so scope ours to just subnet "default".
    gcloud compute routers nats create "${NAT_NAME}" \
      --router="${ROUTER_NAME}" \
      --region="${REGION}" \
      --project="${PROJECT_ID}" \
      --auto-allocate-nat-external-ips \
      --nat-custom-subnet-ip-ranges=default \
      --quiet
    echo "  Created Cloud NAT: ${NAT_NAME} (scoped to subnet 'default')"
  else
    gcloud compute routers nats create "${NAT_NAME}" \
      --router="${ROUTER_NAME}" \
      --region="${REGION}" \
      --project="${PROJECT_ID}" \
      --auto-allocate-nat-external-ips \
      --nat-all-subnet-ip-ranges \
      --quiet
    echo "  Created Cloud NAT: ${NAT_NAME}"
  fi
fi

# --- Hub VM network tag ---
# The IAP SSH firewall rule below is scoped to this tag so it only ever
# grants SSH to the hub VM, not to every VM on the network. On a re-run
# where the VM already exists (e.g. from a deploy predating this tag), make
# sure the tag is applied *before* the firewall rule is narrowed to target
# it, so there is never a window where the rule targets a tag the VM lacks.
# HUB_TAG_CONFIRMED only flips to true once add-tags has actually succeeded
# against a VM this run found. Narrowing an existing unscoped rule below is
# gated on that, not merely on "describe didn't find a VM" -- a zone
# mismatch or a transient describe error looks identical to "no VM" here,
# and narrowing in that case could lock out a VM that still lacks the tag.
HUB_TAG_CONFIRMED=false
if gcloud compute instances describe "${INSTANCE_NAME}" \
    --zone="${ZONE}" --project="${PROJECT_ID}" &>/dev/null; then
  info "Ensuring hub VM has network tag..."
  if gcloud compute instances add-tags "${INSTANCE_NAME}" \
      --zone="${ZONE}" \
      --project="${PROJECT_ID}" \
      --tags="${HUB_TAG}" \
      --quiet; then
    echo "  Ensured network tag on VM ${INSTANCE_NAME}: ${HUB_TAG}"
    HUB_TAG_CONFIRMED=true
  else
    warn "Could not add network tag ${HUB_TAG} to VM ${INSTANCE_NAME}; will not narrow an existing unscoped firewall rule this run."
  fi
fi

# --- IAP SSH firewall rule ---
# gcloud compute ssh via IAP tunneling requires TCP:22 from 35.235.240.0/20.
# Scoped with --target-tags so it only applies to the hub VM, not every VM
# on network default.
FW_RULE_NAME="scion-hub-${HUB_NAME}-allow-iap-ssh"
info "Creating IAP SSH firewall rule (if needed)..."
if gcloud compute firewall-rules describe "${FW_RULE_NAME}" \
    --project="${PROJECT_ID}" &>/dev/null; then
  # A failed read here is not the same as "no target tags": treat it as
  # unknown, not unscoped. Folding a describe failure into an empty
  # EXISTING_TARGET_TAGS (via `|| true`) would make a transient error look
  # exactly like an unscoped rule, and -- if HUB_TAG_CONFIRMED is already
  # true -- narrow the rule from under any foreign target tags it actually
  # carries, which is the one thing the elif branch below exists to avoid.
  if EXISTING_TARGET_TAGS="$(gcloud compute firewall-rules describe "${FW_RULE_NAME}" \
      --project="${PROJECT_ID}" --format="value(targetTags)" 2>/dev/null)"; then
    if [[ -z "$EXISTING_TARGET_TAGS" ]]; then
      if [[ "$HUB_TAG_CONFIRMED" == "true" ]]; then
        warn "Firewall rule ${FW_RULE_NAME} has no target tags (pre-existing, unscoped rule); narrowing to ${HUB_TAG}. Any other VM that relied on this rule for IAP SSH loses that access -- give it its own rule if it still needs one."
        gcloud compute firewall-rules update "${FW_RULE_NAME}" \
          --project="${PROJECT_ID}" \
          --target-tags="${HUB_TAG}" \
          --quiet
        echo "  Updated firewall rule ${FW_RULE_NAME} with --target-tags=${HUB_TAG}"
      else
        warn "Firewall rule ${FW_RULE_NAME} has no target tags, but the hub VM's tag could not be confirmed this run. Leaving it unscoped rather than risk locking out SSH; it will be narrowed once the tag is confirmed on a later run."
      fi
    # Exact whole-tag match against one line of the split list, not a
    # substring/word match against the raw string: `grep -w` would treat
    # "-" as a non-word character and falsely match HUB_TAG against a
    # hyphen-extended tag like "${HUB_TAG}-nfs". Bash parameter expansion
    # does the split (replacing every "," or ";" with a newline) so grep
    # is the only external process spawned, rather than piping through
    # printf and two tr calls.
    elif ! grep -qxF -- "${HUB_TAG}" <<< "${EXISTING_TARGET_TAGS//[;,]/$'\n'}"; then
      warn "Firewall rule ${FW_RULE_NAME} exists but its target tags (${EXISTING_TARGET_TAGS}) do not include ${HUB_TAG}. IAP SSH to the hub VM may fail; add ${HUB_TAG} to the rule manually or delete it and re-run."
    else
      echo "  Firewall rule already exists: ${FW_RULE_NAME} (target tags: ${EXISTING_TARGET_TAGS})"
    fi
  else
    warn "Could not read target tags for firewall rule ${FW_RULE_NAME} (it exists, but the read failed); leaving it as-is rather than risk narrowing it while its target tags are unknown. Re-run once the read succeeds."
  fi
else
  gcloud compute firewall-rules create "${FW_RULE_NAME}" \
    --project="${PROJECT_ID}" \
    --network=default \
    --direction=INGRESS \
    --action=ALLOW \
    --rules=tcp:22 \
    --source-ranges=35.235.240.0/20 \
    --target-tags="${HUB_TAG}" \
    --description="Allow SSH via IAP tunneling for Scion Hub | scion-deployment=${HUB_NAME}" \
    --quiet
  echo "  Created firewall rule: ${FW_RULE_NAME} (target tags: ${HUB_TAG})"
fi

# --- Proxy-to-VM firewall rule (tcp:8080) ---
# The Cloud Run proxy reaches the hub VM's internal IP on :8080 via Direct
# VPC egress (--vpc-egress=all-traffic in Phase 4), which sources traffic
# from the region's "default" subnet range. Rather than depend on the
# network's own default-allow-internal rule -- broad (all protocols, all
# of 10.128.0.0/9, every VM on the network) and not guaranteed to exist at
# all in a hardened org (creating the default network via CLI does not
# create it) -- this scopes a dedicated rule to exactly what's needed:
# tcp:8080, from just the default subnet's own range, to just the hub VM.
info "Looking up the default subnet's IP range (${REGION})..."
DEFAULT_SUBNET_CIDR="$(gcloud compute networks subnets describe default \
  --region="${REGION}" --project="${PROJECT_ID}" \
  --format="value(ipCidrRange)" 2>/dev/null)" || true
if [[ -z "$DEFAULT_SUBNET_CIDR" ]]; then
  err "Could not determine the IP range of the 'default' subnet in ${REGION}."
  err "An auto-mode default network creates one subnet per region automatically; a custom-mode network needs one created explicitly. See https://googlecloudplatform.github.io/scion/hosted/single-node/hub-setup-gce-hardened-org/ (docs-site/src/content/docs/hosted/single-node/hub-setup-gce-hardened-org.md in a checkout) for both."
  exit 1
fi
echo "  Default subnet CIDR (${REGION}): ${DEFAULT_SUBNET_CIDR}"

FW_8080_RULE_NAME="scion-hub-${HUB_NAME}-allow-proxy"
info "Creating proxy-to-VM firewall rule (if needed)..."
# A single combined describe covers both the existence check and all three
# drift fields, instead of four separate gcloud calls. gcloud's value()
# printer joins multiple requested fields with a single tab, and joins a
# repeated field's own values (sourceRanges, targetTags) with ";" --
# verified by rendering synthetic firewall-rule resources through the
# installed Cloud SDK's own csv_printer.ValuePrinter, including the empty-
# field case (an empty or absent field renders as nothing between the
# tabs on either side of it, never omitted or reflowed). Split with plain
# parameter expansion, not `read -d $'\t'`/`IFS=$'\t' read`: bash's `read`
# classifies tab as "IFS whitespace" and silently collapses a leading
# empty field, or adjacent tabs around an empty middle field (verified by
# hand), which would shift every field after it instead of just leaving
# one blank. A trailing empty field alone splits fine.
if FW_8080_DESCRIBE="$(gcloud compute firewall-rules describe "${FW_8080_RULE_NAME}" \
    --project="${PROJECT_ID}" \
    --format="value(sourceRanges,allowed[].map().firewall_rule().list(),targetTags)" \
    2>/dev/null)"; then
  EXISTING_8080_SOURCE="${FW_8080_DESCRIBE%%$'\t'*}"
  FW_8080_REST="${FW_8080_DESCRIBE#*$'\t'}"
  EXISTING_8080_ALLOWED="${FW_8080_REST%%$'\t'*}"
  EXISTING_8080_TARGET_TAGS="${FW_8080_REST#*$'\t'}"
  unset FW_8080_DESCRIBE FW_8080_REST
  if [[ "$EXISTING_8080_SOURCE" == "$DEFAULT_SUBNET_CIDR" \
        && "$EXISTING_8080_ALLOWED" == "tcp:8080" \
        && "$EXISTING_8080_TARGET_TAGS" == "$HUB_TAG" ]]; then
    echo "  Firewall rule already exists: ${FW_8080_RULE_NAME} (source: ${EXISTING_8080_SOURCE})"
  else
    warn "Firewall rule ${FW_8080_RULE_NAME} exists but drifted from what this script expects (source: ${EXISTING_8080_SOURCE}, allowed: ${EXISTING_8080_ALLOWED}, target tags: ${EXISTING_8080_TARGET_TAGS}; expected source: ${DEFAULT_SUBNET_CIDR}, allowed: tcp:8080, target tags: ${HUB_TAG}). Not auto-updating it -- delete the rule and re-run, or update it manually, if this is unintentional."
  fi
else
  gcloud compute firewall-rules create "${FW_8080_RULE_NAME}" \
    --project="${PROJECT_ID}" \
    --network=default \
    --direction=INGRESS \
    --action=ALLOW \
    --rules=tcp:8080 \
    --source-ranges="${DEFAULT_SUBNET_CIDR}" \
    --target-tags="${HUB_TAG}" \
    --description="Allow the Cloud Run IAP proxy (Direct VPC egress) to reach the Scion Hub VM on 8080 | scion-deployment=${HUB_NAME}" \
    --quiet
  echo "  Created firewall rule: ${FW_8080_RULE_NAME} (source: ${DEFAULT_SUBNET_CIDR}, target tags: ${HUB_TAG})"
fi

# --- Hybrid tier: firewall rules ---
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  info "Creating hybrid-tier firewall rules (if needed)..."
  hybrid_ensure_firewall_rules "$HUB_NAME" "$PROJECT_ID" "default" "$REGION"
fi

# --- Create VM ---
# One --tags flag carries every network tag the VM is created with: the
# hub tag the IAP SSH rule targets and, with the tier on, the hybrid-tier
# tag the NFS rules target.
VM_CREATE_TAGS="${HUB_TAG}"
VM_EXTRA_CREATE_ARGS=()
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  VM_CREATE_TAGS="${VM_CREATE_TAGS},$(hybrid_vm_tag "$HUB_NAME")"
fi

info "Creating GCE VM (if needed)..."
if gcloud compute instances describe "${INSTANCE_NAME}" \
    --zone="${ZONE}" --project="${PROJECT_ID}" &>/dev/null; then
  echo "  VM already exists: ${INSTANCE_NAME}"
  if [[ "$HYBRID_ENABLED" == "true" ]]; then
    info "Ensuring hybrid-tier network tag on existing VM..."
    hybrid_apply_vm_tag "${INSTANCE_NAME}" "${ZONE}" "${PROJECT_ID}" "${HUB_NAME}"
    EXISTING_VM_IP="$(gcloud compute instances describe "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --format="get(networkInterfaces[0].networkIP)")"
    hybrid_ensure_internal_ip_existing_vm "${HUB_NAME}" "${PROJECT_ID}" "${REGION}" "default" \
      "${EXISTING_VM_IP}" "${INSTANCE_NAME}" "${ZONE}"
  fi
else
  if [[ "$HYBRID_ENABLED" == "true" ]]; then
    hybrid_ensure_internal_ip_new_vm "${HUB_NAME}" "${PROJECT_ID}" "${REGION}" "default"
    VM_EXTRA_CREATE_ARGS+=(--private-network-ip="${HYBRID_INTERNAL_IP}")
  fi
  # --subnet=default is required, not cosmetic, for a custom-mode
  # "default" network: the Compute API requires an explicit subnetwork
  # for custom-mode networks and only makes it optional for auto-mode
  # ones, so without this flag the create fails here -- after the SAs,
  # IAM bindings, NAT and both firewall rules already exist -- on any
  # project whose "default" network happens to be custom-mode (a
  # hand-built network, a platform-team baseline, or an org that blocks
  # auto-mode networks entirely; see the hardened-org addendum). In auto
  # mode the per-region subnet is also named "default", so this changes
  # nothing there. --network=default is passed alongside it: subnet names
  # are unique per project and region regardless of which network owns
  # them (a project can never have two subnets both named "default" in
  # REGION), so --subnet=default alone can't be ambiguous, but it also
  # doesn't check which network it resolves to. Per `gcloud compute
  # instances create --help`, when both flags are given "subnet must be a
  # subnetwork of the network specified by [--network]", so pairing them
  # makes a misconfigured project (a "default" subnet in REGION that
  # belongs to some other VPC) fail the create with a clear error instead
  # of silently landing the VM in that other VPC's subnet. This also
  # makes the VM create consistent with the Cloud Run Direct VPC egress
  # deploy below (which already passes both flags), Cloud NAT and the
  # CIDR lookup above, which already all assume a subnet named "default"
  # in REGION owned by the network named "default".
  gcloud compute instances create "${INSTANCE_NAME}" \
    --zone="${ZONE}" \
    --project="${PROJECT_ID}" \
    --network=default \
    --subnet=default \
    --machine-type="${MACHINE_TYPE}" \
    --no-address \
    --service-account="${SA_EMAIL}" \
    --scopes=cloud-platform \
    --tags="${VM_CREATE_TAGS}" \
    --boot-disk-size="${DISK_SIZE}" \
    --image-family=ubuntu-2204-lts \
    --image-project=ubuntu-os-cloud \
    --shielded-secure-boot \
    --metadata-from-file=user-data="${SCRIPT_DIR}/cloud-init.yaml" \
    --labels="scion-deployment=${HUB_NAME}" \
    ${VM_EXTRA_CREATE_ARGS[@]+"${VM_EXTRA_CREATE_ARGS[@]}"} \
    --quiet
  echo "  Created VM: ${INSTANCE_NAME} (zone: ${ZONE})"
fi

# --- Internal IP guard (post-create half) ---
# The shared NFS PV's server field needs the reserved internal IP to
# actually match the VM. See hybrid_internal_ip_guard_verify's own
# comment for what this does and doesn't cover.
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  hybrid_internal_ip_guard_verify "$HUB_NAME" "$PROJECT_ID" "$REGION" "$INSTANCE_NAME" "$ZONE"
fi

# --- Wait for SSH readiness (avoids race on initial boot) ---
info "Waiting for SSH access to VM..."
SSH_READY=false
SSH_STDERR_FILE="$(mktemp)"
for attempt in $(seq 1 "$SSH_MAX_ATTEMPTS"); do
  if gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="echo ssh-ok" \
      --ssh-flag="-o ConnectTimeout=5" \
      --quiet 2>"${SSH_STDERR_FILE}"; then
    SSH_READY=true
    break
  fi
  BACKOFF=$((attempt < SSH_BACKOFF_FAST_ATTEMPTS ? SSH_BACKOFF_FAST_SECS : SSH_BACKOFF_SLOW_SECS))
  echo "  SSH attempt ${attempt}/${SSH_MAX_ATTEMPTS} - retrying in ${BACKOFF}s..."
  sleep "$BACKOFF"
done

if [[ "$SSH_READY" != "true" ]]; then
  err "Could not establish SSH connection to ${INSTANCE_NAME} after ${SSH_MAX_ATTEMPTS} attempts."
  if [[ -s "${SSH_STDERR_FILE}" ]]; then
    echo "  Last SSH error:" >&2
    cat "${SSH_STDERR_FILE}" >&2
  fi
  rm -f "${SSH_STDERR_FILE}"
  exit 1
fi
rm -f "${SSH_STDERR_FILE}"
echo "  SSH connection established."

# --- Wait for cloud-init ---
# cloud-init installs Docker and creates the scion user. The script cannot
# proceed until this finishes — writing to directories cloud-init owns before
# it completes causes "No such file or directory" errors.
info "Waiting for cloud-init to complete (this may take a few minutes)..."
CLOUD_INIT_OK=false
for ci_attempt in $(seq 1 "$CLOUD_INIT_MAX_ATTEMPTS"); do
  if gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="sudo cloud-init status --wait" \
      2>/dev/null; then
    CLOUD_INIT_OK=true
    break
  fi
  if [[ $ci_attempt -lt $CLOUD_INIT_MAX_ATTEMPTS ]]; then
    echo "  cloud-init check attempt ${ci_attempt}/${CLOUD_INIT_MAX_ATTEMPTS} returned non-zero, retrying in ${CLOUD_INIT_RETRY_SECS}s..."
    sleep "$CLOUD_INIT_RETRY_SECS"
  fi
done
if [[ "$CLOUD_INIT_OK" != "true" ]]; then
  err "cloud-init did not complete successfully after ${CLOUD_INIT_MAX_ATTEMPTS} attempts."
  echo "  Check cloud-init logs: gcloud compute ssh ${INSTANCE_NAME} --zone=${ZONE} --project=${PROJECT_ID} --command='sudo cloud-init status --long'"
  exit 1
fi
echo "  Cloud-init completed."

# --- Get VM internal IP ---
info "Getting VM internal IP..."
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  VM_IP="$HYBRID_INTERNAL_IP"
else
  VM_IP="$(gcloud compute instances describe "${INSTANCE_NAME}" \
    --zone="${ZONE}" --project="${PROJECT_ID}" \
    --format="get(networkInterfaces[0].networkIP)")"
fi
if [[ -z "$VM_IP" ]]; then
  err "Could not retrieve VM internal IP for ${INSTANCE_NAME}"
  exit 1
fi
echo "  VM internal IP: ${VM_IP}"

# --- Hybrid tier: settings.yaml default GCP identity mode ---
# Spliced into both settings.yaml writes below, where it stands for the
# default_gcp_identity_mode comment and key, and ends with a newline.
# Empty when the tier is off, so both writes carry the comment and key
# written in the heredocs.
HYBRID_GCP_IDENTITY_YAML=""
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  HYBRID_GCP_IDENTITY_YAML='# Hub-wide default GCP identity mode for new agents (V1Settings.DefaultGCPIdentityMode
# in pkg/config, a top-level settings.yaml key, not nested under agent_defaults).
# With the hybrid tier enabled, the hub default GCP identity mode is block.
# Configure a project-level mode ("assign" with a dedicated service account)
# for projects that need GCP access; see docs/deploy/hybrid-tier.md.
default_gcp_identity_mode: block'$'\n'
fi

# --- Hybrid tier: settings.yaml shared_dir_storage block ---
# Rendered once, here, and spliced into both settings.yaml writes below
# (dev mode in Phase 3, proxy mode in Phase 5) so they stay in sync.
# Empty when the tier is off, so both writes render byte-identical to
# before this existed.
HYBRID_SHARED_DIR_STORAGE_YAML=""
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  # The third argument is a PersistentVolumeClaim name, not the
  # PersistentVolume's own name: the Go side consumes shares[0].pv_name
  # as the pod spec's claimName (see pkg/runtime/k8s_runtime.go), so
  # GKE_PVC_NAME (the resolved PVC name, which may differ from the PV's
  # name) is what belongs here.
  HYBRID_SHARED_DIR_STORAGE_YAML="$(hybrid_settings_shared_dir_storage_yaml \
    "$VM_IP" "$HYBRID_NFS_EXPORT_ROOT" "$GKE_PVC_NAME")"
fi

# --- Hybrid tier: NFS squash identity, server, and export ---
# A tier-gated remote step, run here (once cloud-init has created the
# "scion" user and group the squash identity's primary group and export
# ownership depend on) rather than added to cloud-init.yaml itself, which
# is shared with every deployment: this keeps the tier-off path
# byte-for-byte unchanged. Always redone on every re-run, tier on: it's
# cheap, and it's how the export stays in sync if the cluster's node
# subnet CIDR ever changes.
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  info "Creating the NFS squash identity (if needed)..."
  SQUASH_SSH_ERR="$(mktemp)"
  if ! SQUASH_IDS=$(gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="$(hybrid_nfs_squash_identity_script "$HYBRID_NFS_SQUASH_USER")" \
      2>"${SQUASH_SSH_ERR}"); then
    err "Could not create or validate the NFS squash identity on ${INSTANCE_NAME}:"
    err "  $(cat "${SQUASH_SSH_ERR}")"
    rm -f "${SQUASH_SSH_ERR}"
    exit 1
  fi
  rm -f "${SQUASH_SSH_ERR}"
  # The remote script only ever prints this one line on success (see its
  # own comment), but validate it here too rather than trust stdout
  # blindly: a uid/gid pair is about to be embedded directly into the
  # NFS export line's anonuid=/anongid=, and squashing to uid 0 would
  # defeat the entire point of a dedicated, unprivileged squash identity.
  if [[ ! "$SQUASH_IDS" =~ ^[0-9]+:[0-9]+$ ]]; then
    err "Unexpected output from the NFS squash identity script on ${INSTANCE_NAME}: '${SQUASH_IDS}' (expected UID:GID)."
    exit 1
  fi
  SQUASH_UID="${SQUASH_IDS%%:*}"
  SQUASH_GID="${SQUASH_IDS##*:}"
  if [[ "$SQUASH_UID" -eq 0 ]]; then
    err "The NFS squash uid resolved to 0 on ${INSTANCE_NAME}; refusing to export with root as the anonymous uid."
    exit 1
  fi
  echo "  Squash uid: ${SQUASH_UID} (scion group gid: ${SQUASH_GID})"

  info "Installing the NFS server and export (if needed)..."
  NFS_FSID="$(hybrid_nfs_fsid "$HUB_NAME")"
  gcloud compute ssh "${INSTANCE_NAME}" \
    --zone="${ZONE}" --project="${PROJECT_ID}" \
    --command="$(hybrid_nfs_export_script "$HYBRID_NFS_EXPORT_ROOT" "$GKE_NODE_SUBNET_CIDR" \
      "$SQUASH_UID" "$SQUASH_GID" "$NFS_FSID" "$HUB_NAME" \
      "$HYBRID_NFS_IMAGE_PATH" "$HYBRID_SHARED_DIR_IMAGE_SIZE_GB")"
fi

# ===================================================================
# Phase 3: VM Setup
# ===================================================================
section "Phase 3: VM Setup"

# RELEASE_URL and ALLOW_UNVERIFIED_RELEASE are set by the checksum preflight
# in Phase 1, before any GCP resource was created.

# --- Detect VM architecture ---
info "Detecting VM architecture..."
ARCH=$(gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="uname -m" 2>/dev/null)
case "$ARCH" in
  x86_64)  ARCH_SUFFIX="amd64" ;;
  aarch64) ARCH_SUFFIX="arm64" ;;
  *)       ARCH_SUFFIX="amd64" ;;  # default to amd64
esac
echo "  Architecture: ${ARCH} (${ARCH_SUFFIX})"

# --- Stop scion-hub.service before binary update (avoids ETXTBSY on re-run) ---
info "Stopping scion-hub.service (if running)..."
gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="
    if systemctl is-active --quiet scion-hub.service 2>/dev/null; then
      sudo systemctl stop scion-hub.service
      echo 'Stopped scion-hub.service before binary update.'
    else
      echo 'scion-hub.service not running (first install or already stopped).'
    fi
  " 2>/dev/null || true

# --- Download and install scion binary ---
# The release also publishes a SHA256SUMS checksums file (see
# .github/workflows/build-release.yml). Download it alongside the binary and
# verify with sha256sum -c before extracting. A missing tarball or a hash
# mismatch always aborts, with no override. A missing checksums file or a
# missing entry for this asset also aborts by default, unless
# ALLOW_UNVERIFIED_RELEASE=true (set and warned about by the Phase 1
# preflight) explicitly opts out for a release published before checksums
# existed. The match is anchored and the archive name's dots are escaped so
# an unrelated entry (e.g. a suffixed "...tar.gz.old" line) cannot be
# mistaken for this asset's checksum. The single space between the hash and
# the "[ *]" class accepts either sha256sum output mode: two spaces in text
# mode (what build-release.yml's `sha256sum --` produces), or a space and an
# asterisk in binary mode.
SCION_ARCHIVE="scion-linux-${ARCH_SUFFIX}.tar.gz"
SCION_ARCHIVE_RE="${SCION_ARCHIVE//./\\.}"
info "Installing scion binary (${VERSION})..."
gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="
    set -euo pipefail
    echo 'Downloading scion binary...'
    curl -fsSL '${RELEASE_URL}/${SCION_ARCHIVE}' -o /tmp/${SCION_ARCHIVE}
    echo 'Downloading release checksums...'
    if curl -fsSL '${RELEASE_URL}/SHA256SUMS' -o /tmp/SHA256SUMS; then
      if grep -qE '^[0-9a-f]{64} [ *]${SCION_ARCHIVE_RE}\$' /tmp/SHA256SUMS; then
        echo 'Verifying checksum...'
        (cd /tmp && grep -E '^[0-9a-f]{64} [ *]${SCION_ARCHIVE_RE}\$' SHA256SUMS | sha256sum -c -)
      elif [ '${ALLOW_UNVERIFIED_RELEASE}' = 'true' ]; then
        echo 'WARNING: no checksum entry for ${SCION_ARCHIVE} in SHA256SUMS; installing UNVERIFIED (ALLOW_UNVERIFIED_RELEASE=true).' >&2
      else
        echo 'ERROR: no checksum entry for ${SCION_ARCHIVE} in SHA256SUMS -- refusing to install.' >&2
        exit 1
      fi
    elif [ '${ALLOW_UNVERIFIED_RELEASE}' = 'true' ]; then
      echo 'WARNING: could not download SHA256SUMS; installing UNVERIFIED (ALLOW_UNVERIFIED_RELEASE=true).' >&2
    else
      echo 'ERROR: could not download SHA256SUMS for ${VERSION} -- refusing to install.' >&2
      exit 1
    fi
    tar -xzf /tmp/${SCION_ARCHIVE} -C /tmp
    sudo mv /tmp/scion /usr/local/bin/scion
    sudo chmod +x /usr/local/bin/scion
    rm -f /tmp/${SCION_ARCHIVE} /tmp/SHA256SUMS
    echo \"Installed scion binary (${VERSION})\"
  "

# --- Download and install chat plugins ---
if [[ ${#CHAT_PLUGINS[@]} -gt 0 ]]; then
  info "Installing chat plugins..."
  for PLUGIN in "${CHAT_PLUGINS[@]}"; do
    PLUGIN_BINARY="scion-plugin-${PLUGIN}"
    PLUGIN_ARCHIVE="${PLUGIN_BINARY}-linux-${ARCH_SUFFIX}.tar.gz"
    PLUGIN_ARCHIVE_RE="${PLUGIN_ARCHIVE//./\\.}"
    info "  Installing ${PLUGIN_BINARY}..."
    gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="
        set -euo pipefail
        sudo -u scion mkdir -p /home/scion/.scion/plugins/broker
        curl -fsSL '${RELEASE_URL}/${PLUGIN_ARCHIVE}' -o /tmp/${PLUGIN_ARCHIVE}
        if curl -fsSL '${RELEASE_URL}/SHA256SUMS' -o /tmp/SHA256SUMS; then
          if grep -qE '^[0-9a-f]{64} [ *]${PLUGIN_ARCHIVE_RE}\$' /tmp/SHA256SUMS; then
            (cd /tmp && grep -E '^[0-9a-f]{64} [ *]${PLUGIN_ARCHIVE_RE}\$' SHA256SUMS | sha256sum -c -)
          elif [ '${ALLOW_UNVERIFIED_RELEASE}' = 'true' ]; then
            echo 'WARNING: no checksum entry for ${PLUGIN_ARCHIVE} in SHA256SUMS; installing UNVERIFIED (ALLOW_UNVERIFIED_RELEASE=true).' >&2
          else
            echo 'ERROR: no checksum entry for ${PLUGIN_ARCHIVE} in SHA256SUMS -- refusing to install.' >&2
            exit 1
          fi
        elif [ '${ALLOW_UNVERIFIED_RELEASE}' = 'true' ]; then
          echo 'WARNING: could not download SHA256SUMS; installing UNVERIFIED (ALLOW_UNVERIFIED_RELEASE=true).' >&2
        else
          echo 'ERROR: could not download SHA256SUMS for ${VERSION} -- refusing to install.' >&2
          exit 1
        fi
        tar -xzf /tmp/${PLUGIN_ARCHIVE} -C /tmp
        sudo mv /tmp/${PLUGIN_BINARY} /home/scion/.scion/plugins/broker/${PLUGIN_BINARY}
        sudo chown scion:scion /home/scion/.scion/plugins/broker/${PLUGIN_BINARY}
        sudo chmod +x /home/scion/.scion/plugins/broker/${PLUGIN_BINARY}
        rm -f /tmp/${PLUGIN_ARCHIVE} /tmp/SHA256SUMS
        echo 'Installed ${PLUGIN_BINARY}'
      "
  done
  echo "  All chat plugins installed."
fi

# --- Generate session secret and write hub.env (idempotent) ---
# Check if hub.env already exists on the VM
HUB_ENV_EXISTS=$(gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="test -f /home/scion/.scion/hub.env && echo yes || echo no" 2>/dev/null) || true

if [[ "$HUB_ENV_EXISTS" == "yes" ]]; then
  info "hub.env already exists, preserving existing SESSION_SECRET."
else
  info "Generating session secret..."
  SESSION_SECRET="$(openssl rand -base64 32)"

  info "Writing hub.env..."
  # Security: write to a local temp file and transfer via SCP to avoid
  # embedding secrets in the gcloud ssh command string, which would be
  # visible in local process listing (ps aux).  Same bug class as #1211.
  HUB_ENV_TMPFILE="$(mktemp)"
  chmod 600 "${HUB_ENV_TMPFILE}"
  sed \
    -e "s|__SESSION_SECRET__|${SESSION_SECRET}|g" \
    -e "s|__PROJECT_ID__|${PROJECT_ID}|g" \
    "${SCRIPT_DIR}/config-templates/hub.env.template" > "${HUB_ENV_TMPFILE}"
  unset SESSION_SECRET

  gcloud compute scp "${HUB_ENV_TMPFILE}" \
    "${INSTANCE_NAME}:/tmp/hub.env" \
    --zone="${ZONE}" --project="${PROJECT_ID}" --quiet
  rm -f "${HUB_ENV_TMPFILE}"

  gcloud compute ssh "${INSTANCE_NAME}" \
    --zone="${ZONE}" --project="${PROJECT_ID}" \
    --command="
      sudo mv /tmp/hub.env /home/scion/.scion/hub.env
      sudo chown scion:scion /home/scion/.scion/hub.env
      sudo chmod 600 /home/scion/.scion/hub.env
    "
fi

# --- Write settings.yaml (dev mode for initial startup) ---
# Phase 5 will overwrite this with proxy auth config once IAP is ready.
info "Writing settings.yaml (dev mode)..."
gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="
    sudo -u scion tee /home/scion/.scion/settings.yaml > /dev/null << 'SETTINGSEOF'
schema_version: \"1\"
# Explicit default harness. Boot already gets antigravity from the embedded
# defaults via the operational-settings seed, but file-mode paths that read
# settings.yaml directly (admin server-config page; reloadSettings after an
# admin save) do not merge embedded defaults and would otherwise see \"\".
default_harness_config: antigravity
image_registry: \"${IMAGE_REGISTRY}\"
${HYBRID_GCP_IDENTITY_YAML:-"# Hub-wide default GCP identity mode for new agents (V1Settings.DefaultGCPIdentityMode
# in pkg/config, a top-level settings.yaml key, not nested under agent_defaults).
# passthrough is honoured only on the hub's own embedded broker, which is what a
# single-node VM always is, so agents inherit the VM service account (already
# granted roles/aiplatform.user) and authenticate to Vertex AI with no manual
# setup. Change to \"block\" or \"assign\" via the admin UI or the server-config
# API if that is not desired; see docs/deploy/agent-runbook-single-node-vm.md §6.3b.
default_gcp_identity_mode: passthrough
"}server:
  hub:
    name: \"${HUB_NAME}\"
${ADMIN_EMAIL:+    admin_emails:
      - \"${ADMIN_EMAIL}\"}
  maintenance:
    deployment_tier: \"binary\"
    release_channel: \"${RELEASE_CHANNEL}\"
    update_policy: \"${UPDATE_POLICY}\"
  storage:
    local_path: /home/scion/.scion/workspace-storage
  secrets:
    backend: local
  auth:
    mode: dev
${HYBRID_AUTH_TRANSPORT_YAML:+${HYBRID_AUTH_TRANSPORT_YAML}
}${HYBRID_USER_ACCESS_YAML:+${HYBRID_USER_ACCESS_YAML}
}${HYBRID_SHARED_DIR_STORAGE_YAML:+${HYBRID_SHARED_DIR_STORAGE_YAML}
}  listen_port: 8080
SETTINGSEOF
  "

# --- Install systemd unit ---
info "Installing systemd service..."
gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="
    sudo tee /etc/systemd/system/scion-hub.service > /dev/null << 'SERVICEEOF'
$(cat "${SCRIPT_DIR}/config-templates/scion-hub.service")
SERVICEEOF
    sudo systemctl daemon-reload
    sudo systemctl enable scion-hub.service
    sudo systemctl start scion-hub.service
    echo 'scion-hub.service started.'
  "

# --- Health check ---
info "Running health check..."
wait_for_hub_health "Health check" || exit 1

# --- Hub-scoped agent env vars (GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION) ---
# Agents need these for Vertex AI inference. They go in the hub DB as
# hub-scoped env vars with injection mode "always", the same rows that
# `PUT /api/v1/env/<KEY>` with scope=hub would create. There is no
# unauthenticated way to call that API from the VM, so we write the rows
# with sqlite3 directly. This must run after the health check, because the
# hub creates the env_vars table when it first migrates.
#   - scope_id is the hub instance ID. The dispatcher selects hub rows by it,
#     and so do the API and web UI. We read it from /healthz so it always
#     matches the running hub.
#   - The insert only seeds the rows: ON CONFLICT DO NOTHING against the
#     unique index envvar_key_scope_scope_id means a redeploy never duplicates
#     them and never overwrites an admin's edits. A row that was deleted is
#     created again.
#   - The hub reads env vars from the DB on every dispatch (there is no
#     cache), so it does not need a restart.
#   - Timestamps use Go's time.String() layout, which ent writes and the
#     modernc driver reads back.
# If this step fails it only warns, because the vars can be set by hand.
info "Setting hub-scoped agent env vars (GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_LOCATION)..."
HUB_ENV_SCOPE_ID="$(gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="curl -sf http://localhost:8080/healthz" 2>/dev/null \
  | sed -n 's/.*"hub_id":"\([^"]*\)".*/\1/p')" || true

# Escape a value as an SQL string literal. The inputs are also validated
# below, so this is a second layer of protection.
sql_quote() { local s="${1//\'/\'\'}"; printf "'%s'" "$s"; }

# Random RFC 4122 v4 UUID in canonical dashed form, the same text form ent
# stores for the id column.
SQL_UUID="lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))), 2) || '-' || substr('89ab', 1 + (abs(random()) % 4), 1) || substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6)))"
SQL_NOW="strftime('%Y-%m-%d %H:%M:%f +0000 UTC', 'now')"

if [[ -n "$HUB_ENV_SCOPE_ID" ]]; then
  HUB_ENV_SQL_SCOPE_ID="$(sql_quote "${HUB_ENV_SCOPE_ID}")"
else
  # /healthz was unreadable, so the manual fallback below has to find the hub
  # ID itself. It reads the persisted ID from ~/.scion/hub-id with the sqlite3
  # CLI's readfile(). deploy.sh never sets hub_id explicitly, so that file is
  # the hub ID. If the file is missing or empty, the expression is NULL and the
  # NOT NULL constraint rejects the insert, so no junk row is written.
  HUB_ENV_SQL_SCOPE_ID="NULLIF(trim(CAST(readfile('/home/scion/.scion/hub-id') AS TEXT), char(10, 13, 32)), '')"
fi
# GOOGLE_CLOUD_LOCATION=global on purpose: the global Vertex AI endpoint
# serves the Gemini/Claude models agents use, and it is not tied to REGION.
HUB_ENV_SQL="$(cat <<EOF
.timeout 5000
.bail on
INSERT INTO env_vars (id, key, value, scope, scope_id, description, sensitive, injection_mode, secret, allow_progeny, created, updated)
VALUES
  (${SQL_UUID}, 'GOOGLE_CLOUD_PROJECT', $(sql_quote "${PROJECT_ID}"), 'hub', ${HUB_ENV_SQL_SCOPE_ID}, 'Set by deploy.sh', 0, 'always', 0, 0, ${SQL_NOW}, ${SQL_NOW}),
  (${SQL_UUID}, 'GOOGLE_CLOUD_LOCATION', 'global', 'hub', ${HUB_ENV_SQL_SCOPE_ID}, 'Set by deploy.sh', 0, 'always', 0, 0, ${SQL_NOW}, ${SQL_NOW})
ON CONFLICT(key, scope, scope_id) DO NOTHING;
SELECT '  ' || key || '=' || value || ' (scope=hub, injection=' || injection_mode || ')'
  FROM env_vars WHERE scope = 'hub' AND scope_id = ${HUB_ENV_SQL_SCOPE_ID}
  AND key IN ('GOOGLE_CLOUD_PROJECT', 'GOOGLE_CLOUD_LOCATION') ORDER BY key;
EOF
)"

HUB_ENV_OK=false
if [[ ! "$PROJECT_ID" =~ ^[a-z][a-z0-9.:-]*$ ]]; then
  warn "PROJECT_ID '${PROJECT_ID}' has unexpected characters; skipping hub env var write."
elif [[ ! "$HUB_ENV_SCOPE_ID" =~ ^[A-Za-z0-9._-]+$ ]]; then
  warn "Could not read hub_id from /healthz; skipping hub env var write."
elif gcloud compute ssh "${INSTANCE_NAME}" \
    --zone="${ZONE}" --project="${PROJECT_ID}" \
    --command="
      set -euo pipefail
      if ! command -v sqlite3 >/dev/null 2>&1; then
        # VMs created before sqlite3 was in cloud-init.yaml don't have it.
        sudo apt-get update -qq >/dev/null
        sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq sqlite3 >/dev/null
      fi
      sudo -u scion sqlite3 /home/scion/.scion/hub.db << 'SQLEOF'
${HUB_ENV_SQL}
SQLEOF
    "; then
  HUB_ENV_OK=true
  echo "  Hub env vars seeded (scope_id=${HUB_ENV_SCOPE_ID}; existing rows left unchanged)."
fi

if [[ "$HUB_ENV_OK" != "true" ]]; then
  warn "Hub-scoped env vars were NOT set. Agents will not get GOOGLE_CLOUD_PROJECT/GOOGLE_CLOUD_LOCATION."
  {
    echo "  Set them manually (the deploy continues). If sqlite3 is missing, first run"
    echo "  'sudo apt-get update && sudo apt-get install -y sqlite3' on the VM."
    echo "  gcloud compute ssh ${INSTANCE_NAME} --zone=${ZONE} --project=${PROJECT_ID} -- \\"
    echo "    'sudo -u scion sqlite3 /home/scion/.scion/hub.db' <<'SQLEOF'"
    echo "${HUB_ENV_SQL}"
    echo "SQLEOF"
  } >&2
fi

# ===================================================================
# Phase 3b: Container Images
# ===================================================================
if [[ "$IMAGE_SOURCE" == "build" ]]; then
  section "Phase 3b: Build Container Images on VM"

  # NOTE: the checkout lives under /opt (root-owned, world-traversable),
  # not /home/scion — the deployer identity running this over `gcloud
  # compute ssh` is never a member of the `scion` group, and /home/scion
  # is mode 0750 once cloud-init's `useradd -m` runs. Every privileged
  # step uses `sudo`; the deployer only needs to traverse /opt and
  # write /tmp.
  #
  # Idempotency: a successful build writes a version marker
  # (/opt/scion-source/.images-built.version) containing $VERSION. A re-run
  # skips the 10-15 min build iff the marker matches $VERSION AND all 3
  # expected images are still present — a stale `:latest` tag alone is not
  # enough, since re-running with --version vNEXT must not silently keep old
  # images just because something answers to `:latest`. The marker file is
  # what makes the skip version-aware.
  IMAGES_BUILT_MARKER="/opt/scion-source/.images-built.version"
  SKIP_BUILD=false
  if [[ "$CFG_FORCE_REBUILD" == "true" ]]; then
    info "Forcing a rebuild (container_images.force_rebuild or --rebuild-images is set)."
  else
    info "Checking whether images for version ${VERSION} are already built..."
    BUILD_CHECK=$(gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="
        set -euo pipefail
        MARKER='${IMAGES_BUILT_MARKER}'
        if [ -f \"\$MARKER\" ] \
          && [ \"\$(sudo cat \"\$MARKER\" 2>/dev/null)\" = '${VERSION}' ] \
          && sudo docker image inspect localhost/scion/core-base:latest >/dev/null 2>&1 \
          && sudo docker image inspect localhost/scion/scion-base:latest >/dev/null 2>&1 \
          && sudo docker image inspect localhost/scion/scion-antigravity:latest >/dev/null 2>&1; then
          echo skip
        else
          echo build
        fi
      " 2>/dev/null) || true
    if [[ "$BUILD_CHECK" == "skip" ]]; then
      SKIP_BUILD=true
    fi
  fi

  if [[ "$SKIP_BUILD" == "true" ]]; then
    info "Images already built for version ${VERSION} (marker + all 3 images present); skipping build."
    echo "  Set container_images.force_rebuild: true in the config, or pass --rebuild-images, to override."
  else
    info "Cloning scion repository on VM..."
    gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="
        set -euo pipefail
        if [ ! -d /opt/scion-source ]; then
          sudo git clone --depth 1 --branch '${VERSION}' \
            https://github.com/GoogleCloudPlatform/scion.git /opt/scion-source
        else
          cd /opt/scion-source
          sudo git fetch --depth 1 origin tag '${VERSION}'
          sudo git checkout '${VERSION}'
        fi
      "

    # Build only the minimal set of images needed for deployment:
    # core-base (foundation) -> scion-base (adds scion binary) -> scion-antigravity (default harness)
    # Using --target all would build ALL ~12 images including harnesses with known build issues.
    info "Building container images (this may take 10-15 minutes)..."
    gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="
        set -euo pipefail
        cd /opt/scion-source
        rm -f /tmp/scion-image-build.exit
        # Clear the marker before starting: if this build is interrupted or
        # fails partway, no marker should be left claiming a stale version
        # is built (Step 5 below only writes it back on full success).
        sudo rm -f '${IMAGES_BUILT_MARKER}'
        nohup bash -c '
          {
            set -euo pipefail
            # Step 1: Build core-base
            echo \"=== Building core-base ===\"
            sudo bash image-build/scripts/build-images.sh \
              --builder local-docker \
              --target core-base \
              --tag latest

            # Step 2: Build scion-base
            echo \"=== Building scion-base ===\"
            sudo bash image-build/scripts/build-images.sh \
              --builder local-docker \
              --target scion-base \
              --tag latest

            # Step 3: Build antigravity harness directly
            echo \"=== Building scion-antigravity ===\"
            sudo docker build \
              -t scion-antigravity:latest \
              --build-arg BASE_IMAGE=scion-base:latest \
              -f harnesses/antigravity/Dockerfile \
              harnesses/antigravity/

            # Step 4: Tag images under localhost/scion for the runtime
            echo \"=== Tagging images for localhost/scion registry ===\"
            sudo docker tag core-base:latest localhost/scion/core-base:latest
            sudo docker tag scion-base:latest localhost/scion/scion-base:latest
            sudo docker tag scion-antigravity:latest localhost/scion/scion-antigravity:latest

            # Step 5: Record the version marker so re-runs can skip the build.
            # Written last, only on full success (set -e above aborts before
            # this line if any prior step failed).
            echo \"=== Recording image build marker (version ${VERSION}) ===\"
            echo '${VERSION}' | sudo tee '${IMAGES_BUILT_MARKER}' > /dev/null

            echo \"=== All images built and tagged successfully ===\"
          } > /tmp/scion-image-build.log 2>&1
          echo \$? > /tmp/scion-image-build.exit
        ' >/dev/null 2>&1 </dev/null &
        echo \$! > /tmp/scion-image-build.pid
        echo \"Image build started in background (PID \$(cat /tmp/scion-image-build.pid))\"
      "

    # Poll for build completion
    info "Waiting for image build to complete..."
    BUILD_DONE=false
    POLL_COUNT=0
    while [[ "$BUILD_DONE" != "true" ]] && [[ $POLL_COUNT -lt $BUILD_POLL_MAX_ATTEMPTS ]]; do
      sleep "$BUILD_POLL_INTERVAL_SECS"
      POLL_COUNT=$((POLL_COUNT + 1))
      # Check if the exit code file exists (build finished)
      BUILD_EXIT=$(gcloud compute ssh "${INSTANCE_NAME}" \
        --zone="${ZONE}" --project="${PROJECT_ID}" \
        --command="cat /tmp/scion-image-build.exit 2>/dev/null || echo running" \
        2>/dev/null) || true
      if [[ "$BUILD_EXIT" != "running" ]]; then
        BUILD_DONE=true
      else
        # Show progress (last line of build log)
        LAST_LINE=$(gcloud compute ssh "${INSTANCE_NAME}" \
          --zone="${ZONE}" --project="${PROJECT_ID}" \
          --command="tail -1 /tmp/scion-image-build.log 2>/dev/null || echo '(waiting...)'" \
          2>/dev/null) || true
        echo "  [${POLL_COUNT}] ${LAST_LINE}"
      fi
    done

    if [[ "$BUILD_DONE" != "true" ]]; then
      err "Image build timed out after $((BUILD_POLL_MAX_ATTEMPTS * BUILD_POLL_INTERVAL_SECS / 60)) minutes."
      echo "  Check build log: gcloud compute ssh ${INSTANCE_NAME} --zone=${ZONE} --project=${PROJECT_ID} --command='cat /tmp/scion-image-build.log'"
      exit 1
    fi

    if [[ "$BUILD_EXIT" != "0" ]]; then
      err "Image build failed (exit code: ${BUILD_EXIT})."
      echo "  Check build log: gcloud compute ssh ${INSTANCE_NAME} --zone=${ZONE} --project=${PROJECT_ID} --command='tail -50 /tmp/scion-image-build.log'"
      exit 1
    fi
    echo "  Image build completed successfully."
  fi

  info "Verifying container images..."
  gcloud compute ssh "${INSTANCE_NAME}" \
    --zone="${ZONE}" --project="${PROJECT_ID}" \
    --command="sudo docker images | grep -E 'localhost/scion|core-base|scion-base|scion-antigravity'"
  echo "  Container images built and tagged successfully."
else
  section "Phase 3b: Container Images (Registry)"
  info "Using pre-built images from registry: ${IMAGE_REGISTRY}"
  echo "  The hub will pull images from: ${IMAGE_REGISTRY}"
fi

# ===================================================================
# Phase 4: IAP Proxy
# ===================================================================
section "Phase 4: IAP Proxy"

# --- Hybrid tier: Kubernetes objects (PV, namespace, PVC) ---
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  info "Ensuring hybrid-tier Kubernetes objects (if needed)..."
  hybrid_k8s_ensure_objects "$HUB_NAME" "$VM_IP"
fi

# --- Build and deploy Cloud Run IAP proxy ---
# We build the proxy image on the VM and deploy with --image instead of
# `gcloud run deploy --source`, because --source triggers Cloud Build, which
# uploads source to GCS. Enterprise projects with the org policy
# constraints/gcp.restrictServiceUsage denying storage.googleapis.com reject
# that upload with HTTP 403. Docker is already available on the VM and the
# repo is already cloned at /opt/scion-source, so we build and push there
# instead.
PROXY_SERVICE="${INSTANCE_NAME}-iap-proxy"
AR_REPO="cloud-run-source-deploy"
PROXY_IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}/${PROXY_SERVICE}:latest"

# --- Ensure Artifact Registry repo exists ---
# This repo name matches what `gcloud run deploy --source` auto-creates, so
# this stays backwards-compatible with prior --source deploys.
info "Ensuring Artifact Registry repo exists: ${AR_REPO}..."
gcloud artifacts repositories create "${AR_REPO}" \
  --project="${PROJECT_ID}" \
  --location="${REGION}" \
  --repository-format=docker \
  --quiet 2>/dev/null || true
echo "  Artifact Registry repo ready: ${AR_REPO}"

# --- Build proxy image on the VM ---
# When IMAGE_SOURCE=registry, Phase 3b is skipped and /opt/scion-source may
# not exist on the VM. Ensure the repo is cloned (shallow) before building.
info "Building proxy image on VM..."
echo "  Image: ${PROXY_IMAGE}"
gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="
    set -euo pipefail
    if [ ! -d /opt/scion-source ]; then
      sudo git clone --depth 1 \
        https://github.com/GoogleCloudPlatform/scion.git /opt/scion-source
    fi
    sudo docker build -t ${PROXY_IMAGE} /opt/scion-source/extras/cloudrun-iap-proxy
  "
echo "  Proxy image built on VM."

# --- Push proxy image to Artifact Registry ---
info "Pushing proxy image to Artifact Registry..."
gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="sudo gcloud auth configure-docker ${REGION}-docker.pkg.dev --quiet && sudo docker push ${PROXY_IMAGE}"
echo "  Proxy image pushed: ${PROXY_IMAGE}"

# --- Deploy Cloud Run IAP proxy ---
# One-step IAP: --no-allow-unauthenticated --iap on the initial deploy
# itself, instead of `--allow-unauthenticated` followed by a later
# `gcloud beta run services update --iap`. The two-step form has a window
# -- between the two calls -- where the service carries an allUsers
# roles/run.invoker binding; organizations enforcing
# constraints/iam.allowedPolicyMemberDomains reject that binding outright,
# and even where it's allowed, it's an unauthenticated-access window this
# script should never create. `beta run deploy` (rather than the non-beta
# `run deploy`) is used here for compatibility with older Cloud SDK
# versions where --iap required the beta surface; current SDKs also
# support --iap on plain `run deploy`. --service-account is the dedicated
# proxy SA from Phase 2, never SA_EMAIL (the hub VM's own SA).
info "Deploying Cloud Run IAP proxy: ${PROXY_SERVICE}..."
echo "  Target URL: http://${VM_IP}:8080"
echo "  Image: ${PROXY_IMAGE}"

PROXY_SERVICE_LABEL_ARGS=()
PROXY_LABEL_ARG="$(hybrid_cloud_run_label_args "${PROXY_SERVICE}" "${PROJECT_ID}" "${REGION}" "${HUB_NAME}")"
[[ -n "$PROXY_LABEL_ARG" ]] && PROXY_SERVICE_LABEL_ARGS=("$PROXY_LABEL_ARG")

gcloud beta run deploy "${PROXY_SERVICE}" \
  --project="${PROJECT_ID}" \
  --region="${REGION}" \
  --image="${PROXY_IMAGE}" \
  --service-account="${PROXY_SA_EMAIL}" \
  --set-env-vars="TARGET_URL=http://${VM_IP}:8080" \
  --network=default \
  --subnet=default \
  --vpc-egress=all-traffic \
  --no-allow-unauthenticated \
  --iap \
  --port=8080 \
  ${PROXY_SERVICE_LABEL_ARGS[@]+"${PROXY_SERVICE_LABEL_ARGS[@]}"} \
  --quiet
echo "  Cloud Run IAP proxy deployed with IAP enabled (no allUsers invoker binding was ever created)."

# --- Get Cloud Run service URL ---
info "Getting Cloud Run service URL..."
PROXY_URL="$(gcloud run services describe "${PROXY_SERVICE}" \
  --project="${PROJECT_ID}" --region="${REGION}" \
  --format="value(status.url)")"
if [[ -z "$PROXY_URL" ]]; then
  err "Could not retrieve Cloud Run service URL for ${PROXY_SERVICE}"
  exit 1
fi
echo "  Proxy URL: ${PROXY_URL}"

# --- Project number (needed below, and again for the IAP audience in
# Phase 5 -- computed once here and reused there) ---
info "Looking up project number..."
PROJECT_NUMBER="$(gcloud projects describe "${PROJECT_ID}" \
  --format="value(projectNumber)" 2>/dev/null)" || true
if [[ -z "$PROJECT_NUMBER" ]]; then
  err "Could not determine project number for ${PROJECT_ID}"
  exit 1
fi
echo "  Project number: ${PROJECT_NUMBER}"

# --- Grant the IAP service agent access to invoke the proxy ---
# gcloud's --iap above already grants the IAP service agent
# roles/run.invoker itself (serverless_operations._HandleIap), but only
# warns if that grant fails -- it never fails the deploy. This explicit
# call is a safety net that makes that failure fatal instead: idempotent
# (add-iam-policy-binding is a no-op if the binding already exists), so
# it's a no-op itself on the common path where gcloud's own grant already
# worked. --condition=None matches this deploy family's existing
# IAM-binding style (see scripts/cloudrun/deploy.sh's add_project_role)
# and avoids an interactive "add a condition?" prompt in an org with
# conditional IAM.
info "Granting the IAP service agent access to invoke the proxy..."
IAP_SA_EMAIL="service-${PROJECT_NUMBER}@gcp-sa-iap.iam.gserviceaccount.com"
gcloud run services add-iam-policy-binding "${PROXY_SERVICE}" \
  --project="${PROJECT_ID}" \
  --region="${REGION}" \
  --member="serviceAccount:${IAP_SA_EMAIL}" \
  --role="roles/run.invoker" \
  --condition=None \
  --quiet
echo "  IAP service agent can invoke: ${PROXY_SERVICE}"

# --- Verify no allUsers invoker binding remains ---
# `gcloud beta run deploy --no-allow-unauthenticated` above already asks
# gcloud to remove any allUsers roles/run.invoker binding when the
# service already existed (serverless_operations._HandleAllowUnauthenticated),
# which covers the upgrade path from a prior --allow-unauthenticated
# deploy. But gcloud only warns if that removal fails -- it never fails
# the deploy -- so this re-checks explicitly and fails safe: query the
# current policy first (a fresh deploy, or one where gcloud's own removal
# already worked, finds nothing and does nothing further -- no removal
# call, no warning); if allUsers is still bound, try one more removal,
# and if that also fails, stop the deploy rather than leave the service
# reachable by anyone.
info "Verifying no allUsers invoker binding remains..."
# Also removed by the single EXIT trap registered near the top of this
# script, so an interrupt while get-iam-policy runs does not leave it in
# $TMPDIR.
ALLUSERS_STDERR_FILE="$(mktemp)"
if ALLUSERS_INVOKER="$(gcloud run services get-iam-policy "${PROXY_SERVICE}" \
    --project="${PROJECT_ID}" \
    --region="${REGION}" \
    --flatten="bindings[].members" \
    --filter="bindings.role=roles/run.invoker AND bindings.members=allUsers" \
    --format="value(bindings.members)" 2>"${ALLUSERS_STDERR_FILE}")"; then
  rm -f "${ALLUSERS_STDERR_FILE}"
else
  # Fail closed, not open: an unreadable policy (a transient error, a
  # permissions gap, ...) is not the same as "verified no allUsers
  # binding", and treating it that way would silently defeat the whole
  # point of this check.
  err "Could not read the IAM policy for ${PROXY_SERVICE} to verify no allUsers invoker binding remains:"
  if [[ -s "${ALLUSERS_STDERR_FILE}" ]]; then
    cat "${ALLUSERS_STDERR_FILE}" >&2
  fi
  rm -f "${ALLUSERS_STDERR_FILE}"
  exit 1
fi
if [[ -z "$ALLUSERS_INVOKER" ]]; then
  echo "  No allUsers invoker binding present."
else
  warn "Found a legacy allUsers invoker binding on ${PROXY_SERVICE} (likely from a prior --allow-unauthenticated deploy); removing it."
  if gcloud run services remove-iam-policy-binding "${PROXY_SERVICE}" \
      --project="${PROJECT_ID}" \
      --region="${REGION}" \
      --member="allUsers" \
      --role="roles/run.invoker" \
      --condition=None \
      --quiet 2>/dev/null; then
    echo "  allUsers invoker binding removed."
  else
    err "Could not remove the allUsers invoker binding from ${PROXY_SERVICE}. Refusing to leave this service reachable by allUsers."
    err "Verify and fix manually: gcloud run services get-iam-policy ${PROXY_SERVICE} --region=${REGION} --project=${PROJECT_ID}"
    exit 1
  fi
fi

# --- Bind IAP access for deployer ---
info "Binding IAP access for deployer..."
# Use gcloud auth list for robust detection (works with service accounts and CI),
# fall back to gcloud config get account.
OPERATOR_EMAIL="$(gcloud auth list --filter=status:ACTIVE --format='value(account)' 2>/dev/null | head -1)" || true
if [[ -z "$OPERATOR_EMAIL" ]]; then
  OPERATOR_EMAIL="$(gcloud config get-value account 2>/dev/null)" || true
fi
if [[ -z "$OPERATOR_EMAIL" ]]; then
  warn "Could not determine operator email; skipping IAP binding."
else
  # Detect correct IAM member prefix: service accounts vs human users
  if [[ "$OPERATOR_EMAIL" == *.gserviceaccount.com ]]; then
    OPERATOR_MEMBER="serviceAccount:${OPERATOR_EMAIL}"
  else
    OPERATOR_MEMBER="user:${OPERATOR_EMAIL}"
  fi
  # Use gcloud iap web add-iam-policy-binding (supports --resource-type=cloud-run)
  # Note: this is distinct from "gcloud iap web enable" which does NOT support cloud-run.
  if SERVICE_IAP_BIND_ERR="$(gcloud iap web add-iam-policy-binding \
      --resource-type=cloud-run --service="${PROXY_SERVICE}" \
      --region="${REGION}" --project="${PROJECT_ID}" \
      --member="${OPERATOR_MEMBER}" \
      --role=roles/iap.httpsResourceAccessor \
      --condition=None \
      --quiet 2>&1 >/dev/null)"; then
    echo "  IAP access granted to: ${OPERATOR_EMAIL} (service-level binding)"
  else
    warn "Service-level IAP binding failed; falling back to project-level binding."
    print_gcloud_error "$SERVICE_IAP_BIND_ERR"
    if IAP_BIND_ERR="$(gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
      --member="${OPERATOR_MEMBER}" \
      --role=roles/iap.httpsResourceAccessor \
      --condition=None \
      --quiet 2>&1 >/dev/null)"; then
      echo "  IAP access granted to: ${OPERATOR_EMAIL} (project-level fallback)"
    else
      err "Failed to grant IAP access to ${OPERATOR_EMAIL} at both service and project levels."
      err "Project-level binding error:"
      print_gcloud_error "$IAP_BIND_ERR"
      err "You may not be able to access the Hub. Please grant roles/iap.httpsResourceAccessor manually."
    fi
  fi
fi

# --- Hybrid tier: grant agent transport SA IAP access ---
# The Cloud Run service and its own IAP enablement now exist, so the
# resource-level grant deferred from the transport-setup step in Phase 2
# happens here, alongside the operator's own grant above.
if [[ "$HYBRID_ENABLED" == "true" ]]; then
  info "Granting agent transport service account IAP access..."
  hybrid_grant_transport_sa_iap_access "${HYBRID_TRANSPORT_SA_EMAIL}" "${PROXY_SERVICE}" "${REGION}" "${PROJECT_ID}"
fi

# --- Wait for IAP enforcement ---
info "Waiting for IAP enforcement to activate..."
echo "  IAP takes 30-60 seconds to begin enforcing after being enabled."
echo "  Waiting ${IAP_ENFORCEMENT_WAIT_SECS} seconds..."
sleep "$IAP_ENFORCEMENT_WAIT_SECS"
echo "  Wait complete."

# ===================================================================
# Phase 5: Finalize
# ===================================================================
section "Phase 5: Finalize"

# --- Compute IAP audience ---
# For Cloud Run services, the IAP audience uses the format:
#   /projects/PROJECT_NUMBER/locations/REGION/services/SERVICE_NAME
# PROJECT_NUMBER was already looked up in Phase 4 (needed there first, to
# address the IAP service agent binding), so this just reuses it.
info "Computing IAP audience..."
IAP_AUDIENCE="/projects/${PROJECT_NUMBER}/locations/${REGION}/services/${PROXY_SERVICE}"
echo "  IAP audience:   ${IAP_AUDIENCE}"

# --- Update settings.yaml with proxy auth ---
info "Updating settings.yaml with proxy auth configuration..."
gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="
    sudo -u scion tee /home/scion/.scion/settings.yaml > /dev/null << 'SETTINGSEOF'
schema_version: \"1\"
# Explicit default harness. Boot already gets antigravity from the embedded
# defaults via the operational-settings seed, but file-mode paths that read
# settings.yaml directly (admin server-config page; reloadSettings after an
# admin save) do not merge embedded defaults and would otherwise see \"\".
default_harness_config: antigravity
image_registry: \"${IMAGE_REGISTRY}\"
${HYBRID_GCP_IDENTITY_YAML:-"# Hub-wide default GCP identity mode for new agents (V1Settings.DefaultGCPIdentityMode
# in pkg/config, a top-level settings.yaml key, not nested under agent_defaults).
# passthrough is honoured only on the hub's own embedded broker, which is what a
# single-node VM always is, so agents inherit the VM service account (already
# granted roles/aiplatform.user) and authenticate to Vertex AI with no manual
# setup. Change to \"block\" or \"assign\" via the admin UI or the server-config
# API if that is not desired; see docs/deploy/agent-runbook-single-node-vm.md §6.3b.
default_gcp_identity_mode: passthrough
"}server:
  hub:
    name: \"${HUB_NAME}\"
${ADMIN_EMAIL:+    admin_emails:
      - \"${ADMIN_EMAIL}\"}
  maintenance:
    deployment_tier: \"binary\"
    release_channel: \"${RELEASE_CHANNEL}\"
    update_policy: \"${UPDATE_POLICY}\"
  storage:
    local_path: /home/scion/.scion/workspace-storage
  secrets:
    backend: local
  auth:
    mode: proxy
    proxy:
      provider: iap
      iap:
        audience: \"${IAP_AUDIENCE}\"
${HYBRID_AUTH_TRANSPORT_YAML:+${HYBRID_AUTH_TRANSPORT_YAML}
}${HYBRID_USER_ACCESS_YAML:+${HYBRID_USER_ACCESS_YAML}
}${HYBRID_SHARED_DIR_STORAGE_YAML:+${HYBRID_SHARED_DIR_STORAGE_YAML}
}  listen_port: 8080
SETTINGSEOF
  "
echo "  settings.yaml updated (auth mode: proxy, provider: iap)."

# --- Restart hub service ---
info "Restarting scion-hub.service..."
gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="
    sudo systemctl restart scion-hub.service
    echo 'scion-hub.service restarted.'
  "

# --- Post-restart health check ---
info "Running post-restart health check..."
wait_for_hub_health "Post-restart health check" || exit 1

# ===================================================================
# Done
# ===================================================================
echo ""
echo -e "${BOLD}=== Deployment Complete ===${RESET}"
echo ""
echo "  Hub name:     ${HUB_NAME}"
echo "  Instance:     ${INSTANCE_NAME}"
echo "  Zone:         ${ZONE}"
echo "  Scion:        ${VERSION}"
echo "  Auth mode:    proxy (IAP)"
echo "  Proxy:        ${PROXY_SERVICE}"
echo "  Access URL:   ${PROXY_URL}"
echo ""
echo "Open the following URL in your browser to access the hub:"
echo ""
echo "  ${PROXY_URL}"
echo ""
echo "You will be prompted to authenticate via Google IAP."
echo ""
echo "Agents running on the VM reach the hub at http://scion-hub.internal:8080 over the Docker bridge (no IAP needed)."
echo ""
echo "To view service logs:"
echo ""
echo "  gcloud compute ssh ${INSTANCE_NAME} \\"
echo "    --zone=${ZONE} --project=${PROJECT_ID} \\"
echo "    --command='sudo journalctl -u scion-hub.service -f'"
