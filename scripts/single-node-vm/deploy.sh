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
# Agents running on the VM connect via localhost (no IAP needed).
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
#                                 (build on the VM).
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
readonly IAP_ENFORCEMENT_WAIT_SECS=60

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

  echo ""
  echo "The following resources will be deleted:"
  echo "  Cloud Run service: ${PROXY_SERVICE} (region: ${REGION})"
  echo "  GCE VM:            ${INSTANCE_NAME} (zone: ${ZONE})"
  echo "  Cloud NAT:         ${NAT_NAME} (router: ${ROUTER_NAME})"
  echo "  Cloud Router:      ${ROUTER_NAME} (region: ${REGION})"
  echo "  Service account:   ${SA_EMAIL}"
  echo "  Firewall rule:     ${FW_RULE_NAME}"
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

  info "Deleting Cloud Run IAP proxy service..."
  if gcloud run services delete "${PROXY_SERVICE}" \
      --region="${REGION}" --project="${PROJECT_ID}" --quiet 2>/dev/null; then
    echo "  Deleted: ${PROXY_SERVICE}"
  else
    warn "Cloud Run service ${PROXY_SERVICE} not found or already deleted."
  fi

  info "Deleting GCE VM..."
  if gcloud compute instances delete "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" --quiet 2>/dev/null; then
    echo "  Deleted: ${INSTANCE_NAME}"
  else
    warn "GCE VM ${INSTANCE_NAME} not found or already deleted."
  fi

  info "Deleting Cloud NAT..."
  if gcloud compute routers nats delete "${NAT_NAME}" \
      --router="${ROUTER_NAME}" \
      --region="${REGION}" --project="${PROJECT_ID}" --quiet 2>/dev/null; then
    echo "  Deleted: ${NAT_NAME}"
  else
    warn "Cloud NAT ${NAT_NAME} not found or already deleted."
  fi

  info "Deleting Cloud Router..."
  if gcloud compute routers delete "${ROUTER_NAME}" \
      --region="${REGION}" --project="${PROJECT_ID}" --quiet 2>/dev/null; then
    echo "  Deleted: ${ROUTER_NAME}"
  else
    warn "Cloud Router ${ROUTER_NAME} not found or already deleted."
  fi

  info "Deleting service account..."
  if gcloud iam service-accounts delete "${SA_EMAIL}" \
      --project="${PROJECT_ID}" --quiet 2>/dev/null; then
    echo "  Deleted: ${SA_EMAIL}"
  else
    warn "Service account ${SA_EMAIL} not found or already deleted."
  fi

  # Note: We intentionally do NOT revoke roles/iap.tunnelResourceAccessor from
  # the deployer. This role is bound to the operator (not a service account) and
  # may be used for IAP SSH access to other VMs in the project. Revoking it here
  # would silently break access to those other resources.
  info "Skipping IAP tunnel role cleanup (operator may use it for other VMs)."

  info "Deleting IAP SSH firewall rule..."
  if gcloud compute firewall-rules delete "${FW_RULE_NAME}" \
      --project="${PROJECT_ID}" --quiet 2>/dev/null; then
    echo "  Deleted: ${FW_RULE_NAME}"
  else
    warn "Firewall rule ${FW_RULE_NAME} not found or already deleted."
  fi

  echo ""
  echo -e "${BOLD}=== Teardown Complete ===${RESET}"
  echo ""
  echo "  Deleted Cloud Run service: ${PROXY_SERVICE}"
  echo "  Deleted GCE VM:            ${INSTANCE_NAME}"
  echo "  Deleted Cloud NAT:         ${NAT_NAME}"
  echo "  Deleted Cloud Router:      ${ROUTER_NAME}"
  echo "  Deleted service account:   ${SA_EMAIL}"
  echo "  Deleted firewall rule:     ${FW_RULE_NAME}"
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

# ===================================================================
# Phase 2: GCP Resources
# ===================================================================
section "Phase 2: GCP Resources"

