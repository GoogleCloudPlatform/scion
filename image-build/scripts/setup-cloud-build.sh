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

set -euo pipefail

# One-time setup for building Scion images with Google Cloud Build.
#
# This script:
#   1. Enables required GCP APIs (Cloud Build, Artifact Registry).
#   2. Creates an Artifact Registry Docker repository if it doesn't exist.
#   3. Grants Cloud Build service account permission to push to the repository.
#   4. Prints the registry path for use with build-images.sh or trigger-cloudbuild.sh.
#
# Usage:
#   setup-cloud-build.sh [--project <project>] [--location <location>] [--repo <repo-name>]

PROJECT="genai-437003"
LOCATION="us-central1"
REPO_NAME="scion-images"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)  PROJECT="$2"; shift 2 ;;
    --location) LOCATION="$2"; shift 2 ;;
    --repo)     REPO_NAME="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: $(basename "$0") [--project <project>] [--location <location>] [--repo <repo-name>]"
      echo ""
      echo "Options:"
      echo "  --project <project>    GCP project (default: \$GCLOUD_PROJECT or gcloud config)"
      echo "  --location <location>  Artifact Registry location (default: us-central1)"
      echo "  --repo <repo-name>     Repository name (default: scion)"
      exit 0
      ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

# Resolve project
if [[ -z "${PROJECT}" ]]; then
  PROJECT="${GCLOUD_PROJECT:-}"
fi
if [[ -z "${PROJECT}" ]]; then
  PROJECT="$(gcloud config get-value project 2>/dev/null)" || true
fi
if [[ -z "${PROJECT}" ]]; then
  echo "Error: Could not determine GCP project."
  echo "Set --project, \$GCLOUD_PROJECT, or run 'gcloud config set project <project>'."
  exit 1
fi

REGISTRY="${LOCATION}-docker.pkg.dev/${PROJECT}/${REPO_NAME}"

echo "Setting up Cloud Build for Scion images"
echo "  Project:  ${PROJECT}"
echo "  Location: ${LOCATION}"
echo "  Repo:     ${REPO_NAME}"
echo "  Registry: ${REGISTRY}"
echo ""

# 1. Enable required APIs
echo "==> Enabling required APIs..."
gcloud services enable \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  --project="${PROJECT}"

# 2. Create Artifact Registry repository
echo "==> Creating Artifact Registry repository '${REPO_NAME}'..."
if gcloud artifacts repositories describe "${REPO_NAME}" \
    --location="${LOCATION}" \
    --project="${PROJECT}" &>/dev/null; then
  echo "    Repository already exists."
else
  gcloud artifacts repositories create "${REPO_NAME}" \
    --repository-format=docker \
    --location="${LOCATION}" \
    --project="${PROJECT}" \
    --description="Scion container images"
  echo "    Repository created."
fi

# 3. Grant Cloud Build permission to push
echo "==> Granting Cloud Build access to Artifact Registry..."
PROJECT_NUMBER="$(gcloud projects describe "${PROJECT}" --format='value(projectNumber)')"
CB_SA="${PROJECT_NUMBER}@cloudbuild.gserviceaccount.com"

gcloud artifacts repositories add-iam-policy-binding "${REPO_NAME}" \
  --location="${LOCATION}" \
  --project="${PROJECT}" \
  --member="serviceAccount:${CB_SA}" \
  --role="roles/artifactregistry.writer" \
  --quiet

echo "    Granted artifactregistry.writer to ${CB_SA}."

gcloud artifacts repositories add-iam-policy-binding "${REPO_NAME}" \
  --location="${LOCATION}" \
  --project="${PROJECT}" \
  --member="serviceAccount:${CB_SA}" \
  --role="roles/artifactregistry.reader" \
  --quiet

echo "    Granted artifactregistry.reader to ${CB_SA}."


echo ""
echo "Setup complete! Registry path:"
echo "  ${REGISTRY}"
echo ""
echo "To build images with Cloud Build:"
echo "  image-build/scripts/trigger-cloudbuild.sh --project ${PROJECT} --registry ${REGISTRY}"
echo ""
echo "To build images locally:"
echo "  image-build/scripts/build-images.sh --registry ${REGISTRY} --push"
echo ""
echo "To configure scion to use these images:"
echo "  scion config set image_registry ${REGISTRY}"
