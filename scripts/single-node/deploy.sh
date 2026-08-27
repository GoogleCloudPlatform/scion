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

# deploy.sh — deploy a single-node Scion Hub on a Cloud Run Instance with IAP.
#
# This is a thin wrapper around `scion deploy-instance`. It exists so the
# tutorial has a single-command entry point an operator can read and audit
# before running.
#
# Usage:
#   ./deploy.sh --name NAME --project PROJECT --image IMAGE [options]
#
# Required:
#   --name        Instance name (e.g. my-scion-hub)
#   --project     GCP project ID
#   --image       Container image (tag or digest)
#
# Optional:
#   --region          GCP region (default: us-east4)
#   --admin-email     Admin email override (default: deployer's gcloud account)
#   --service-account GCP service account for the instance
#   --memory          Memory limit (default: 8Gi)
#   --cpu             CPU limit (default: 4)
#
# Environment:
#   SCION_BIN     Path to, or name of, the scion binary to run. Overrides the
#                 default search order (repo-root ./scion, then ./scion in the
#                 current directory, then `scion` on $PATH). Must resolve to an
#                 executable command or the script exits with an error.
#
# What it does:
#   1. Creates or updates a Cloud Run Instance with the sandbox launcher enabled.
#   2. Enables IAP on the Instance (iapEnabled + invokerIamDisabled via REST v2).
#   3. Waits for IAP enforcement to activate (~30-75 seconds).
#   4. Binds the deployer as an IAP-authorized user.
#   5. Asserts the IAP perimeter — fails the deploy if unauthenticated requests
#      reach the app.
#   6. Prints the Instance URL.

set -euo pipefail

# ---------------------------------------------------------------------------
# Locate the scion binary
#
# Resolution order:
#   1. $SCION_BIN, if set — explicit override, wins over everything.
#   2. ./scion in the repository root — where the tutorial's
#      `go build -tags no_embed_web -o ./scion ./cmd/scion/` puts it.
#   3. ./scion in the current directory.
#   4. `scion` on $PATH.
#
# Step 2 matters: `deploy-instance` may not exist in an installed release, so
# the tutorial tells the operator to build into the repo root and then run this
# script from there. Looking only at $PATH would fail that documented flow.
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

if [[ -n "${SCION_BIN:-}" ]]; then
  if ! command -v "$SCION_BIN" &>/dev/null; then
    echo "Error: SCION_BIN is set to '$SCION_BIN', which is not an executable command." >&2
    exit 1
  fi
elif [[ -x "$REPO_ROOT/scion" ]]; then
  SCION_BIN="$REPO_ROOT/scion"
elif [[ -x "./scion" ]]; then
  SCION_BIN="./scion"
elif command -v scion &>/dev/null; then
  SCION_BIN="scion"
else
  echo "Error: no 'scion' binary found." >&2
  echo "Looked for: \$SCION_BIN, $REPO_ROOT/scion, ./scion, and 'scion' on \$PATH." >&2
  echo "Build one with:" >&2
  echo "  go build -tags no_embed_web -o ./scion ./cmd/scion/" >&2
  echo "or set SCION_BIN to the path of an existing binary." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Forward all arguments to scion deploy-instance
# ---------------------------------------------------------------------------
exec "$SCION_BIN" deploy-instance "$@"
