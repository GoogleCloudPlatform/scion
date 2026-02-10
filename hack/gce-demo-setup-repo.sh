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

# hack/gce-demo-setup-repo.sh - Setup SSH deploy key and clone the repo on GCE

set -euo pipefail

# Configuration
REPO="ptone/scion-agent"
INSTANCE_NAME="scion-demo"
ZONE="us-central1-a"
PROJECT_ID=$(gcloud config get-value project 2>/dev/null)

if [[ -z "$PROJECT_ID" ]]; then
    echo "Error: PROJECT_ID is not set and could not be determined from gcloud config."
    exit 1
fi

echo "=== Ensuring scion user and .ssh directory exist on VM ==="
gcloud compute ssh "${INSTANCE_NAME}" \
    --project="${PROJECT_ID}" \
    --zone="${ZONE}" \
    --command "
        sudo useradd -m -s /bin/bash scion || true
        sudo usermod -aG docker scion || true
        sudo mkdir -p /home/scion/.ssh
        sudo chown -R scion:scion /home/scion/.ssh
        sudo chmod 700 /home/scion/.ssh
    "

echo "=== Generating SSH Key on VM (if needed) ==="
gcloud compute ssh "${INSTANCE_NAME}" \
    --project="${PROJECT_ID}" \
    --zone="${ZONE}" \
    --command "
        sudo -u scion sh -c '
            if [ ! -f /home/scion/.ssh/id_ed25519 ]; then
                ssh-keygen -t ed25519 -N \"\" -f /home/scion/.ssh/id_ed25519 -C \"scion-demo-deploy-key\"
            fi
        '
    "

echo "=== Retrieving Public Key from VM ==="
PUB_KEY=$(gcloud compute ssh "${INSTANCE_NAME}" \
    --project="${PROJECT_ID}" \
    --zone="${ZONE}" \
    --command "sudo cat /home/scion/.ssh/id_ed25519.pub")

echo "=== Adding Deploy Key to GitHub Repo: ${REPO} ==="
# Create a local temp file for the public key to use with gh
TMP_PUB_KEY=$(mktemp)
echo "$PUB_KEY" > "$TMP_PUB_KEY"
trap 'rm -f "$TMP_PUB_KEY"' EXIT

# Add the deploy key (using --allow-write in case it needs to push back, though scion-agent usually doesn't need it on demo)
# We use a timestamp in the title to avoid collisions if re-run, or we could use a fixed title.
KEY_TITLE="scion-demo-$(date +%Y%m%d-%H%M%S)"
gh repo deploy-key add "$TMP_PUB_KEY" --repo "$REPO" --title "$KEY_TITLE" || echo "Key already exists or could not be added, continuing..."

echo "=== Cloning Repo on GCE Instance using SSH ==="
gcloud compute ssh "${INSTANCE_NAME}" \
    --project="${PROJECT_ID}" \
    --zone="${ZONE}" \
    --command "
        set -euo pipefail
        
        # Add github.com to known_hosts to avoid interactive prompt
        sudo -u scion sh -c 'ssh-keyscan github.com >> /home/scion/.ssh/known_hosts'
        
        if [ ! -d \"/home/scion/scion-agent\" ]; then
            echo \"Cloning git@github.com:${REPO}.git...\"
            sudo -u scion git clone \"git@github.com:${REPO}.git\" /home/scion/scion-agent
        else
            echo \"Directory /home/scion/scion-agent already exists, skipping clone.\"
        fi
        
        echo \"=== Repository Setup Complete ===\"
    "

echo "=== Success ==="

