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

# scripts/starter-hub/gce-demo-provision.sh - Provision or delete a GCE VM for Scion Demo

set -euo pipefail

INSTANCE_NAME="scion-demo"
SERVICE_ACCOUNT_NAME="scion-demo-sa"
FIREWALL_RULE="scion-demo-allow-http-https"
REGION=${REGION:-"us-central1"}
ZONE=${ZONE:-"us-central1-a"}
PROJECT_ID=$(gcloud config get-value project 2>/dev/null)
CLOUD_INIT_FILE="scripts/starter-hub/gce-demo-cloud-init.yaml"

if [[ -z "$PROJECT_ID" ]]; then
    echo "Error: PROJECT_ID is not set and could not be determined from gcloud config."
    exit 1
fi

SERVICE_ACCOUNT_EMAIL="${SERVICE_ACCOUNT_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

function delete_resources() {
    echo "=== Deleting Scion Demo Resources ==="
    
    if gcloud compute instances describe "${INSTANCE_NAME}" --zone "${ZONE}" &>/dev/null; then
        echo "Deleting instance ${INSTANCE_NAME}..."
        gcloud compute instances delete "${INSTANCE_NAME}" --zone "${ZONE}" --quiet
    else
        echo "Instance ${INSTANCE_NAME} not found."
    fi

    if gcloud iam service-accounts describe "${SERVICE_ACCOUNT_EMAIL}" &>/dev/null; then
        echo "Deleting service account ${SERVICE_ACCOUNT_EMAIL}..."
        gcloud iam service-accounts delete "${SERVICE_ACCOUNT_EMAIL}" --quiet
    else
        echo "Service account ${SERVICE_ACCOUNT_EMAIL} not found."
    fi

    if gcloud compute firewall-rules describe "${FIREWALL_RULE}" &>/dev/null; then
        echo "Deleting firewall rule ${FIREWALL_RULE}..."
        gcloud compute firewall-rules delete "${FIREWALL_RULE}" --quiet
    else
        echo "Firewall rule ${FIREWALL_RULE} not found."
    fi

    # Optional Cluster deletion
    if [[ -f "scripts/starter-hub/gce-demo-cluster.sh" ]]; then
        ./scripts/starter-hub/gce-demo-cluster.sh delete
    fi

    # Telemetry SA deletion
    if [[ -f "scripts/starter-hub/gce-demo-telemetry-sa.sh" ]]; then
        ./scripts/starter-hub/gce-demo-telemetry-sa.sh delete
    fi

    echo "=== Deletion Complete ==="
}

if [[ "${1:-}" == "delete" ]]; then
    delete_resources
    exit 0
fi

echo "=== Scion Demo Provisioning ==="

# Enable required APIs
echo "Enabling required Google Cloud APIs..."
gcloud services enable \
    cloudtrace.googleapis.com \
    cloudmonitoring.googleapis.com \
    logging.googleapis.com \
    --project "${PROJECT_ID}"

# Prompt for size
if [[ -z "${SIZE_CHOICE:-}" ]]; then
    echo "Choose instance size:"
    echo "1) Small  (10s of agents)  - e2-standard-4 (4 vCPU, 16GB)"
    echo "2) Medium (100s of agents) - n2-standard-32 (32 vCPU, 128GB)"
    echo "3) Large  (~1000 agents)   - n2-standard-128 (128 vCPU, 512GB)"
    read -p "Select [1-3]: " SIZE_CHOICE
fi

case $SIZE_CHOICE in
    1) MACHINE_TYPE="e2-standard-4" ;;
    2) MACHINE_TYPE="n2-standard-32" ;;
    3) MACHINE_TYPE="n2-standard-128" ;;
    *) echo "Invalid choice: $SIZE_CHOICE"; exit 1 ;;
esac

echo "Selected Machine Type: ${MACHINE_TYPE}"

