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

# hack/create-cluster.sh - Create a GKE Autopilot cluster and configure kubectl
# This script creates a GKE Autopilot cluster in the current gcloud project
# and configures the local kubectl context to use it.

set -euo pipefail

# Configurable variables with defaults
CLUSTER_NAME=${CLUSTER_NAME:-"scion-agents"}
REGION=${REGION:-"us-central1"}
PROJECT_ID=${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null)}

if [[ -z "$PROJECT_ID" ]]; then
    echo "Error: PROJECT_ID is not set and could not be determined from gcloud config."
    exit 1
fi

echo "=== Creating GKE Autopilot Cluster ==="
echo "Cluster Name: ${CLUSTER_NAME}"
echo "Region:       ${REGION}"
echo "Project:      ${PROJECT_ID}"

# Create the cluster if it doesn't exist
if ! gcloud container clusters describe "${CLUSTER_NAME}" --region "${REGION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
    echo "Creating cluster (this may take several minutes)..."
    gcloud container clusters create-auto "${CLUSTER_NAME}" \
        --region "${REGION}" \
        --project "${PROJECT_ID}"
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
