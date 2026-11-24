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

# Verifies that every harness in harnesses/<name>/Dockerfile recorded a
# successful push via buildx-record-digest.sh (BUILD_STEP_STATUS_DIR).
#
# Used as the final gate step in image-build/cloudbuild-common.yaml so
# allowFailure harness steps cannot block scion-hub or other harnesses while
# the overall Cloud Build still fails visibly if any harness build failed.
#
# Usage:
#   image-build/scripts/verify-harness-builds.sh <status-dir> [harnesses-dir]

set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
  echo "usage: verify-harness-builds.sh <status-dir> [harnesses-dir]" >&2
  exit 2
fi

status_dir="$1"
harnesses_dir="${2:-harnesses}"

if [ ! -d "${harnesses_dir}" ]; then
  echo "verify-harness-builds: harnesses directory not found: ${harnesses_dir}" >&2
  exit 2
fi

checked=0
failed=()
for df in "${harnesses_dir}"/*/Dockerfile; do
  [ -f "${df}" ] || continue
  checked=$((checked + 1))
  name="$(basename "$(dirname "${df}")")"
  img="scion-${name}"
  if [ ! -s "${status_dir}/${img}" ]; then
    failed+=("${img}")
  fi
done

if [ "${checked}" -eq 0 ]; then
  echo "verify-harness-builds: no harnesses/*/Dockerfile found under ${harnesses_dir}" >&2
  exit 2
fi

if [ "${#failed[@]}" -gt 0 ]; then
  echo "verify-harness-builds: ERROR: ${#failed[@]} of ${checked} harness image build(s) failed: ${failed[*]}" >&2
  exit 1
fi

echo "verify-harness-builds: all ${checked} harness images built and pushed."
