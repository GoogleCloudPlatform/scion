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
#   ./deploy.sh [--version VERSION]
#   ./deploy.sh --delete
#
# Options:
#   --version VERSION   Scion release version to install (e.g. v0.5.0).
#                       If omitted, the latest release is fetched from GitHub.
#   --delete            Tear down all resources created by a previous deploy.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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
# Parse flags
# ---------------------------------------------------------------------------
VERSION=""
DELETE_MODE=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --delete) DELETE_MODE=true; shift ;;
    --help|-h)
      sed -n '/^# scripts\/single-node-vm/,/^[^#]/{ /^#/s/^# \?//p }' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    *) err "Unknown flag: $1"; exit 1 ;;
  esac
done

# ---------------------------------------------------------------------------
# Teardown flow (--delete)
# ---------------------------------------------------------------------------
if [[ "$DELETE_MODE" == "true" ]]; then
  section "Teardown: Delete Single-Node-VM Resources"

  info "Detecting GCP project..."
  PROJECT_ID="$(gcloud config get-value project 2>/dev/null)" || true
  if [[ -z "$PROJECT_ID" ]]; then
    err "No GCP project configured. Run: gcloud config set project PROJECT_ID"
    exit 1
  fi
  echo "  Project: ${PROJECT_ID}"

  read -p "Hub name [my-hub]: " HUB_NAME
  HUB_NAME="${HUB_NAME:-my-hub}"

  read -p "GCP region [us-central1]: " REGION
  REGION="${REGION:-us-central1}"

  ZONE="${REGION}-b"
  INSTANCE_NAME="scion-hub-${HUB_NAME}"
  PROXY_SERVICE="${INSTANCE_NAME}-iap-proxy"
  SA_NAME="scion-hub-vm"
  SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

  echo ""
  echo "The following resources will be deleted:"
  echo "  Cloud Run service: ${PROXY_SERVICE} (region: ${REGION})"
  echo "  GCE VM:            ${INSTANCE_NAME} (zone: ${ZONE})"
  echo "  Service account:   ${SA_EMAIL}"
  echo ""
  read -p "Continue? [y/N]: " CONFIRM
  if [[ "${CONFIRM,,}" != "y" ]]; then
    echo "Aborted."
    exit 0
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

  info "Deleting service account..."
  if gcloud iam service-accounts delete "${SA_EMAIL}" \
      --project="${PROJECT_ID}" --quiet 2>/dev/null; then
    echo "  Deleted: ${SA_EMAIL}"
  else
    warn "Service account ${SA_EMAIL} not found or already deleted."
  fi

  echo ""
  echo -e "${BOLD}=== Teardown Complete ===${RESET}"
  echo ""
  echo "  Deleted Cloud Run service: ${PROXY_SERVICE}"
  echo "  Deleted GCE VM:            ${INSTANCE_NAME}"
  echo "  Deleted service account:   ${SA_EMAIL}"
  exit 0
fi

# ===================================================================
# Phase 1: Prerequisites
# ===================================================================
section "Phase 1: Prerequisites"

# --- GCP project ---
info "Detecting GCP project..."
PROJECT_ID="$(gcloud config get-value project 2>/dev/null)" || true
if [[ -z "$PROJECT_ID" ]]; then
  err "No GCP project configured. Run: gcloud config set project PROJECT_ID"
  exit 1
fi
echo "  Project: ${PROJECT_ID}"

# --- Interactive prompts ---
read -p "Hub name [my-hub]: " HUB_NAME
HUB_NAME="${HUB_NAME:-my-hub}"

read -p "GCP region [us-central1]: " REGION
REGION="${REGION:-us-central1}"

echo "Machine size:"
echo "  1) Small  (e2-standard-4,  4 vCPU,  16GB) - up to ~10 agents"
echo "  2) Medium (n2-standard-16, 16 vCPU, 64GB) - up to ~50 agents"
read -p "Select [1]: " SIZE_CHOICE
SIZE_CHOICE="${SIZE_CHOICE:-1}"

case "$SIZE_CHOICE" in
  1) MACHINE_TYPE="e2-standard-4" ;;
  2) MACHINE_TYPE="n2-standard-16" ;;
  *) err "Invalid selection: $SIZE_CHOICE"; exit 1 ;;
esac

