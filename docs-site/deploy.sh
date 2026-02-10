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

set -e

# Pick up PROJECT_ID from env variable, but default to deploy-demo-test
PROJECT_ID=${PROJECT_ID:-duet01}
REGION=${REGION:-us-west1}
SERVICE_NAME=${SERVICE_NAME:-scion-docs}

echo "Deploying Scion Documentation Site..."
echo "Project ID: $PROJECT_ID"
echo "Region:     $REGION"
echo "Service:    $SERVICE_NAME"

# Submit to Cloud Build
# We pass the project explicitly to gcloud
# We calculate a short SHA for tagging if in a git repo
GIT_SHA=$(git rev-parse --short HEAD 2>/dev/null || echo "latest")
REGISTRY="${REGION}-docker.pkg.dev/${PROJECT_ID}/scion-images"

gcloud builds submit \
  --async \
  --project "$PROJECT_ID" \
  --config cloudbuild.yaml \
  --substitutions="_SERVICE_NAME=$SERVICE_NAME,_REGION=$REGION,_GIT_SHA=$GIT_SHA,_REGISTRY=$REGISTRY" \
  .
