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

# Scion CLI installer
#
# Usage:
#   gcloud storage cat gs://BUCKET_NAME/install.sh | bash
#
# Environment variables:
#   SCION_INSTALL_DIR  - Override install directory (default: auto-detected)
#   SCION_VERSION      - Install a specific version hash (default: latest)

set -euo pipefail

# --- Platform detection ---

detect_platform() {
    local os arch

    case "$(uname -s)" in
        Darwin)  os="darwin" ;;
        Linux)   os="linux" ;;
        MINGW*|MSYS*|CYGWIN*)
            echo "Error: Windows is not currently supported." >&2
            exit 1
            ;;
        *)
            echo "Error: unsupported operating system: $(uname -s)" >&2
            exit 1
            ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64)   arch="amd64" ;;
        arm64|aarch64)  arch="arm64" ;;
        *)
            echo "Error: unsupported architecture: $(uname -m)" >&2
            exit 1
            ;;
    esac

    echo "${os}-${arch}"
}

# --- Install directory resolution ---

detect_install_dir() {
    if [[ -n "${SCION_INSTALL_DIR:-}" ]]; then
        echo "${SCION_INSTALL_DIR}"
        return
    fi

    # Prefer /usr/local/bin if writable, otherwise fall back to ~/.local/bin
    if [[ -d /usr/local/bin && -w /usr/local/bin ]]; then
        echo "/usr/local/bin"
    else
        local user_bin="${HOME}/.local/bin"
        mkdir -p "${user_bin}"
        echo "${user_bin}"
    fi
}

# --- Main ---

PLATFORM=$(detect_platform)
INSTALL_DIR=$(detect_install_dir)
VERSION="${SCION_VERSION:-latest}"
TMPDIR=$(mktemp -d)
trap '[ -n "${TMPDIR:-}" ] && rm -rf "${TMPDIR}"' EXIT

echo "Scion CLI Installer"
echo "  Platform:  ${PLATFORM}"
echo "  Install:   ${INSTALL_DIR}"
echo "  Version:   ${VERSION}"
echo ""

# Determine GCS path
# The bucket name is embedded by the dist set-up process.
BUCKET_NAME="__BUCKET_NAME__"

if [[ "$VERSION" == "latest" ]]; then
    ZIP_PATH="gs://${BUCKET_NAME}/latest/${PLATFORM}/scion.zip"
else
    ZIP_PATH="gs://${BUCKET_NAME}/versions/${PLATFORM}/scion-${VERSION}.zip"
fi

echo "Downloading ${ZIP_PATH}..."
gcloud storage cp "${ZIP_PATH}" "${TMPDIR}/scion.zip"

echo "Extracting..."
unzip -qo "${TMPDIR}/scion.zip" -d "${TMPDIR}"

BINARY_NAME="scion"
if [[ "${PLATFORM}" == windows-* ]]; then
    BINARY_NAME="scion.exe"
fi

chmod +x "${TMPDIR}/${BINARY_NAME}"
mv "${TMPDIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"

echo ""
echo "Installed scion to ${INSTALL_DIR}/${BINARY_NAME}"

# Check if install dir is in PATH
case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
        echo ""
        echo "WARNING: ${INSTALL_DIR} is not in your PATH."
        echo "Add it by running:"
        echo ""
        echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
        echo ""
        ;;
esac

echo "Run 'scion --help' to get started."
