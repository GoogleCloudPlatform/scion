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

# scripts/single-node-vm/check-cloud-init.sh — shellcheck the `runcmd` body
# of cloud-init.yaml.
#
# cloud-init serializes the `runcmd` list into a script that executes under
# /bin/sh (dash on Ubuntu), not bash. Bash idioms silently misbehave instead
# of erroring under dash (see the `&>` bug fixed by ptone/scion#1759) --
# there is no cloud-init-native check for this, so this script extracts the
# runcmd list and runs `shellcheck -s sh` over it as a standalone step a
# human or CI can run before merging changes to cloud-init.yaml.
#
# Usage:
#   ./check-cloud-init.sh [path/to/cloud-init.yaml]
#
# Requires:
#   - shellcheck on PATH.
#   - A Python 3 interpreter with PyYAML -- the same dependency deploy.sh
#     already requires for --config parsing. Set PYTHON=/path/to/python3 to
#     override, same convention as deploy.sh, if the system python3 lacks
#     PyYAML (see deploy.sh's PYTHON override / PEP 668 guidance).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLOUD_INIT_FILE="${1:-${SCRIPT_DIR}/cloud-init.yaml}"
PYTHON="${PYTHON:-python3}"

if [[ ! -f "$CLOUD_INIT_FILE" ]]; then
  echo "ERROR: cloud-init file not found: ${CLOUD_INIT_FILE}" >&2
  exit 1
fi

if ! command -v shellcheck &>/dev/null; then
  echo "ERROR: shellcheck is required but was not found on PATH." >&2
  exit 1
fi

if ! command -v "$PYTHON" &>/dev/null || ! "$PYTHON" -c "import yaml" &>/dev/null; then
  echo "ERROR: '${PYTHON}' with the PyYAML module is required to parse ${CLOUD_INIT_FILE}." >&2
  echo "       Install it the way deploy.sh documents (apt-get install python3-yaml, a venv" >&2
  echo "       with 'pip install pyyaml', etc.), or set PYTHON=/path/to/python3." >&2
  exit 1
fi

RUNCMD_SCRIPT="$(mktemp)"
trap 'rm -f "$RUNCMD_SCRIPT"' EXIT

# Pull the runcmd list out with PyYAML (not sed/awk -- YAML block scalars
# and quoting rules aren't safely hand-parsed) and join it into one shell
# script in list order, the same order cloud-init executes it under /bin/sh.
"$PYTHON" -c "
import shlex
import sys
import yaml

with open(sys.argv[1]) as f:
    doc = yaml.safe_load(f)

runcmd = doc.get('runcmd') if isinstance(doc, dict) else None
if not runcmd:
    sys.stderr.write(\"No 'runcmd' list found in \" + sys.argv[1] + '\n')
    sys.exit(1)

with open(sys.argv[2], 'w') as out:
    out.write('#!/bin/sh\n')
    for item in runcmd:
        if isinstance(item, str):
            out.write(item.rstrip('\n') + '\n')
        else:
            # cloud-init also allows list-of-args runcmd entries (argv form,
            # run without a shell) -- shlex.join matches that argv-quoting
            # semantics more closely than a plain ' '.join. Dead code today:
            # every entry in this file is a string.
            out.write(shlex.join(str(x) for x in item) + '\n')
" "$CLOUD_INIT_FILE" "$RUNCMD_SCRIPT"

echo "Extracted runcmd body from ${CLOUD_INIT_FILE}:"
echo "---"
cat -n "$RUNCMD_SCRIPT"
echo "---"
echo ""
echo "Running: shellcheck -s sh"
shellcheck -s sh "$RUNCMD_SCRIPT"
echo "shellcheck: no issues found in the runcmd body."
