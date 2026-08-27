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
# This script implements the complete deploy flow:
#   1. Resolves the deploying operator's identity
#   2. Resolves the GCP project number
#   3a. Creates or updates the Cloud Run Instance via gcloud (v1 surface,
#       required because sandboxLauncher is a v1-only field)
#   3b. Enables IAP via REST v2 PATCH (iapEnabled + invokerIamDisabled are
#       v2-only fields)
#   4. Waits for IAP enforcement to become active (30-75s)
#   5. Binds the IAP access policy for the operator
#   6. Prints effective access (project-level and region-level)
#   7. Asserts the IAP perimeter is enforcing (fails loudly if not)
#   8. Prints the Instance URL
#
# The script is idempotent: re-running converges without duplication.
# iapEnabled and invokerIamDisabled are sent on EVERY write to prevent
# silent perimeter drops.
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
#   --image-registry  Override image registry (default: derived from --image)

set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults (must match the Go command's defaults byte-for-byte)
# ---------------------------------------------------------------------------
NAME=""
IMAGE=""
PROJECT=""
REGION="us-east4"
ADMIN_EMAIL=""
SERVICE_ACCOUNT=""
MEMORY="8Gi"
CPU="4"
IMAGE_REGISTRY=""

# ---------------------------------------------------------------------------
# Parse flags
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --name)           NAME="$2"; shift 2 ;;
    --image)          IMAGE="$2"; shift 2 ;;
    --project)        PROJECT="$2"; shift 2 ;;
    --region)         REGION="$2"; shift 2 ;;
    --admin-email)    ADMIN_EMAIL="$2"; shift 2 ;;
    --service-account) SERVICE_ACCOUNT="$2"; shift 2 ;;
    --memory)         MEMORY="$2"; shift 2 ;;
    --cpu)            CPU="$2"; shift 2 ;;
    --image-registry) IMAGE_REGISTRY="$2"; shift 2 ;;
    --help|-h)
      sed -n '/^# deploy\.sh/,/^[^#]/{ /^#/s/^# \?//p }' "$0"
      exit 0
      ;;
    *)
      echo "Error: unknown flag: $1" >&2
      exit 1
      ;;
  esac
done

