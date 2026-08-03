#!/bin/bash
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

# scripts/starter-hub/hub-config.sh - Shared configuration for all starter-hub scripts
#
# Set HUB_NAME before sourcing this file to parameterize all scripts for a
# specific deployment. For example:
#
#   export HUB_NAME=staging
#   ./scripts/starter-hub/gce-demo-deploy.sh
#
# All resource names, domains, and file paths are derived from HUB_NAME and
# BASE_DOMAIN. Override any individual variable via the environment if the
# derived default doesn't fit your setup.

# --- Primary Configuration ---
# HUB_NAME drives all resource naming. Defaults to "demo".
HUB_NAME="${HUB_NAME:-demo}"
BASE_DOMAIN="${BASE_DOMAIN:-scion-ai.dev}"

# --- Feature Flags ---
# Set to "false" to skip GKE cluster creation, credential setup, and
# container.admin IAM role. The hub will run with Docker as the default runtime.
ENABLE_GKE="${ENABLE_GKE:-false}"

# --- Derived: GCP Resources ---
INSTANCE_NAME="${INSTANCE_NAME:-scion-${HUB_NAME}}"
SERVICE_ACCOUNT_NAME="${SERVICE_ACCOUNT_NAME:-scion-${HUB_NAME}-sa}"
FIREWALL_RULE="${FIREWALL_RULE:-scion-${HUB_NAME}-allow-http-https}"
CLUSTER_NAME="${CLUSTER_NAME:-scion-${HUB_NAME}-cluster}"

# --- Derived: Domain & DNS ---
# CERT_DOMAIN is the zone used for wildcard certs (e.g., "demo.scion-ai.dev")
CERT_DOMAIN="${CERT_DOMAIN:-${HUB_NAME}.${BASE_DOMAIN}}"
# HUB_DOMAIN is the full hostname for the hub (e.g., "hub.demo.scion-ai.dev")
HUB_DOMAIN="${HUB_DOMAIN:-hub.${CERT_DOMAIN}}"
# DNS_ZONE_NAME is the Cloud DNS managed zone name (e.g., "demo-scion-ai-dev")
DNS_ZONE_NAME="${DNS_ZONE_NAME:-$(echo "${CERT_DOMAIN}" | tr '.' '-')}"

# --- Derived: Region / Zone ---
REGION="${REGION:-us-central1}"
ZONE="${ZONE:-us-central1-a}"

# --- Derived: Project ---
PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
DNS_PROJECT_ID="${DNS_PROJECT_ID:-${PROJECT_ID}}"

# --- Derived: Paths ---
HUB_ENV_FILE="${HUB_ENV_FILE:-.scratch/hub-${HUB_NAME}.env}"
REPO_DIR="${REPO_DIR:-/home/scion/scion}"
SCION_BIN="${SCION_BIN:-/usr/local/bin/scion}"

# --- Shared Defaults ---
GITHUB_REPO="${GITHUB_REPO:-GoogleCloudPlatform/scion}"
CERT_EMAIL="${CERT_EMAIL:-ptone@google.com}"
CLOUD_INIT_FILE="${CLOUD_INIT_FILE:-scripts/starter-hub/gce-demo-cloud-init.yaml}"

# --- Shared Helpers ---

# Wait for the instance to be reachable via SSH and for cloud-init to finish.
# Call this before the first SSH-dependent step after provisioning.
wait_for_cloud_init() {
    echo "=== Waiting for VM to be ready (SSH + cloud-init) ==="
    local max_wait=600  # 10 minutes
    local interval=15
    local elapsed=0

    while (( elapsed < max_wait )); do
        local result
        result=$(gcloud compute ssh "${INSTANCE_NAME}" \
            --project="${PROJECT_ID}" \
            --zone="${ZONE}" \
            --ssh-flag="-o ConnectTimeout=10" \
            --command "cloud-init status 2>/dev/null || echo 'status: unknown'" \
            2>/dev/null) || result="SSH_UNREACHABLE"

        if [[ "$result" == "SSH_UNREACHABLE" ]]; then
            echo "  -> SSH not available yet... (${elapsed}s elapsed)"
        elif echo "$result" | grep -q "status: done"; then
            echo "  -> VM ready: cloud-init complete (${elapsed}s elapsed)"
            return 0
        elif echo "$result" | grep -q "status: error"; then
            echo "  -> Warning: cloud-init finished with errors (${elapsed}s elapsed)"
            echo "     Check: sudo cat /var/log/cloud-init-output.log"
            return 0
        else
            local status_text
            status_text=$(echo "$result" | head -1)
            echo "  -> SSH available, cloud-init: ${status_text} (${elapsed}s elapsed)"
        fi

        sleep "$interval"
        elapsed=$(( elapsed + interval ))
    done

    echo "Error: VM did not become ready after ${max_wait}s"
    return 1
}
