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

# teardown.sh — delete a single-node Scion Cloud Run Instance.
#
# Usage:
#   ./teardown.sh --name NAME --project PROJECT [--region REGION]
#
# What it does:
#   1. Deletes the Cloud Run Instance.
#   2. Prints a reminder about IAP policy bindings (region-scoped, not
#      per-instance — the script does not remove them automatically because
#      they may grant access to other instances in the same region).

set -euo pipefail

# ---------------------------------------------------------------------------
# Parse arguments
# ---------------------------------------------------------------------------
NAME=""
PROJECT=""
REGION="us-east4"

usage() {
  echo "Usage: $0 --name NAME --project PROJECT [--region REGION]" >&2
}

# require_value FLAG VALUE — reject a flag given no value, an empty value, or
# another flag as its value. Without this, `--name` with nothing after it dies
# on `$2: unbound variable`, and `--name --project foo` silently takes
# "--project" as the instance name. This script deletes cloud resources; a
# misparsed argument must stop it, not be guessed at.
require_value() {
  if [[ -z "${2:-}" || "$2" == -* ]]; then
    echo "Error: $1 requires a non-empty value." >&2
    usage
    exit 1
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --name)     require_value "$1" "${2:-}"; NAME="$2";    shift 2 ;;
    --project)  require_value "$1" "${2:-}"; PROJECT="$2"; shift 2 ;;
    --region)   require_value "$1" "${2:-}"; REGION="$2";  shift 2 ;;
    *)
      echo "Unknown option: $1" >&2
      usage
      exit 1
      ;;
  esac
done

if [[ -z "$NAME" || -z "$PROJECT" ]]; then
  echo "Error: --name and --project are required." >&2
  usage
  exit 1
fi

# ---------------------------------------------------------------------------
# Delete the Instance
# ---------------------------------------------------------------------------
echo "==> Deleting Cloud Run Instance '$NAME' in $PROJECT ($REGION)..."
gcloud beta run instances delete "$NAME" \
  --region="$REGION" \
  --project="$PROJECT" \
  --quiet

echo "    Instance deleted."

# ---------------------------------------------------------------------------
# Reminder about IAP bindings
# ---------------------------------------------------------------------------
echo ""
echo "=== Teardown complete ==="
echo ""
echo "Note: IAP access bindings are scoped to the region, not the instance."
echo "If this was the only instance in $REGION, you may want to clean up:"
echo ""
echo "  gcloud iap web get-iam-policy \\"
echo "    --project=$PROJECT \\"
echo "    --region=$REGION \\"
echo "    --resource-type=cloud-run"
echo ""
echo "To remove a specific user's access:"
echo ""
echo "  gcloud iap web remove-iam-policy-binding \\"
echo "    --project=$PROJECT \\"
echo "    --region=$REGION \\"
echo "    --resource-type=cloud-run \\"
echo "    --member=user:EMAIL \\"
echo "    --role=roles/iap.httpsResourceAccessor"