# --- Enable APIs ---
info "Enabling required APIs..."
gcloud services enable \
  compute.googleapis.com \
  run.googleapis.com \
  iap.googleapis.com \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  aiplatform.googleapis.com \
  --project="${PROJECT_ID}" --quiet

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
    --project="${PROJECT_ID}"
  echo "  Created service account: ${SA_EMAIL}"
fi

# Bind minimal IAM roles (idempotent)
# artifactregistry.writer lets the VM build and push the Cloud Run IAP proxy
# image directly to Artifact Registry (see Phase 4).
info "Binding IAM roles..."
for ROLE in roles/logging.logWriter roles/monitoring.metricWriter roles/cloudtrace.agent roles/artifactregistry.writer roles/aiplatform.user; do
  gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member="serviceAccount:${SA_EMAIL}" \
    --role="${ROLE}" \
    --quiet &>/dev/null
done
echo "  Roles bound: logging.logWriter, monitoring.metricWriter, cloudtrace.agent, artifactregistry.writer, aiplatform.user"

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
  if gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member="${DEPLOYER_MEMBER}" \
    --role="roles/iap.tunnelResourceAccessor" \
    --quiet >/dev/null 2>&1; then
    echo "  IAP tunnel access granted to: ${DEPLOYER_EMAIL}"
  else
    warn "Failed to grant roles/iap.tunnelResourceAccessor to ${DEPLOYER_EMAIL}."
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
    --description="Allow SSH via IAP tunneling for Scion Hub" \
    --quiet
  echo "  Created firewall rule: ${FW_RULE_NAME} (target tags: ${HUB_TAG})"
fi

# --- Create VM ---
info "Creating GCE VM (if needed)..."
if gcloud compute instances describe "${INSTANCE_NAME}" \
    --zone="${ZONE}" --project="${PROJECT_ID}" &>/dev/null; then
  echo "  VM already exists: ${INSTANCE_NAME}"
else
  gcloud compute instances create "${INSTANCE_NAME}" \
    --zone="${ZONE}" \
    --project="${PROJECT_ID}" \
    --machine-type="${MACHINE_TYPE}" \
    --no-address \
    --service-account="${SA_EMAIL}" \
    --scopes=cloud-platform \
    --tags="${HUB_TAG}" \
    --boot-disk-size="${DISK_SIZE}" \
    --image-family=ubuntu-2204-lts \
    --image-project=ubuntu-os-cloud \
    --metadata-from-file=user-data="${SCRIPT_DIR}/cloud-init.yaml" \
    --quiet
  echo "  Created VM: ${INSTANCE_NAME} (zone: ${ZONE})"
fi

# --- Wait for SSH readiness (avoids race on initial boot) ---
info "Waiting for SSH access to VM..."
SSH_READY=false
SSH_STDERR_FILE="$(mktemp)"
trap 'rm -f "$SSH_STDERR_FILE"' EXIT
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

# ===================================================================
# Phase 3: VM Setup
# ===================================================================
section "Phase 3: VM Setup"

RELEASE_URL="https://github.com/GoogleCloudPlatform/scion/releases/download/${VERSION}"

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
info "Installing scion binary (${VERSION})..."
gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="
    set -euo pipefail
    echo 'Downloading scion binary...'
    curl -fsSL '${RELEASE_URL}/scion-linux-${ARCH_SUFFIX}.tar.gz' -o /tmp/scion.tar.gz
    tar -xzf /tmp/scion.tar.gz -C /tmp
    sudo mv /tmp/scion /usr/local/bin/scion
    sudo chmod +x /usr/local/bin/scion
    rm -f /tmp/scion.tar.gz
    echo \"Installed scion binary (${VERSION})\"
  "

