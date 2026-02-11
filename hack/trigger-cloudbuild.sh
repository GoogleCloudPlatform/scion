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

# Trigger Cloud Build for scion images
# Usage: trigger-cloudbuild.sh [target]
#   target: all (default), core-base, scion-base, harnesses

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

TARGET="${1:-all}"
PROJECT="${PROJECT:-ptone-misc}"
SHORT_SHA=$(git rev-parse --short HEAD)
COMMIT_SHA=$(git rev-parse HEAD)

case "${TARGET}" in
  all)
    echo "Submitting full build (core-base -> scion-base -> harnesses) to Cloud Build..."
    CONFIG="image-build/cloudbuild.yaml"
    SUBSTITUTIONS="_SHORT_SHA=${SHORT_SHA},_COMMIT_SHA=${COMMIT_SHA}"
    ;;
  core-base)
    echo "Submitting core-base build to Cloud Build..."
    CONFIG="image-build/cloudbuild-core-base.yaml"
    SUBSTITUTIONS="_SHORT_SHA=${SHORT_SHA}"
    ;;
  scion-base)
    echo "Submitting scion-base build to Cloud Build..."
    CONFIG="image-build/cloudbuild-scion-base.yaml"
    SUBSTITUTIONS="_SHORT_SHA=${SHORT_SHA},_COMMIT_SHA=${COMMIT_SHA}"
    ;;
  harnesses)
    echo "Submitting harnesses build to Cloud Build..."
    CONFIG="image-build/cloudbuild-harnesses.yaml"
    SUBSTITUTIONS="_SHORT_SHA=${SHORT_SHA}"
    ;;
  *)
    echo "Unknown target: ${TARGET}"
    echo "Usage: trigger-cloudbuild.sh [all|core-base|scion-base|harnesses]"
    echo ""
    echo "Targets:"
    echo "  all         - Full rebuild of all images (default)"
    echo "  core-base   - Build only core-base (foundation tools)"
    echo "  scion-base  - Build only scion-base (uses existing core-base:latest)"
    echo "  harnesses   - Build only harnesses (uses existing scion-base:latest)"
    exit 1
    ;;
esac

gcloud builds submit --async \
  --project="${PROJECT}" \
  --substitutions="${SUBSTITUTIONS}" \
  --config="${CONFIG}" .

echo ""
echo "Build submitted. View progress at:"
echo "  https://console.cloud.google.com/cloud-build/builds?project=${PROJECT}"
