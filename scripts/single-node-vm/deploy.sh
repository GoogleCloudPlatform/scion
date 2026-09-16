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
# Scion Hub on a GCE VM (no public IP).
#
# This script provisions a GCE VM, downloads the scion binary from GitHub
# Releases, and starts the hub via systemd. The VM has no public IP; access
# is via SSH tunnel (Phase 2) or IAP proxy (Phase 3, not yet implemented).
#
# The script is idempotent: re-running converges without duplication.
#
# Usage:
#   ./deploy.sh [--version VERSION]
#
# Options:
#   --version VERSION   Scion release version to install (e.g. v0.5.0).
#                       If omitted, the latest release is fetched from GitHub.

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
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --help|-h)
      sed -n '/^# scripts\/single-node-vm/,/^[^#]/{ /^#/s/^# \?//p }' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    *) err "Unknown flag: $1"; exit 1 ;;
  esac
done

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
echo "  Instance:     ${INSTANCE_NAME}"

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
    --boot-disk-size=200GB \
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

# --- Write settings.yaml ---
info "Writing settings.yaml..."
SETTINGS_CONTENT="$(sed \
  -e "s|__HUB_NAME__|${HUB_NAME}|g" \
  "${SCRIPT_DIR}/config-templates/settings.yaml.tpl")"

gcloud compute ssh "${INSTANCE_NAME}" \
  --zone="${ZONE}" --project="${PROJECT_ID}" \
  --command="
    sudo -u scion tee /home/scion/.scion/settings.yaml > /dev/null << 'SETTINGSEOF'
${SETTINGS_CONTENT}
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
# Done
# ===================================================================
echo ""
echo -e "${BOLD}=== Deployment Complete ===${RESET}"
echo ""
echo "  Hub name:     ${HUB_NAME}"
echo "  Instance:     ${INSTANCE_NAME}"
echo "  Zone:         ${ZONE}"
echo "  Scion:        ${VERSION}"
echo "  Auth mode:    dev (no IAP proxy yet)"
echo ""
echo "The VM has no public IP. To access the hub, create an SSH tunnel:"
echo ""
echo "  gcloud compute ssh ${INSTANCE_NAME} \\"
echo "    --zone=${ZONE} --project=${PROJECT_ID} \\"
echo "    -- -L 8080:localhost:8080"
echo ""
echo "Then open http://localhost:8080 in your browser."
echo ""
echo "To view service logs:"
echo ""
echo "  gcloud compute ssh ${INSTANCE_NAME} \\"
echo "    --zone=${ZONE} --project=${PROJECT_ID} \\"
echo "    --command='sudo journalctl -u scion-hub.service -f'"