echo "Disk size:"
echo "  1) 200 GB (default)"
echo "  2) 500 GB"
echo "  3) Custom"
read -p "Select [1]: " DISK_CHOICE
DISK_CHOICE="${DISK_CHOICE:-1}"

case "$DISK_CHOICE" in
  1) DISK_SIZE="200GB" ;;
  2) DISK_SIZE="500GB" ;;
  3)
    read -p "Enter disk size in GB: " CUSTOM_DISK
    if [[ -z "$CUSTOM_DISK" ]] || ! [[ "$CUSTOM_DISK" =~ ^[0-9]+$ ]]; then
      err "Invalid disk size: $CUSTOM_DISK"
      exit 1
    fi
    DISK_SIZE="${CUSTOM_DISK}GB"
    ;;
  *) err "Invalid selection: $DISK_CHOICE"; exit 1 ;;
esac

echo "Chat integrations to install:"
echo "  1) Telegram"
echo "  2) Discord"
echo "  3) Slack"
echo "  4) Teams"
echo "  5) None"
read -p "Select (comma-separated) [5]: " CHAT_CHOICE
CHAT_CHOICE="${CHAT_CHOICE:-5}"

# Parse chat plugin selections into an array
CHAT_PLUGINS=()
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

# Derived values
ZONE="${REGION}-b"
INSTANCE_NAME="scion-hub-${HUB_NAME}"
SA_NAME="scion-hub-vm"
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