# Prompt for cluster
if [[ -z "${CREATE_CLUSTER:-}" ]]; then
    read -p "Create GKE cluster for agents? [y/N]: " CLUSTER_CHOICE
    if [[ "${CLUSTER_CHOICE,,}" == "y" ]]; then
        CREATE_CLUSTER="true"
    else
        CREATE_CLUSTER="false"
    fi
fi

# Create Service Account if it doesn't exist
if ! gcloud iam service-accounts describe "${SERVICE_ACCOUNT_EMAIL}" &>/dev/null; then
    echo "Creating service account ${SERVICE_ACCOUNT_NAME}..."
    gcloud iam service-accounts create "${SERVICE_ACCOUNT_NAME}" \
        --display-name "Scion Demo Service Account"
    
    echo "Waiting for service account to propagate..."
    sleep 10
else
    echo "Service account ${SERVICE_ACCOUNT_NAME} already exists."
fi

# Add/Ensure roles (Logging, Monitoring, Tracing, etc.)
echo "Adding/ensuring roles on service account..."
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member "serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
    --role "roles/logging.logWriter" > /dev/null
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member "serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
    --role "roles/monitoring.metricWriter" > /dev/null
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member "serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
    --role "roles/cloudtrace.agent" > /dev/null
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member "serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
    --role "roles/cloudsql.client" > /dev/null
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member "serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
    --role "roles/storage.objectAdmin" > /dev/null
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member "serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
    --role "roles/iam.serviceAccountTokenCreator" > /dev/null

# Also grant the service account token creator role on ITSELF - required for signBlob via metadata server
gcloud iam service-accounts add-iam-policy-binding "${SERVICE_ACCOUNT_EMAIL}" \
    --member "serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
    --role "roles/iam.serviceAccountTokenCreator" \
    --project "${PROJECT_ID}" > /dev/null

gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member "serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
    --role "roles/dns.admin" > /dev/null
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member "serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
    --role "roles/secretmanager.admin" > /dev/null
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member "serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
    --role "roles/container.admin" > /dev/null

# Create Firewall Rule if it doesn't exist
if ! gcloud compute firewall-rules describe "${FIREWALL_RULE}" &>/dev/null; then
    echo "Creating firewall rule ${FIREWALL_RULE}..."
    gcloud compute firewall-rules create "${FIREWALL_RULE}" \
        --allow=tcp:80,tcp:443 \
        --target-tags=https-server \
        --description="Allow HTTP and HTTPS traffic for Scion Demo"
else
    echo "Firewall rule ${FIREWALL_RULE} already exists."
fi

# Create Instance
echo "Creating GCE instance ${INSTANCE_NAME}..."
gcloud compute instances create "${INSTANCE_NAME}" \
    --project="${PROJECT_ID}" \
    --zone="${ZONE}" \
    --machine-type="${MACHINE_TYPE}" \
    --network-interface=network-tier=PREMIUM,subnet=default \
    --maintenance-policy=MIGRATE \
    --provisioning-model=STANDARD \
    --service-account="${SERVICE_ACCOUNT_EMAIL}" \
    --scopes=https://www.googleapis.com/auth/cloud-platform \
    --tags=https-server,scion-demo \
    --labels=env=demo,project=scion,type=scion-demo \
    --create-disk=auto-delete=yes,boot=yes,device-name=scion-demo,image=projects/ubuntu-os-cloud/global/images/family/ubuntu-2204-lts,mode=rw,size=200,type=projects/${PROJECT_ID}/zones/${ZONE}/diskTypes/pd-balanced \
    --metadata-from-file=user-data="${CLOUD_INIT_FILE}"

# Cluster creation
if [[ "${CREATE_CLUSTER:-}" == "true" ]]; then
    ./scripts/starter-hub/gce-demo-cluster.sh
fi

echo ""
echo "=== Success ==="
echo "Instance ${INSTANCE_NAME} is being provisioned."
echo "Cloud-init is running to install dependencies. This may take a few minutes."
echo "You can check progress by SSHing into the machine and running: tail -f /var/log/cloud-init-output.log"

echo ""
echo "To delete this deployment, run: $0 delete"
