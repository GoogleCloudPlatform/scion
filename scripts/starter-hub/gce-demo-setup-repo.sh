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

# scripts/starter-hub/gce-demo-setup-repo.sh - Clone the public repo on GCE

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hub-config.sh
source "${SCRIPT_DIR}/hub-config.sh"

REPO="${GITHUB_REPO}"

if [[ -z "$PROJECT_ID" ]]; then
    echo "Error: PROJECT_ID is not set and could not be determined from gcloud config."
    exit 1
fi

require_non_service_ssh_user

wait_for_cloud_init

echo "=== Ensuring scion user exists on VM ==="
gcloud compute ssh "${SSH_TARGET}" \
    --project="${PROJECT_ID}" \
    --zone="${ZONE}" \
    --command "
        if ! id scion >/dev/null 2>&1; then
            sudo useradd -m -s /bin/bash scion
            echo '  -> Created scion user'
        else
            echo '  -> scion user already exists'
        fi
        if id -nG scion 2>/dev/null | tr ' ' '\n' | grep -qx 'google-sudoers'; then
            echo 'Error: hub service user \"scion\" is in the google-sudoers group (passwordless root).' >&2
            echo '  Do not use \"scion\" as an SSH or OS Login username on the Hub VM, and do not' >&2
            echo '  include a \"scion\" key in project or instance SSH metadata.' >&2
            echo '  To remediate on the VM: sudo gpasswd -d scion google-sudoers' >&2
            echo '  Then remove any \"scion\" SSH key from GCP metadata and rerun with a different SSH_USER.' >&2
            exit 1
        fi
        if getent group docker >/dev/null 2>&1; then
            sudo usermod -aG docker scion
            echo '  -> Added scion to docker group'
        else
            echo '  -> docker group does not exist yet (cloud-init may still be running), skipping'
        fi
    "

echo "=== Cloning Repo on GCE Instance ==="
gcloud compute ssh "${SSH_TARGET}" \
    --project="${PROJECT_ID}" \
    --zone="${ZONE}" \
    --command "
        set -euo pipefail

        CLONE_URL=\"https://github.com/${REPO}.git\"

        if sudo -u scion git -C /home/scion/scion rev-parse --git-dir >/dev/null 2>&1; then
            echo \"Repository /home/scion/scion already exists, fetching latest...\"
            sudo -u scion sh -c 'cd /home/scion/scion && git fetch origin && git reset --hard origin/HEAD'
        else
            if sudo test -e \"/home/scion/scion\"; then
                echo \"Removing existing non-git path /home/scion/scion...\"
                sudo rm -rf /home/scion/scion
            fi
            echo \"Cloning \${CLONE_URL}...\"
            sudo -u scion git clone \"\${CLONE_URL}\" /home/scion/scion
        fi

        echo \"=== Repository Setup Complete ===\"
    "

echo "=== Success ==="

