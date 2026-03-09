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

# scripts/starter-hub/gce-demo-telemetry-sa.sh - Create GCP service account for agent telemetry export
#
# Creates a dedicated, least-privilege service account for writing telemetry
# data (traces, logs, metrics) to Google Cloud Observability. A JSON key is
# downloaded locally for injection into agent containers via the Hub secrets
# system.
#
# Usage:
#   ./scripts/starter-hub/gce-demo-telemetry-sa.sh          # Create SA, grant roles, download key
#   ./scripts/starter-hub/gce-demo-telemetry-sa.sh delete    # Remove SA and local key file
#
# The key file is written to .scratch/telemetry-gcp-credentials.json
# (git-ignored). It should be uploaded to the Hub as a file-type secret named
# "scion-telemetry-gcp-credentials" with target "~/.scion/telemetry-gcp-credentials.json".

set -euo pipefail

SA_NAME="scion-telemetry-writer"
PROJECT_ID=${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null)}
KEY_DIR=".scratch"
KEY_FILE="${KEY_DIR}/telemetry-gcp-credentials.json"

if [[ -z "$PROJECT_ID" ]]; then
    echo "Error: PROJECT_ID is not set and could not be determined from gcloud config."
    exit 1
fi

SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

# --- IAM roles: write-only telemetry access (see .design/hosted/metrics-design-gcp-auth.md §3.2) ---
ROLES=(
    "roles/cloudtrace.agent"
    "roles/logging.logWriter"
    "roles/monitoring.metricWriter"
)

function delete_resources() {
    echo "=== Deleting Telemetry Service Account ==="

    # Remove IAM bindings
    for role in "${ROLES[@]}"; do
        if gcloud projects get-iam-policy "${PROJECT_ID}" \
            --flatten="bindings[].members" \
            --filter="bindings.members:serviceAccount:${SA_EMAIL} AND bindings.role:${role}" \
            --format="value(bindings.role)" 2>/dev/null | grep -q "${role}"; then
            echo "Removing ${role}..."
            gcloud projects remove-iam-policy-binding "${PROJECT_ID}" \
                --member "serviceAccount:${SA_EMAIL}" \
                --role "${role}" > /dev/null
        fi
    done

    # Delete service account
    if gcloud iam service-accounts describe "${SA_EMAIL}" &>/dev/null; then
        echo "Deleting service account ${SA_EMAIL}..."
        gcloud iam service-accounts delete "${SA_EMAIL}" --quiet
    else
        echo "Service account ${SA_EMAIL} not found."
    fi

    # Remove local key file
    if [[ -f "${KEY_FILE}" ]]; then
        echo "Removing local key file ${KEY_FILE}..."
        rm -f "${KEY_FILE}"
    fi

    echo "=== Deletion Complete ==="
}

if [[ "${1:-}" == "delete" ]]; then
    delete_resources
    exit 0
fi

echo "=== Telemetry Service Account Setup ==="
echo "Project:         ${PROJECT_ID}"
echo "Service Account: ${SA_NAME}"
echo "Key File:        ${KEY_FILE}"
echo ""

# Enable required APIs
echo "Enabling required Google Cloud APIs..."
gcloud services enable \
    cloudtrace.googleapis.com \
    logging.googleapis.com \
    monitoring.googleapis.com \
    --project "${PROJECT_ID}"

# Create service account if it doesn't exist
if ! gcloud iam service-accounts describe "${SA_EMAIL}" &>/dev/null; then
    echo "Creating service account ${SA_NAME}..."
    gcloud iam service-accounts create "${SA_NAME}" \
        --display-name "Scion Telemetry Writer" \
        --description "Least-privilege SA for agent telemetry export (traces, logs, metrics)"

    echo "Waiting for service account to propagate..."
    sleep 5
else
    echo "Service account ${SA_NAME} already exists."
fi

# Grant IAM roles
echo "Granting IAM roles..."
for role in "${ROLES[@]}"; do
    echo "  -> ${role}"
    gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
        --member "serviceAccount:${SA_EMAIL}" \
        --role "${role}" > /dev/null
done

# Create and download key
mkdir -p "${KEY_DIR}"

if [[ -f "${KEY_FILE}" ]]; then
    echo ""
    echo "Key file ${KEY_FILE} already exists."
    echo "To regenerate, delete it first and re-run this script."
else
    echo ""
    echo "Creating and downloading service account key..."
    gcloud iam service-accounts keys create "${KEY_FILE}" \
        --iam-account "${SA_EMAIL}"
    chmod 600 "${KEY_FILE}"
    echo "Key saved to ${KEY_FILE} (mode 0600)"
fi

echo ""
echo "=== Success ==="
echo ""
echo "Next steps:"
echo "  1. Upload the key to the Hub as a file-type secret:"
echo ""
echo "     scion hub secret set scion-telemetry-gcp-credentials \\"
echo "       @${KEY_FILE} \\"
echo "       --hub '<HUB_ENDPOINT_URL>' \\"
echo "       --scope hub \\"
echo "       --type file \\"
echo "       --target '~/.scion/telemetry-gcp-credentials.json'"
echo ""
echo "  2. Ensure grove settings include 'provider: gcp' under telemetry.cloud"
echo ""
echo "  To delete this SA, run: $0 delete"
