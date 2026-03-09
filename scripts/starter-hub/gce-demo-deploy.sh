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

# scripts/starter-hub/gce-demo-deploy.sh - One-stop deployment for the Scion Demo Hub

set -euo pipefail

echo "=== Scion Demo Full Deployment ==="

# Step 1: Provision Infrastructure
echo ""
echo "--- Step 1: Provisioning Infrastructure ---"
./scripts/starter-hub/gce-demo-provision.sh

# Step 2: Telemetry Service Account
echo ""
echo "--- Step 2: Creating Telemetry Service Account ---"
./scripts/starter-hub/gce-demo-telemetry-sa.sh

# Step 3: Setup Repository
echo ""
echo "--- Step 3: Setting up Repository ---"
./scripts/starter-hub/gce-demo-setup-repo.sh

# Step 4: Build and Start Hub
echo ""
echo "--- Step 4: Building and Starting Hub ---"
./scripts/starter-hub/gce-start-hub.sh --full

echo ""
echo "=== Full Deployment Complete ==="
echo "Your Scion Hub should now be available at https://hub.demo.scion-ai.dev"
echo ""
echo "Note: To enable agent telemetry, upload the GCP credentials key to the Hub:"
echo "  scion secret set scion-telemetry-gcp-credentials \\"
echo "    --type file --target '~/.scion/telemetry-gcp-credentials.json' \\"
echo "    --from-file '.scratch/telemetry-gcp-credentials.json' --scope hub"