# ---------------------------------------------------------------------------
# Validate required flags
# ---------------------------------------------------------------------------
missing=()
[[ -z "$NAME" ]]    && missing+=("--name")
[[ -z "$IMAGE" ]]   && missing+=("--image")
[[ -z "$PROJECT" ]] && missing+=("--project")
if [[ ${#missing[@]} -gt 0 ]]; then
  echo "Error: missing required flag(s): ${missing[*]}" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Helper: run gcloud, capturing only stdout (stderr goes to stderr).
# gcloud writes diagnostic messages (notably the impersonation warning
# "WARNING: This command is using service account impersonation...") to
# stderr. Using only stdout prevents those from contaminating data values
# like project numbers and URLs.
# ---------------------------------------------------------------------------
run_gcloud() {
  gcloud "$@"
}

# ===================================================================
# Step 1: Resolve identity
# ===================================================================
echo "==> Step 1: Resolving deployer identity..."
OPERATOR_EMAIL="$(run_gcloud config get account 2>/dev/null)"
OPERATOR_EMAIL="$(echo "$OPERATOR_EMAIL" | tr -d '[:space:]')"
if [[ -z "$OPERATOR_EMAIL" ]]; then
  echo "Error: gcloud returned empty account — is gcloud configured?" >&2
  exit 1
fi
echo "    Deployer: $OPERATOR_EMAIL"

# Determine admin email: use --admin-email override if provided, otherwise operator
admin_email="$OPERATOR_EMAIL"
if [[ -n "$ADMIN_EMAIL" ]]; then
  admin_email="$ADMIN_EMAIL"
  echo "    Admin override: $admin_email"
fi

# Guard: gcloud --set-env-vars is comma-delimited. A comma in the email
# would silently split into a second env var, breaking the command.
if [[ "$admin_email" == *","* ]]; then
  echo "Error: --admin-email value '$admin_email' contains a comma, which would break gcloud --set-env-vars" >&2
  exit 1
fi

# ===================================================================
# Step 2: Resolve project number
# ===================================================================
echo "==> Step 2: Resolving project number..."
PROJECT_NUMBER="$(run_gcloud projects describe "$PROJECT" --format='value(projectNumber)' 2>/dev/null)"
PROJECT_NUMBER="$(echo "$PROJECT_NUMBER" | tr -d '[:space:]')"
if [[ -z "$PROJECT_NUMBER" ]]; then
  echo "Error: gcloud returned empty project number for '$PROJECT'" >&2
  exit 1
fi
# Validate: reject anything that is not a pure numeric string.
# gcloud under service-account impersonation prepends a WARNING to stderr;
# if stderr leaks into the captured value, the project number becomes
# garbage and every consumer silently embeds it.
if ! [[ "$PROJECT_NUMBER" =~ ^[0-9]+$ ]]; then
  echo "Error: project number '$PROJECT_NUMBER' is not purely numeric — gcloud output may be contaminated (stderr mixed into stdout?)" >&2
  exit 1
fi
echo "    Project: $PROJECT (number: $PROJECT_NUMBER)"

# Compute the IAP audience and instance URL.
# NOTE: The audience uses "services" even though this is an Instance.
# This is IAP's fixed resource vocabulary across every backend type.
# Do NOT change to "instances".
# FORMAT STRING: /projects/%s/locations/%s/services/%s
IAP_AUDIENCE="/projects/${PROJECT_NUMBER}/locations/${REGION}/services/${NAME}"
# FORMAT STRING: https://%s-%s.%s.run.app
INSTANCE_URL="https://${NAME}-${PROJECT_NUMBER}.${REGION}.run.app"

# Validate the computed URL before using it in gates and deploy.
if ! [[ "$INSTANCE_URL" =~ ^https://[^/]+\.run\.app$ ]]; then
  echo "Error: computed instance URL '$INSTANCE_URL' is invalid (project number may be contaminated)" >&2
  exit 1
fi

# Resolve the image registry: --image-registry override, or derived from --image.
# The broker requires SCION_IMAGE_REGISTRY to pull agent images.
if [[ -z "$IMAGE_REGISTRY" ]]; then
  # Derive registry from image reference.
  # Strip tag (:...) or digest (@sha256:...) first.
  ref="$IMAGE"
  # Strip digest
  ref="${ref%%@*}"
  # Strip tag (only if the colon is after the last slash)
  if [[ "$ref" == */* ]]; then
    last_component="${ref##*/}"
    prefix="${ref%/*}"
    last_component="${last_component%%:*}"
    ref="${prefix}/${last_component}"
  fi
  # The registry is everything before the last path component
  IMAGE_REGISTRY="${ref%/*}"
  if [[ -z "$IMAGE_REGISTRY" ]] || [[ "$IMAGE_REGISTRY" == "$ref" ]]; then
    echo "Error: cannot derive registry from image '$IMAGE' — expected host/org/name format (e.g. us-docker.pkg.dev/project/repo/image)" >&2
    echo "Use --image-registry to set it explicitly" >&2
    exit 1
  fi
  # Sanity: registry must contain a dot (hostname) or colon (port)
  if [[ "$IMAGE_REGISTRY" != *"."* ]] && [[ "$IMAGE_REGISTRY" != *":"* ]]; then
    echo "Error: derived registry '$IMAGE_REGISTRY' from image '$IMAGE' does not look like a hostname — use --image-registry to override" >&2
    exit 1
  fi
fi
echo "    Image registry: $IMAGE_REGISTRY"

# ===================================================================
# Step 3a: Create/update the Instance via gcloud (v1 surface)
# gcloud speaks v1, which is the ONLY surface that has sandboxLauncher.
# REST v2 neither sets nor returns sandboxLauncher.
# ===================================================================
echo "==> Step 3a: Creating/updating Cloud Run Instance (gcloud, v1 surface)..."

gcloud_args=(
  beta run instances deploy "$NAME"
  --image "$IMAGE"
  --sandbox-launcher
  --region "$REGION"
  --project "$PROJECT"
  --set-env-vars "SCION_SERVER_MODE=hosted,SCION_SERVER_AUTH_MODE=proxy,SCION_SERVER_AUTH_PROXY_PROVIDER=iap,SCION_SERVER_AUTH_PROXY_IAP_AUDIENCE=${IAP_AUDIENCE},SCION_SERVER_HUB_ADMINEMAILS=${admin_email},SCION_IMAGE_REGISTRY=${IMAGE_REGISTRY}"
)

if [[ -n "$SERVICE_ACCOUNT" ]]; then
  gcloud_args+=(--service-account "$SERVICE_ACCOUNT")
fi
if [[ -n "$MEMORY" ]]; then
  gcloud_args+=(--memory "$MEMORY")
fi
if [[ -n "$CPU" ]]; then
  gcloud_args+=(--cpu "$CPU")
fi

echo "    gcloud ${gcloud_args[*]:0:6}"
run_gcloud "${gcloud_args[@]}"
echo "    Instance deployed successfully."

# ===================================================================
# Step 3b: Enable IAP via REST v2 PATCH
# iapEnabled and invokerIamDisabled are v2-only fields. gcloud has no
# --iap flag, so we flip both booleans with a single REST PATCH.
#
# This PATCH is safe because it uses updateMask to touch ONLY the IAP
# booleans, leaving all v1-only fields (like sandboxLauncher) untouched.
#
# Invariant: invokerIamDisabled: true is NEVER sent without iapEnabled: true.
# ===================================================================
echo "==> Step 3b: Enabling IAP (REST v2 PATCH)..."

# Get access token — NEVER print it to stdout.
ACCESS_TOKEN="$(gcloud auth print-access-token 2>/dev/null)"
ACCESS_TOKEN="$(echo "$ACCESS_TOKEN" | tr -d '[:space:]')"
if [[ -z "$ACCESS_TOKEN" ]]; then
  echo "Error: gcloud returned empty access token" >&2
  exit 1
fi

PATCH_URL="https://${REGION}-run.googleapis.com/v2/projects/${PROJECT}/locations/${REGION}/instances/${NAME}?updateMask=iapEnabled,invokerIamDisabled"
echo "    PATCH $PATCH_URL"

PATCH_RESP_FILE="$(mktemp)"
trap 'rm -f "$PATCH_RESP_FILE"' EXIT
HTTP_CODE="$(curl -s -o "$PATCH_RESP_FILE" -w "%{http_code}" \
  -X PATCH \
  -H "Authorization: Bearer ${ACCESS_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"iapEnabled":true,"invokerIamDisabled":true}' \
  "$PATCH_URL")"

if [[ "$HTTP_CODE" -ge 300 ]]; then
  echo "Error: REST PATCH returned $HTTP_CODE:" >&2
  head -c 500 "$PATCH_RESP_FILE" >&2
  echo >&2
  exit 1
fi
echo "    IAP enabled on instance."

# ===================================================================
# Step 4: Gate 1 — Wait for IAP reconcile
# 30-75s observed. The API returns before enforcement is live.
# ===================================================================
echo "==> Step 4: Waiting for IAP enforcement to activate..."
echo "    (This typically takes 30-75 seconds)"

MAX_WAIT=180   # 3 minutes
POLL_INTERVAL=5
ELAPSED=0
LAST_STATUS=""
GATE1_HEADERS="$(mktemp)"

while [[ $ELAPSED -lt $MAX_WAIT ]]; do
  # Unauthenticated probe: no credentials, no redirect following.
  # Capture both status code and headers in a single request.
  PROBE_CODE="$(curl -s -o /dev/null -D "$GATE1_HEADERS" -w "%{http_code}" \
    --max-time 15 \
    "$INSTANCE_URL" 2>/dev/null)" || PROBE_CODE="000"

  if [[ "$PROBE_CODE" == "302" ]]; then
    LOCATION="$(grep -i '^location:' "$GATE1_HEADERS" | head -1 | tr -d '\r')"
    if [[ "$LOCATION" == *"accounts.google.com"* ]]; then
      rm -f "$GATE1_HEADERS"
      echo "    IAP enforcement is active."
      break
    fi
  fi

  LAST_STATUS="$PROBE_CODE"
  if [[ "$PROBE_CODE" == "000" ]]; then
    echo "    Polling... (not ready yet: connection failed)"
  else
    echo "    Polling... (status $PROBE_CODE, waiting for IAP 302)"
  fi
  sleep "$POLL_INTERVAL"
  ELAPSED=$((ELAPSED + POLL_INTERVAL))
done
rm -f "$GATE1_HEADERS" 2>/dev/null

if [[ $ELAPSED -ge $MAX_WAIT ]]; then
  diag="Error: timed out after ${MAX_WAIT}s waiting for IAP to enforce on $INSTANCE_URL"
  if [[ "$LAST_STATUS" == "502" ]] || [[ "$LAST_STATUS" == "503" ]]; then
    diag="$diag — last seen: $LAST_STATUS (instance may not be serving — check CMD, port, and container health)"
  elif [[ "$LAST_STATUS" == "000" ]]; then
    diag="$diag — last probe: connection failed (instance may not be serving on port 8080)"
  elif [[ -n "$LAST_STATUS" ]]; then
    diag="$diag — last seen: HTTP $LAST_STATUS"
  fi
  echo "$diag" >&2
  exit 1
fi

# ===================================================================
# Step 5: Bind IAP access policy at the region level
# ===================================================================
echo "==> Step 5: Binding IAP access policy..."

# Determine IAM member prefix: serviceAccount: or user:
iam_member_prefix() {
  local email="$1"
  if [[ "$email" == *".gserviceaccount.com" ]]; then
    echo "serviceAccount:"
  else
    echo "user:"
  fi
}

MEMBER_PREFIX="$(iam_member_prefix "$OPERATOR_EMAIL")"
run_gcloud iap web add-iam-policy-binding \
  "--project=${PROJECT}" \
  "--region=${REGION}" \
  --resource-type=cloud-run \
  "--member=${MEMBER_PREFIX}${OPERATOR_EMAIL}" \
  --role=roles/iap.httpsResourceAccessor
echo "    IAP access granted to $OPERATOR_EMAIL"

# If admin-email differs from operator, also bind for the admin
if [[ -n "$ADMIN_EMAIL" ]] && [[ "$ADMIN_EMAIL" != "$OPERATOR_EMAIL" ]]; then
  ADMIN_MEMBER_PREFIX="$(iam_member_prefix "$ADMIN_EMAIL")"
  run_gcloud iap web add-iam-policy-binding \
    "--project=${PROJECT}" \
    "--region=${REGION}" \
    --resource-type=cloud-run \
    "--member=${ADMIN_MEMBER_PREFIX}${ADMIN_EMAIL}" \
    --role=roles/iap.httpsResourceAccessor
  echo "    IAP access granted to $ADMIN_EMAIL"
fi

# ===================================================================
# Step 6: Read back and print effective access
# Both project-level and region-level, because project-level grants
# inherit invisibly.
# ===================================================================
echo "==> Step 6: Reading effective access..."

echo "    --- Region-level IAP bindings ---"
region_policy="$(run_gcloud iap web get-iam-policy \
  "--project=${PROJECT}" \
  "--region=${REGION}" \
  --resource-type=cloud-run 2>/dev/null)" || true
if [[ -n "$region_policy" ]]; then
  while IFS= read -r line; do echo "    $line"; done <<< "$region_policy"
else
  echo "    (no bindings)"
fi

echo "    --- Project-level IAP bindings (inherited) ---"
project_policy="$(run_gcloud projects get-iam-policy "$PROJECT" \
  --format=yaml 2>/dev/null)" || true
if [[ -n "$project_policy" ]]; then
  # Filter for iap.httpsResourceAccessor bindings
  echo "$project_policy" | awk '
    /^- members:/ || /^- role:/ { flush() }
    { current = current "\n" $0 }
    /role:.*iap\.httpsResourceAccessor/ { has_iap = 1 }
    END { flush() }
    function flush() {
      if (has_iap && current != "") {
        gsub(/^\n/, "", current)
        print current
      }
      current = ""
      has_iap = 0
    }
  ' | sed 's/^/    /' || echo "    (no project-level iap.httpsResourceAccessor bindings)"
else
  echo "    Warning: could not read project-level IAM policy"
fi

# ===================================================================
# Step 7: Gate 2 — Assert the perimeter (MOST VALUABLE DELIVERABLE)
#
# Fetch with NO credential. Require 302 to accounts.google.com.
# FAIL the deploy if the app answers. This is the guard for the
# single point of failure: with invoker IAM disabled, iapEnabled=false
# leaves the Instance open to the internet with nothing but hub
# session auth.
#
# This gate doubles as the post-deploy smoke check: if the Instance
# is dead (wrong port, crash loop, missing binary, bad CMD), Cloud Run
# returns 502/503, NOT the IAP 302. So a passing gate proves both
# that IAP is enforcing AND that the Instance is serving behind it.
#
# Do NOT use curl -f here. curl -f exits non-zero on 403, which is
# the exact response that means the gate PASSED. Capture the status
# code explicitly and branch on it.
# ===================================================================
echo "==> Step 7: Asserting IAP perimeter enforcement..."

# Capture both status code and headers from an unauthenticated request.
GATE2_HEADERS="$(mktemp)"
GATE2_CODE="$(curl -s -o /dev/null -D "$GATE2_HEADERS" -w "%{http_code}" \
  --max-time 15 \
  "$INSTANCE_URL" 2>/dev/null)" || GATE2_CODE="000"

if [[ "$GATE2_CODE" == "000" ]]; then
  rm -f "$GATE2_HEADERS"
  echo "SECURITY FAILURE: could not reach instance URL $INSTANCE_URL" >&2
  exit 1
fi

if [[ "$GATE2_CODE" != "302" ]]; then
  rm -f "$GATE2_HEADERS"
  if [[ "$GATE2_CODE" == "502" ]] || [[ "$GATE2_CODE" == "503" ]]; then
    echo "SECURITY FAILURE: expected 302 redirect but got $GATE2_CODE — the instance may not be serving (check Dockerfile CMD, port configuration, and container logs). Cloud Run returns $GATE2_CODE when the container is unhealthy or not listening on port 8080" >&2
  else
    echo "SECURITY FAILURE: expected 302 redirect but got $GATE2_CODE — IAP may not be enforcing! An unauthenticated request reached the app, which means the instance is UNPROTECTED" >&2
  fi
  exit 1
fi

# Got 302 — verify it points to accounts.google.com
GATE2_LOCATION="$(grep -i '^location:' "$GATE2_HEADERS" | head -1 | tr -d '\r')"
rm -f "$GATE2_HEADERS"

if [[ "$GATE2_LOCATION" != *"accounts.google.com"* ]]; then
  echo "SECURITY FAILURE: got 302 but not to accounts.google.com (Location: $GATE2_LOCATION) — IAP may not be enforcing" >&2
  exit 1
fi

echo "    IAP perimeter verified: unauthenticated requests are blocked."
echo "    Instance is serving and IAP-protected."

# ===================================================================
# Step 8: Print the URL
# ===================================================================
echo ""
echo "=== Deploy Complete ==="
echo "Instance URL: $INSTANCE_URL"
echo "Admin email:  $admin_email"
echo ""
echo "Open the URL in a browser to log in. The deployer is seeded as admin."
