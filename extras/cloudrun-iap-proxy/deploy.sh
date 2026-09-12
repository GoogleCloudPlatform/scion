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

# extras/cloudrun-iap-proxy/deploy.sh - Deploy Go IAP Reverse Proxy to Cloud Run

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Configuration with defaults
PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
REGION="${REGION:-us-central1}"
SERVICE_NAME="${SERVICE_NAME:-scion-hub-iap-proxy}"
TARGET_URL="${TARGET_URL:-}"
VPC_NETWORK="${VPC_NETWORK:-default}"
VPC_SUBNET="${VPC_SUBNET:-default}"
ALLOW_UNAUTHENTICATED="${ALLOW_UNAUTHENTICATED:-true}"

usage() {
    echo "Usage: TARGET_URL=<target-url> [PROJECT_ID=<project-id>] [REGION=<region>] [SERVICE_NAME=<service-name>] $0"
    echo ""
    echo "Environment Variables:"
    echo "  TARGET_URL            Target backend URL to proxy to (e.g., http://10.128.0.2:8080) [Required]"
    echo "  PROJECT_ID            GCP project ID (default: current gcloud project: '${PROJECT_ID}')"
    echo "  REGION                Cloud Run region (default: '${REGION}')"
    echo "  SERVICE_NAME          Cloud Run service name (default: '${SERVICE_NAME}')"
    echo "  VPC_NETWORK           VPC network for Direct VPC Egress (default: '${VPC_NETWORK}')"
    echo "  VPC_SUBNET            VPC subnetwork for Direct VPC Egress (default: '${VPC_SUBNET}')"
    echo "  ALLOW_UNAUTHENTICATED Allow unauthenticated invocation (default: '${ALLOW_UNAUTHENTICATED}')"
    exit 1
}

if [[ -z "${TARGET_URL}" ]]; then
    echo "ERROR: TARGET_URL is required." >&2
    usage
fi

if [[ -z "${PROJECT_ID}" ]]; then
    echo "ERROR: PROJECT_ID is required (set PROJECT_ID or configure active gcloud project)." >&2
    usage
fi

echo "=== Deploying Cloud Run IAP Proxy ==="
echo "Project:      ${PROJECT_ID}"
echo "Region:       ${REGION}"
echo "Service Name: ${SERVICE_NAME}"
echo "Target URL:   ${TARGET_URL}"
echo "VPC Network:  ${VPC_NETWORK}"
echo "VPC Subnet:   ${VPC_SUBNET}"

AUTH_FLAG="--allow-unauthenticated"
if [[ "${ALLOW_UNAUTHENTICATED}" == "false" ]]; then
    AUTH_FLAG="--no-allow-unauthenticated"
fi

# Deploy Cloud Run service with Direct VPC Egress
gcloud run deploy "${SERVICE_NAME}" \
    --project="${PROJECT_ID}" \
    --region="${REGION}" \
    --source="${SCRIPT_DIR}" \
    --set-env-vars="TARGET_URL=${TARGET_URL}" \
    --network="${VPC_NETWORK}" \
    --subnet="${VPC_SUBNET}" \
    --vpc-egress=all-traffic \
    ${AUTH_FLAG} \
    --port=8080

URL=$(gcloud run services describe "${SERVICE_NAME}" --project="${PROJECT_ID}" --region="${REGION}" --format="value(status.url)")

echo ""
echo "=== Deployment Successful ==="
echo "Cloud Run URL: ${URL}"
echo ""
echo "Testing Cloud Run proxy connection..."
curl -s "${URL}/proxy-healthz" || true
echo ""