# --- Release version ---
if [[ -z "$VERSION" ]]; then
  info "Detecting latest Scion release..."
  VERSION="$(curl -fsSL https://api.github.com/repos/GoogleCloudPlatform/scion/releases/latest \
    | grep '"tag_name"' | sed -E 's/.*"tag_name":\s*"([^"]+)".*/\1/')" || true
  if [[ -z "$VERSION" ]]; then
    err "Could not detect latest release. Use --version to specify."
    exit 1
  fi
fi
echo "  Scion version: ${VERSION}"

# --- Validate gcloud auth ---
info "Validating gcloud authentication..."
ACCOUNT="$(gcloud config get-value account 2>/dev/null)" || true
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
  --project="${PROJECT_ID}" --quiet

# --- Service account ---
info "Creating service account (if needed)..."
if gcloud iam service-accounts describe "${SA_EMAIL}" \
    --project="${PROJECT_ID}" &>/dev/null; then
  echo "  Service account already exists: ${SA_EMAIL}"
else
  gcloud iam service-accounts create "${SA_NAME}" \
    --display-name="Scion Hub VM" \
    --project="${PROJECT_ID}"
  echo "  Created service account: ${SA_EMAIL}"
fi

# Bind minimal IAM roles (idempotent)
info "Binding IAM roles..."
for ROLE in roles/logging.logWriter roles/monitoring.metricWriter roles/cloudtrace.agent; do
  gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member="serviceAccount:${SA_EMAIL}" \
    --role="${ROLE}" \
    --quiet &>/dev/null
done
echo "  Roles bound: logging.logWriter, monitoring.metricWriter, cloudtrace.agent"

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
    --boot-disk-size="${DISK_SIZE}" \
    --image-family=ubuntu-2204-lts \
    --image-project=ubuntu-os-cloud \
    --metadata-from-file=user-data="${SCRIPT_DIR}/cloud-init.yaml" \
    --quiet
  echo "  Created VM: ${INSTANCE_NAME} (zone: ${ZONE})"
fi

# --- Wait for cloud-init ---
info "Waiting for cloud-init to complete (this may take a few minutes)..."
MAX_WAIT=300
ELAPSED=0
POLL_INTERVAL=15
while [[ $ELAPSED -lt $MAX_WAIT ]]; do
  if gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="test -f /var/log/cloud-init-output.log && grep -q 'Cloud-init.*finished' /var/log/cloud-init-output.log" \
      --quiet 2>/dev/null; then
    echo "  Cloud-init completed."
    break
  fi
  echo "  Waiting... (${ELAPSED}s / ${MAX_WAIT}s)"
  sleep "$POLL_INTERVAL"
  ELAPSED=$((ELAPSED + POLL_INTERVAL))
done

if [[ $ELAPSED -ge $MAX_WAIT ]]; then
  warn "Cloud-init did not finish within ${MAX_WAIT}s. Proceeding anyway."
fi

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
  HUB_ENV_CONTENT="$(sed \
    -e "s|__SESSION_SECRET__|${SESSION_SECRET}|g" \
    -e "s|__PROJECT_ID__|${PROJECT_ID}|g" \
    "${SCRIPT_DIR}/config-templates/hub.env.template")"

  gcloud compute ssh "${INSTANCE_NAME}" \
    --zone="${ZONE}" --project="${PROJECT_ID}" \
    --command="
      sudo -u scion tee /home/scion/.scion/hub.env > /dev/null << 'ENVEOF'
${HUB_ENV_CONTENT}
ENVEOF
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
settings_version: \"1\"
server:
  hub:
    name: \"${HUB_NAME}\"
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
for i in $(seq 1 12); do
  if gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="curl -sf http://localhost:8080/healthz" \
      2>/dev/null; then
    HEALTH_OK=true
    break
  fi
  echo "  Attempt ${i}/12 - waiting 5s..."
  sleep 5
done

if [[ "$HEALTH_OK" == "true" ]]; then
  echo ""
  echo -e "${GREEN}  Health check passed.${RESET}"
else
  warn "Health check did not pass within 60s. Check the service logs:"
  echo "  gcloud compute ssh ${INSTANCE_NAME} --zone=${ZONE} --project=${PROJECT_ID} \\"
  echo "    --command='sudo journalctl -u scion-hub.service --no-pager -n 50'"
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

# --- Deploy Cloud Run IAP proxy ---
PROXY_SERVICE="${INSTANCE_NAME}-iap-proxy"
info "Deploying Cloud Run IAP proxy: ${PROXY_SERVICE}..."
echo "  Target URL: http://${VM_IP}:8080"
echo "  Source: ${SCRIPT_DIR}/../../extras/cloudrun-iap-proxy"

gcloud run deploy "${PROXY_SERVICE}" \
  --project="${PROJECT_ID}" \
  --region="${REGION}" \
  --source="${SCRIPT_DIR}/../../extras/cloudrun-iap-proxy" \
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
info "Enabling IAP on Cloud Run service..."
gcloud iap web enable \
  --resource-type=cloud-run \
  --service="${PROXY_SERVICE}" \
  --region="${REGION}" \
  --project="${PROJECT_ID}"
echo "  IAP enabled."

# --- Bind IAP access for deployer ---
info "Binding IAP access for deployer..."
OPERATOR_EMAIL="$(gcloud config get-value account 2>/dev/null)" || true
if [[ -z "$OPERATOR_EMAIL" ]]; then
  warn "Could not determine operator email; skipping IAP binding."
else
  gcloud iap web add-iam-policy-binding \
    --resource-type=cloud-run \
    --service="${PROXY_SERVICE}" \
    --region="${REGION}" \
    --project="${PROJECT_ID}" \
    --member="user:${OPERATOR_EMAIL}" \
    --role=roles/iap.httpsResourceAccessUser \
    --quiet
  echo "  IAP access granted to: ${OPERATOR_EMAIL}"
fi

# --- Wait for IAP enforcement ---
info "Waiting for IAP enforcement to activate..."
echo "  IAP takes 30-60 seconds to begin enforcing after being enabled."
echo "  Waiting 60 seconds..."
sleep 60
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
settings_version: \"1\"
server:
  hub:
    name: \"${HUB_NAME}\"
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
for i in $(seq 1 12); do
  if gcloud compute ssh "${INSTANCE_NAME}" \
      --zone="${ZONE}" --project="${PROJECT_ID}" \
      --command="curl -sf http://localhost:8080/healthz" \
      2>/dev/null; then
    HEALTH_OK=true
    break
  fi
  echo "  Attempt ${i}/12 - waiting 5s..."
  sleep 5
done

if [[ "$HEALTH_OK" == "true" ]]; then
  echo ""
  echo -e "${GREEN}  Health check passed.${RESET}"
else
  warn "Health check did not pass within 60s. Check the service logs:"
  echo "  gcloud compute ssh ${INSTANCE_NAME} --zone=${ZONE} --project=${PROJECT_ID} \\"
  echo "    --command='sudo journalctl -u scion-hub.service --no-pager -n 50'"
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
