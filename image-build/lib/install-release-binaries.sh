#!/usr/bin/env bash
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
#
# install-release-binaries.sh --tag <tag> [--arch <amd64|arm64>] [--dest <dir>] [--base-url <url>] <binary...>
#
# Downloads GitHub release tarballs for the named Scion Linux binaries
# (scion, sciontool) together with SHA256SUMS, verifies each tarball against
# its own line in SHA256SUMS, and extracts the binaries into --dest
# (default: /usr/local/bin).

set -euo pipefail

TAG=""
ARCH=""
DEST="/usr/local/bin"
BASE_URL="${SCION_RELEASE_BASE_URL:-https://github.com/GoogleCloudPlatform/scion/releases/download}"
BINARIES=()

usage() {
  cat >&2 <<EOF
Usage: $(basename "$0") --tag <release-tag> [--arch <amd64|arm64>] [--dest <dir>] [--base-url <url>] <scion|sciontool>...
EOF
  exit "${1:-2}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)
      [[ $# -ge 2 ]] || { echo "Error: --tag requires a value" >&2; usage 2; }
      TAG="$2"; shift 2 ;;
    --arch)
      [[ $# -ge 2 ]] || { echo "Error: --arch requires a value" >&2; usage 2; }
      ARCH="$2"; shift 2 ;;
    --dest)
      [[ $# -ge 2 ]] || { echo "Error: --dest requires a value" >&2; usage 2; }
      DEST="$2"; shift 2 ;;
    --base-url)
      [[ $# -ge 2 ]] || { echo "Error: --base-url requires a value" >&2; usage 2; }
      BASE_URL="$2"; shift 2 ;;
    -h|--help)
      usage 0 ;;
    --)
      shift
      while [[ $# -gt 0 ]]; do
        BINARIES+=("$1"); shift
      done
      ;;
    -*)
      echo "Error: unknown option '$1'" >&2
      usage 2 ;;
    *)
      BINARIES+=("$1"); shift ;;
  esac
done

BASE_URL="${BASE_URL%/}"

release_tag_re='^(v[0-9]+\.[0-9]+\.[0-9]+(-(preview|rc)\.[0-9]+)?|nightly-[0-9]{8})$'
if [[ -z "${TAG}" ]]; then
  echo "Error: --tag is required (expected a release tag such as v0.4.0-preview.3 or nightly-20261123)." >&2
  exit 1
fi
if [[ ! "${TAG}" =~ ${release_tag_re} ]]; then
  echo "Error: invalid release tag '${TAG}' (expected vX.Y.Z, vX.Y.Z-preview.N, vX.Y.Z-rc.N, or nightly-YYYYMMDD)." >&2
  exit 1
fi

if [[ -z "${ARCH}" ]]; then
  case "$(uname -m)" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)
      echo "Error: unsupported host architecture '$(uname -m)'; pass --arch amd64 or --arch arm64." >&2
      exit 1 ;;
  esac
fi
if [[ "${ARCH}" != "amd64" && "${ARCH}" != "arm64" ]]; then
  echo "Error: unsupported --arch '${ARCH}' (allowed: amd64, arm64)." >&2
  exit 1
fi

if [[ ${#BINARIES[@]} -eq 0 ]]; then
  echo "Error: at least one binary name (scion, sciontool) is required." >&2
  usage 2
fi
for bin in "${BINARIES[@]}"; do
  if [[ "${bin}" != "scion" && "${bin}" != "sciontool" ]]; then
    echo "Error: unsupported binary '${bin}' (allowed: scion, sciontool)." >&2
    exit 1
  fi
done

if command -v sha256sum >/dev/null 2>&1; then
  SHA_CMD=(sha256sum -c -)
elif command -v shasum >/dev/null 2>&1; then
  SHA_CMD=(shasum -a 256 -c -)
else
  echo "Error: neither sha256sum nor shasum found on PATH." >&2
  exit 1
fi

TMPDIR_WORK="$(mktemp -d "${TMPDIR:-/tmp}/scion-release-bin.XXXXXX")"
trap 'rm -rf -- "${TMPDIR_WORK}"' EXIT

SUMS_URL="${BASE_URL}/${TAG}/SHA256SUMS"
SUMS_FILE="${TMPDIR_WORK}/SHA256SUMS"
echo "Downloading ${SUMS_URL}..."
if ! curl -fsSL "${SUMS_URL}" -o "${SUMS_FILE}"; then
  echo "Error: failed to download SHA256SUMS for release ${TAG} from ${SUMS_URL}." >&2
  echo "  Verify that release ${TAG} exists and has published assets, or build with --binaries source." >&2
  exit 1
fi

lookup_sum_line() {
  local asset="$1"
  awk -v asset="${asset}" '
    {
      sub(/\r$/, "")
      if (NF == 2) {
        f = $2
        sub(/^\*/, "", f)
        sub(/^\.\//, "", f)
        if (f == asset && $1 ~ /^[0-9a-fA-F]{64}$/) {
          print $1 "  " asset
        }
      }
    }
  ' "${SUMS_FILE}"
}

# Pre-flight: verify every requested binary has a single valid entry in
# SHA256SUMS before downloading any tarball, so an older release missing
# sciontool fails immediately without downloading scion first.
SUM_LINES=()
for bin in "${BINARIES[@]}"; do
  asset="${bin}-linux-${ARCH}.tar.gz"
  sum_line="$(lookup_sum_line "${asset}")"
  if [[ -z "${sum_line}" ]]; then
    if [[ "${bin}" == "sciontool" ]]; then
      echo "Error: release ${TAG} has no ${asset} entry in SHA256SUMS." >&2
      echo "  Release ${TAG} predates sciontool assets; re-run with --binaries source." >&2
    else
      echo "Error: release ${TAG} has no ${asset} entry in SHA256SUMS." >&2
      echo "  Verify that release ${TAG} includes Linux ${ARCH} assets, or re-run with --binaries source." >&2
    fi
    exit 1
  fi
  line_count="$(printf '%s\n' "${sum_line}" | wc -l | tr -d '[:space:]')"
  if [[ "${line_count}" -ne 1 ]]; then
    echo "Error: multiple SHA256SUMS entries found for ${asset} in release ${TAG}." >&2
    exit 1
  fi
  SUM_LINES+=("${sum_line}")
done

mkdir -p "${DEST}"

idx=0
for bin in "${BINARIES[@]}"; do
  asset="${bin}-linux-${ARCH}.tar.gz"
  sum_line="${SUM_LINES[${idx}]}"
  idx=$((idx + 1))

  asset_url="${BASE_URL}/${TAG}/${asset}"
  asset_path="${TMPDIR_WORK}/${asset}"
  echo "Downloading ${asset_url}..."
  if ! curl -fsSL "${asset_url}" -o "${asset_path}"; then
    if [[ "${bin}" == "sciontool" ]]; then
      echo "Error: failed to download ${asset_url}." >&2
      echo "  Release ${TAG} predates sciontool assets or the asset is missing; re-run with --binaries source." >&2
    else
      echo "Error: failed to download ${asset_url}." >&2
    fi
    exit 1
  fi

  if ! (cd "${TMPDIR_WORK}" && printf '%s\n' "${sum_line}" | "${SHA_CMD[@]}"); then
    echo "Error: SHA256 verification failed for ${asset} (release ${TAG})." >&2
    exit 1
  fi

  extract_dir="${TMPDIR_WORK}/extract-${bin}"
  mkdir -p "${extract_dir}"
  tar -xzf "${asset_path}" -C "${extract_dir}" "${bin}"
  if [[ ! -f "${extract_dir}/${bin}" || -L "${extract_dir}/${bin}" ]]; then
    echo "Error: ${asset} did not contain regular file '${bin}'." >&2
    exit 1
  fi
  install -m 0755 "${extract_dir}/${bin}" "${DEST}/${bin}"
  echo "Installed ${DEST}/${bin} from ${asset} (${TAG})."
done
