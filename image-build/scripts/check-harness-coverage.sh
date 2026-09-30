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

# Guard: every default harness in harnesses/<name>/Dockerfile must have a
# build step in each "full catalog" Cloud Build config.
#
# ptone/scion#2357: harnesses/muse-code existed in the tree but was absent
# from cloudbuild-harnesses.yaml, so a deployment that publishes only the
# Cloud Build output got image-pull failures for scion-muse-code. The
# per-image builders (local-docker, local-podman) never had this problem --
# lib/targets.sh's discover_harness_names() walks harnesses/ at build time.
# The cloudbuild-*.yaml files are static snapshots gcloud requires up front
# (see image-build/README.md, "Cloud Build Configs"), so nothing forced them
# to stay in sync with the catalog. This script is that sync check.
#
# Checked files ("full catalog" configs -- every default harness must
# appear):
#   - cloudbuild.yaml            ("Full rebuild" header)
#   - cloudbuild-common.yaml     ("Common rebuild" header)
#   - cloudbuild-harnesses.yaml  (harnesses-only rebuild)
#   - cloudbuild-thick.yaml      ("Thick base full rebuild" header)
#
# Deliberately NOT checked: cloudbuild-omni.yaml. Its own header documents
# it as "a deliberate subset of harnesses for single-node deployment" and
# names the exact chain (_OMNI_CHAIN in scripts/lib/targets.sh); it is
# expected to omit harnesses and is exempt by design, not by oversight.
#
# Usage:
#   image-build/scripts/check-harness-coverage.sh
#
# Requires a Python 3 interpreter with PyYAML. Set PYTHON=/path/to/python3
# to override, same convention as scripts/single-node-vm/check-cloud-init.sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
IMAGE_BUILD_DIR="${REPO_ROOT}/image-build"
HARNESS_ROOT="${REPO_ROOT}/harnesses"
PYTHON="${PYTHON:-python3}"

# Full-catalog configs: every harnesses/<name>/Dockerfile must have a step.
CHECKED_CONFIGS=(
  "cloudbuild.yaml"
  "cloudbuild-common.yaml"
  "cloudbuild-harnesses.yaml"
  "cloudbuild-thick.yaml"
)

if ! command -v "${PYTHON}" &>/dev/null || ! "${PYTHON}" -c "import yaml" &>/dev/null; then
  echo "ERROR: '${PYTHON}' with the PyYAML module is required to parse the" >&2
  echo "       cloudbuild-*.yaml files. Install it (apt-get install" >&2
  echo "       python3-yaml, a venv with 'pip install pyyaml', etc.), or set" >&2
  echo "       PYTHON=/path/to/python3." >&2
  exit 2
fi

if [[ ! -d "${HARNESS_ROOT}" ]]; then
  echo "ERROR: harnesses directory not found: ${HARNESS_ROOT}" >&2
  exit 2
fi

# Expected harness set: every directory directly under harnesses/ that has
# its own Dockerfile. Same rule as lib/targets.sh's discover_harness_names().
expected="$(find "${HARNESS_ROOT}" -mindepth 2 -maxdepth 2 -name Dockerfile -print \
  | while IFS= read -r dockerfile; do basename "$(dirname "${dockerfile}")"; done \
  | sort -u)"

if [[ -z "${expected}" ]]; then
  echo "ERROR: no harnesses/<name>/Dockerfile found under ${HARNESS_ROOT}" >&2
  echo "       (harness discovery is broken, not that the catalog is empty)" >&2
  exit 2
fi

fail=0
for config_name in "${CHECKED_CONFIGS[@]}"; do
  config_path="${IMAGE_BUILD_DIR}/${config_name}"
  if [[ ! -f "${config_path}" ]]; then
    echo "ERROR: expected Cloud Build config not found: ${config_path}" >&2
    exit 2
  fi

  actual="$("${PYTHON}" -c "
import sys
import yaml

with open(sys.argv[1]) as f:
    doc = yaml.safe_load(f)

names = set()
for step in doc.get('steps') or []:
    d = step.get('dir', '')
    prefix = 'harnesses/'
    if d.startswith(prefix):
        names.add(d[len(prefix):])

for name in sorted(names):
    print(name)
" "${config_path}")"

  missing="$(comm -23 <(printf '%s\n' "${expected}") <(printf '%s\n' "${actual}"))"
  extra="$(comm -13 <(printf '%s\n' "${expected}") <(printf '%s\n' "${actual}"))"

  if [[ -n "${missing}" ]]; then
    echo "FAIL: ${config_name} is missing a build step for:" >&2
    while IFS= read -r name; do echo "  - ${name}" >&2; done <<<"${missing}"
    fail=1
  fi
  if [[ -n "${extra}" ]]; then
    echo "FAIL: ${config_name} builds a harness with no harnesses/<name>/Dockerfile:" >&2
    while IFS= read -r name; do echo "  - ${name}" >&2; done <<<"${extra}"
    fail=1
  fi
done

if [[ "${fail}" -ne 0 ]]; then
  echo "" >&2
  echo "Every harnesses/<name>/Dockerfile must have a build step in each of:" >&2
  printf '  %s\n' "${CHECKED_CONFIGS[@]}" >&2
  echo "(cloudbuild-omni.yaml is exempt -- see this script's header.)" >&2
  exit 1
fi

harness_count="$(printf '%s\n' "${expected}" | wc -l | tr -d ' ')"
echo "check-harness-coverage: all ${#CHECKED_CONFIGS[@]} configs cover all ${harness_count} harnesses"
