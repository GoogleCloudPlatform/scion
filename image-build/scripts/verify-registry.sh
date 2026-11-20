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

# Pre-flight check for Artifact Registry push access.
#
# Verifies that the target AR repository exists and the active service
# account has permission to push images. Intended to run as an early
# Cloud Build step so IAM errors are caught in seconds instead of after
# a long multi-arch build.
#
# Usage:
#   verify-registry.sh <registry>
#
# Example:
#   verify-registry.sh us-docker.pkg.dev/my-project/my-repo

set -euo pipefail

REGISTRY="${1:?Usage: verify-registry.sh <registry>}"

# Parse registry: <host>/<project>/<repo>
reg_host=$(echo "$REGISTRY" | cut -d/ -f1)
reg_project=$(echo "$REGISTRY" | cut -d/ -f2)
reg_repo=$(echo "$REGISTRY" | cut -d/ -f3)
reg_location="${reg_host%-docker.pkg.dev}"

echo "Pre-flight: checking Artifact Registry access..."
echo "  registry=$REGISTRY (project=$reg_project location=$reg_location repo=$reg_repo)"

# 1. Verify the Artifact Registry repository exists and is readable.
describe_err_file="$(mktemp "${TMPDIR:-/tmp}/verify-registry-err.XXXXXX")"
describe_rc=0
gcloud artifacts repositories describe "$reg_repo" \
    --location="$reg_location" --project="$reg_project" \
    --format="value(name)" >/dev/null 2>"$describe_err_file" || describe_rc=$?

if [[ $describe_rc -ne 0 ]]; then
  describe_err="$(cat "$describe_err_file")"
  rm -f -- "$describe_err_file"
  echo "" >&2
  if grep -qiE 'PERMISSION_DENIED|HTTPError 40[13]|unauthori[sz]ed|forbidden|does not have permission|permission .* denied|credential' <<<"$describe_err"; then
    printf '%s\n' "$describe_err" >&2
    echo "ERROR: Permission denied accessing Artifact Registry repository: $REGISTRY" >&2
    echo "The active service account lacks artifactregistry.repositories.get (or newly bound IAM policies have not yet propagated)." >&2
    echo "Grant access with:" >&2
    echo "  image-build/scripts/setup-cloud-build.sh --project $reg_project --location $reg_location --repo $reg_repo" >&2
  elif grep -qiE 'NOT_FOUND|was not found|does not exist' <<<"$describe_err"; then
    echo "ERROR: Artifact Registry repository not found: $REGISTRY" >&2
    echo "Create it and grant push access with:" >&2
    echo "  image-build/scripts/setup-cloud-build.sh --project $reg_project --location $reg_location --repo $reg_repo" >&2
  else
    printf '%s\n' "$describe_err" >&2
    echo "ERROR: Failed to describe Artifact Registry repository: $REGISTRY" >&2
  fi
  exit 1
fi
rm -f -- "$describe_err_file"

# 2. Verify push permission via the Artifact Registry testIamPermissions API.
token=$(gcloud auth print-access-token)
parent="projects/$reg_project/locations/$reg_location/repositories/$reg_repo"
response=$(curl -sf -X POST \
  "https://artifactregistry.googleapis.com/v1/$parent:testIamPermissions" \
  -H "Authorization: Bearer $token" \
  -H "Content-Type: application/json" \
  -d '{"permissions":["artifactregistry.repositories.uploadArtifacts"]}' 2>/dev/null || echo "{}")

if ! echo "$response" | grep -q "uploadArtifacts"; then
  echo "" >&2
  echo "ERROR: Missing push permission on $REGISTRY" >&2
  echo "The active service account cannot upload artifacts to this repository." >&2
  echo "Grant access with:" >&2
  echo "  image-build/scripts/setup-cloud-build.sh --project $reg_project --location $reg_location --repo $reg_repo" >&2
  exit 1
fi

echo "Registry access verified: $REGISTRY"