# --- Download and install chat plugins ---
if [[ ${#CHAT_PLUGINS[@]} -gt 0 ]]; then
  info "Installing chat plugins..."
  for PLUGIN in "${CHAT_PLUGINS[@]}"; do
    PLUGIN_BINARY="scion-plugin-${PLUGIN}"
    PLUGIN_ARCHIVE="${PLUGIN_BINARY}-linux-${ARCH_SUFFIX}.tar.gz"
    info "  Installing ${PLUGIN_BINARY}..."
    gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="
        set -euo pipefail
        sudo -u scion mkdir -p /home/scion/.scion/plugins/broker
        curl -fsSL '${RELEASE_URL}/${PLUGIN_ARCHIVE}' -o /tmp/${PLUGIN_ARCHIVE}
        tar -xzf /tmp/${PLUGIN_ARCHIVE} -C /tmp
        sudo mv /tmp/${PLUGIN_BINARY} /home/scion/.scion/plugins/broker/${PLUGIN_BINARY}
        sudo chown scion:scion /home/scion/.scion/plugins/broker/${PLUGIN_BINARY}
        sudo chmod +x /home/scion/.scion/plugins/broker/${PLUGIN_BINARY}
        rm -f /tmp/${PLUGIN_ARCHIVE}
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
image_registry: \"${IMAGE_REGISTRY}\"
# Hub-wide default GCP identity mode for new agents (V1Settings.DefaultGCPIdentityMode
# in pkg/config, a top-level settings.yaml key, not nested under agent_defaults).
# passthrough is honoured only on the hub's own embedded broker, which is what a
# single-node VM always is, so agents inherit the VM service account (already
# granted roles/aiplatform.user) and authenticate to Vertex AI with no manual
# setup. Change to \"block\" or \"assign\" via the admin UI or the server-config
# API if that is not desired; see docs/deploy/agent-runbook-single-node-vm.md §6.3b.
default_gcp_identity_mode: passthrough
server:
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
  listen_port: 8080
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
HEALTH_OK=false
for i in $(seq 1 "$HEALTH_CHECK_MAX_ATTEMPTS"); do
  if gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="curl -sf http://localhost:8080/healthz" \
      2>/dev/null; then
    HEALTH_OK=true
    break
  fi
  echo "  Attempt ${i}/${HEALTH_CHECK_MAX_ATTEMPTS} - waiting ${HEALTH_CHECK_RETRY_SECS}s..."
  sleep "$HEALTH_CHECK_RETRY_SECS"
done

if [[ "$HEALTH_OK" == "true" ]]; then
  echo ""
  echo -e "${GREEN}  Health check passed.${RESET}"
else
  err "Health check did not pass within $((HEALTH_CHECK_MAX_ATTEMPTS * HEALTH_CHECK_RETRY_SECS))s. The hub is not running."
  echo "  Check the service logs:"
  echo "  gcloud compute ssh ${INSTANCE_NAME} --zone=${ZONE} --project=${PROJECT_ID} \\"
  echo "    --command='sudo journalctl -u scion-hub.service --no-pager -n 50'"
  exit 1
fi

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

# --- Get VM internal IP ---
info "Getting VM internal IP..."
VM_IP="$(gcloud compute instances describe "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --format="get(networkInterfaces[0].networkIP)")"
if [[ -z "$VM_IP" ]]; then
  err "Could not retrieve VM internal IP for ${INSTANCE_NAME}"
  exit 1
fi
echo "  VM internal IP: ${VM_IP}"

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
info "Deploying Cloud Run IAP proxy: ${PROXY_SERVICE}..."
echo "  Target URL: http://${VM_IP}:8080"
echo "  Image: ${PROXY_IMAGE}"

gcloud run deploy "${PROXY_SERVICE}" \
  --project="${PROJECT_ID}" \
  --region="${REGION}" \
  --image="${PROXY_IMAGE}" \
  --set-env-vars="TARGET_URL=http://${VM_IP}:8080" \
  --network=default \
  --subnet=default \
  --vpc-egress=all-traffic \
  --allow-unauthenticated \
  --port=8080 \
  --quiet

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

# --- Enable IAP ---
# Note: --resource-type=cloud-run is NOT valid for gcloud iap web enable.
# The supported path for Cloud Run is the --iap flag on the service itself.
info "Enabling IAP on Cloud Run service..."
gcloud beta run services update "${PROXY_SERVICE}" \
  --region="${REGION}" \
  --project="${PROJECT_ID}" \
  --iap \
  --quiet
echo "  IAP enabled."

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
  if gcloud iap web add-iam-policy-binding \
      --resource-type=cloud-run --service="${PROXY_SERVICE}" \
      --region="${REGION}" --project="${PROJECT_ID}" \
      --member="${OPERATOR_MEMBER}" \
      --role=roles/iap.httpsResourceAccessor \
      --quiet >/dev/null 2>&1; then
    echo "  IAP access granted to: ${OPERATOR_EMAIL} (service-level binding)"
  else
    warn "Service-level IAP binding failed; falling back to project-level binding."
    if gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
      --member="${OPERATOR_MEMBER}" \
      --role=roles/iap.httpsResourceAccessor \
      --quiet >/dev/null 2>&1; then
      echo "  IAP access granted to: ${OPERATOR_EMAIL} (project-level fallback)"
    else
      err "Failed to grant IAP access to ${OPERATOR_EMAIL} at both service and project levels."
      err "You may not be able to access the Hub. Please grant roles/iap.httpsResourceAccessor manually."
    fi
  fi
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
info "Computing IAP audience..."
PROJECT_NUMBER="$(gcloud projects describe "${PROJECT_ID}" \
  --format="value(projectNumber)" 2>/dev/null)" || true
if [[ -z "$PROJECT_NUMBER" ]]; then
  err "Could not determine project number for ${PROJECT_ID}"
  exit 1
fi
IAP_AUDIENCE="/projects/${PROJECT_NUMBER}/locations/${REGION}/services/${PROXY_SERVICE}"
echo "  Project number: ${PROJECT_NUMBER}"
echo "  IAP audience:   ${IAP_AUDIENCE}"

# --- Update settings.yaml with proxy auth ---
info "Updating settings.yaml with proxy auth configuration..."
gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="
    sudo -u scion tee /home/scion/.scion/settings.yaml > /dev/null << 'SETTINGSEOF'
schema_version: \"1\"
image_registry: \"${IMAGE_REGISTRY}\"
# Hub-wide default GCP identity mode for new agents (V1Settings.DefaultGCPIdentityMode
# in pkg/config, a top-level settings.yaml key, not nested under agent_defaults).
# passthrough is honoured only on the hub's own embedded broker, which is what a
# single-node VM always is, so agents inherit the VM service account (already
# granted roles/aiplatform.user) and authenticate to Vertex AI with no manual
# setup. Change to \"block\" or \"assign\" via the admin UI or the server-config
# API if that is not desired; see docs/deploy/agent-runbook-single-node-vm.md §6.3b.
default_gcp_identity_mode: passthrough
server:
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
  listen_port: 8080
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
HEALTH_OK=false
for i in $(seq 1 "$HEALTH_CHECK_MAX_ATTEMPTS"); do
  if gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="curl -sf http://localhost:8080/healthz" \
      2>/dev/null; then
    HEALTH_OK=true
    break
  fi
  echo "  Attempt ${i}/${HEALTH_CHECK_MAX_ATTEMPTS} - waiting ${HEALTH_CHECK_RETRY_SECS}s..."
  sleep "$HEALTH_CHECK_RETRY_SECS"
done

if [[ "$HEALTH_OK" == "true" ]]; then
  echo ""
  echo -e "${GREEN}  Health check passed.${RESET}"
else
  err "Post-restart health check did not pass within $((HEALTH_CHECK_MAX_ATTEMPTS * HEALTH_CHECK_RETRY_SECS))s. The hub is not running."
  echo "  Check the service logs:"
  echo "  gcloud compute ssh ${INSTANCE_NAME} --zone=${ZONE} --project=${PROJECT_ID} \\"
  echo "    --command='sudo journalctl -u scion-hub.service --no-pager -n 50'"
  exit 1
fi

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
echo "Agents running on the VM connect via localhost:8080 (no IAP needed)."
echo ""
echo "To view service logs:"
echo ""
echo "  gcloud compute ssh ${INSTANCE_NAME} \\"
echo "    --zone=${ZONE} --project=${PROJECT_ID} \\"
echo "    --command='sudo journalctl -u scion-hub.service -f'"
