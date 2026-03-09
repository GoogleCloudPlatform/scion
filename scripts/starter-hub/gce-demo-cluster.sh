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

# scripts/starter-hub/gce-demo-cluster.sh - Create or delete a GKE Autopilot cluster for Scion Demo

set -euo pipefail

CLUSTER_NAME="scion-demo-cluster"
REGION=${REGION:-"us-central1"}
PROJECT_ID=${PROJECT_ID:-"deploy-demo-test"}

function delete_resources() {
    echo "=== Deleting Scion Demo Cluster ==="
    
    if gcloud container clusters describe "${CLUSTER_NAME}" --region "${REGION}" --project "${PROJECT_ID}" &>/dev/null; then
        echo "Deleting cluster ${CLUSTER_NAME}..."
        gcloud container clusters delete "${CLUSTER_NAME}" --region "${REGION}" --project "${PROJECT_ID}" --quiet
    else
        echo "Cluster ${CLUSTER_NAME} not found."
    fi
    
    echo "=== Deletion Complete ==="
}

if [[ "${1:-}" == "delete" ]]; then
    delete_resources
    exit 0
fi

echo "=== Scion Demo Cluster Provisioning ==="
echo "Project: ${PROJECT_ID}"
echo "Cluster: ${CLUSTER_NAME}"
echo "Region:  ${REGION}"

# Enable GKE API
echo "Enabling GKE API..."
gcloud services enable container.googleapis.com --project "${PROJECT_ID}"

# Create the cluster if it doesn't exist
if ! gcloud container clusters describe "${CLUSTER_NAME}" --region "${REGION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
    echo "Creating GKE Autopilot cluster (this may take 10+ minutes)..."
    gcloud container clusters create-auto "${CLUSTER_NAME}" \
        --region "${REGION}" \
        --project "${PROJECT_ID}" \
        --release-channel "regular" \
        --labels=env=demo,project=scion,type=scion-demo-cluster
else
    echo "Cluster '${CLUSTER_NAME}' already exists."
fi

echo "=== Configuring kubectl authentication ==="
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
    --region "${REGION}" \
    --project "${PROJECT_ID}"

echo ""
echo "=== Success ==="
echo "You can now use 'kubectl' to interact with your GKE Autopilot cluster."
echo ""
echo "To delete this cluster, run: $0 delete"
