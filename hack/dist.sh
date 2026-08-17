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

# hack/dist.sh - Build and distribute the scion CLI binary
#
# Usage:
#   ./hack/dist.sh set-up    # Create bucket, configure IAM, upload installer
#   ./hack/dist.sh publish   # Build and upload cross-platform binaries
#
# End users can then install scion via:
#   gcloud storage cat gs://scion-dist-<PROJECT_ID>/install.sh | bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PROJECT_ID=$(gcloud config get-value project 2>/dev/null)
if [[ -z "$PROJECT_ID" ]]; then
    echo "Error: PROJECT_ID could not be determined from gcloud config."
    exit 1
fi

BUCKET_NAME="scion-dist-${PROJECT_ID}"

# Platform matrix: GOOS/GOARCH pairs
PLATFORMS=(
    "darwin/arm64"
    "darwin/amd64"
    "linux/arm64"
    "linux/amd64"
    # "windows/arm64"   # TODO: enable once Windows build issues are resolved
    # "windows/amd64"   # TODO: enable once Windows build issues are resolved
)

function setup() {
    echo "=== Setting up distribution bucket ==="

    # Create the bucket if it doesn't exist
    if gsutil ls -b "gs://${BUCKET_NAME}" &>/dev/null; then
        echo "Bucket gs://${BUCKET_NAME} already exists."
    else
        echo "Creating bucket gs://${BUCKET_NAME}..."
        gsutil mb -p "${PROJECT_ID}" "gs://${BUCKET_NAME}"
    fi

    # Set IAM policy to allow google.com domain read access
    echo "Setting IAM policy for google.com domain read access..."
    gsutil iam ch "domain:google.com:objectViewer" "gs://${BUCKET_NAME}"

    # Upload install.sh to bucket root with the bucket name baked in
    echo "Uploading install.sh to bucket root..."
    local install_src="${SCRIPT_DIR}/install.sh"
    local install_tmp
    install_tmp=$(mktemp)
    sed "s|__BUCKET_NAME__|${BUCKET_NAME}|g" "${install_src}" > "${install_tmp}"
    gsutil -q cp "${install_tmp}" "gs://${BUCKET_NAME}/install.sh"
    rm -f "${install_tmp}"

    echo "=== Setup complete ==="
    echo "Bucket: gs://${BUCKET_NAME}"
    echo ""
    echo "Users can install scion with:"
    echo "  gcloud storage cat gs://${BUCKET_NAME}/install.sh | bash"
}

function publish() {
    echo "=== Publishing scion binaries ==="

    SHORT_HASH=$(git -C "${PROJECT_ROOT}" rev-parse --short HEAD)
    LDFLAGS=$(bash "${SCRIPT_DIR}/version.sh")
    BUILD_DIR=$(mktemp -d)
    trap '[ -n "${BUILD_DIR:-}" ] && rm -rf "${BUILD_DIR}"' EXIT

    echo "Commit: ${SHORT_HASH}"
    echo "Build dir: ${BUILD_DIR}"

    for platform in "${PLATFORMS[@]}"; do
        GOOS="${platform%/*}"
        GOARCH="${platform##*/}"
        PLATFORM_DIR="${GOOS}-${GOARCH}"

        BINARY_NAME="scion"
        if [[ "$GOOS" == "windows" ]]; then
            BINARY_NAME="scion.exe"
        fi

        echo "Building ${GOOS}/${GOARCH}..."
        GOOS="${GOOS}" GOARCH="${GOARCH}" CGO_ENABLED=0 \
            go build -buildvcs=false -ldflags "${LDFLAGS}" \
            -o "${BUILD_DIR}/${PLATFORM_DIR}/${BINARY_NAME}" \
            "${PROJECT_ROOT}/cmd/scion"

        # Create zip archive
        ZIP_FILE="${BUILD_DIR}/${PLATFORM_DIR}/scion.zip"
        (cd "${BUILD_DIR}/${PLATFORM_DIR}" && zip -q scion.zip "${BINARY_NAME}")

        # Upload to latest/<platform>/scion.zip
        echo "  Uploading to latest/${PLATFORM_DIR}/scion.zip..."
        gsutil -q cp "${ZIP_FILE}" "gs://${BUCKET_NAME}/latest/${PLATFORM_DIR}/scion.zip"

        # Upload to versions/<platform>/scion-<hash>.zip
        echo "  Uploading to versions/${PLATFORM_DIR}/scion-${SHORT_HASH}.zip..."
        gsutil -q cp "${ZIP_FILE}" "gs://${BUCKET_NAME}/versions/${PLATFORM_DIR}/scion-${SHORT_HASH}.zip"
    done

    echo ""
    echo "=== Publish complete ==="
    echo "Commit: ${SHORT_HASH}"
    echo "Bucket: gs://${BUCKET_NAME}"
    echo ""
    echo "Latest binaries:"
    for platform in "${PLATFORMS[@]}"; do
        GOOS="${platform%/*}"
        GOARCH="${platform##*/}"
        echo "  gs://${BUCKET_NAME}/latest/${GOOS}-${GOARCH}/scion.zip"
    done
}

function usage() {
    echo "Usage: $0 {set-up|publish}"
    echo ""
    echo "Commands:"
    echo "  set-up    Create the GCS distribution bucket and configure IAM"
    echo "  publish   Build cross-platform binaries and upload to GCS"
    exit 1
}

case "${1:-}" in
    set-up)  setup ;;
    publish) publish ;;
    *)       usage ;;
esac
