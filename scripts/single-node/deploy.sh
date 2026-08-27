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
#
# Sourceable: this file can be sourced to access individual di_* functions
# for testing. Sourcing has no side effects — no gcloud calls, no output,
# no argument parsing at file scope.

# ---------------------------------------------------------------------------
# Pure functions — no side effects, testable in isolation
# ---------------------------------------------------------------------------

# di_validate_project_number rejects anything that is not a pure numeric string.
# gcloud under service-account impersonation prepends a WARNING to stderr; if
# stderr leaks into the captured value, the project number becomes garbage and
# every consumer silently embeds it.
# Prints error to stderr and returns 1 on failure.
di_validate_project_number() {
  local num="$1"
  if [[ -z "$num" ]]; then
    echo "Error: project number is empty" >&2
    return 1
  fi
  if ! [[ "$num" =~ ^[0-9]+$ ]]; then
    echo "Error: project number '$num' is not purely numeric — gcloud output may be contaminated (stderr mixed into stdout?)" >&2
    return 1
  fi
  return 0
}

# di_validate_instance_url rejects anything that does not look like an https
# Cloud Run URL (https://<something>.run.app).
# Prints error to stderr and returns 1 on failure.
di_validate_instance_url() {
  local url="$1"
  if [[ -z "$url" ]]; then
    echo "Error: instance URL is empty" >&2
    return 1
  fi
  if ! [[ "$url" =~ ^https://[a-zA-Z0-9._-]+\.run\.app$ ]]; then
    echo "Error: computed instance URL '$url' is invalid (project number may be contaminated)" >&2
    return 1
  fi
  return 0
}

# di_derive_registry extracts the registry prefix from a container image
# reference. For example:
#   ghcr.io/ptone/scion-omni:latest       → ghcr.io/ptone
#   us-docker.pkg.dev/proj/repo/img:tag    → us-docker.pkg.dev/proj/repo
#   ghcr.io/ptone/scion-omni@sha256:abc    → ghcr.io/ptone
#   localhost:5000/myimage:latest           → localhost:5000
# Prints the registry to stdout, or prints error to stderr and returns 1.
di_derive_registry() {
  local image="$1"
  local ref="$image"
  # Strip digest (@sha256:...)
  ref="${ref%%@*}"
  # Strip tag (only if the colon is after the last slash)
  if [[ "$ref" == */* ]]; then
    local last_component="${ref##*/}"
    local prefix="${ref%/*}"
    last_component="${last_component%%:*}"
    ref="${prefix}/${last_component}"
  fi
  # The registry is everything before the last path component
  local registry="${ref%/*}"
  if [[ -z "$registry" ]] || [[ "$registry" == "$ref" ]]; then
    echo "Error: cannot derive registry from image '$image' — expected host/org/name format (e.g. us-docker.pkg.dev/project/repo/image)" >&2
    return 1
  fi
  # Sanity: registry must contain a dot (hostname) or colon (port)
  if [[ "$registry" != *"."* ]] && [[ "$registry" != *":"* ]]; then
    echo "Error: derived registry '$registry' from image '$image' does not look like a hostname — use --image-registry to override" >&2
    return 1
  fi
  echo "$registry"
  return 0
}

# di_iam_member_prefix returns the correct IAM member prefix for the given email.
# Service accounts (ending in .gserviceaccount.com) use "serviceAccount:";
# all other emails use "user:".
di_iam_member_prefix() {
  local email="$1"
  if [[ "$email" == *".gserviceaccount.com" ]]; then
    echo "serviceAccount:"
  else
    echo "user:"
  fi
}

# di_validate_admin_email rejects admin emails containing commas.
# gcloud --set-env-vars is comma-delimited, so a comma in the value
# would silently split into a second env var, breaking the command.
# Prints error to stderr and returns 1 on failure.
di_validate_admin_email() {
  local email="$1"
  if [[ "$email" == *","* ]]; then
    echo "Error: --admin-email value '$email' contains a comma, which would break gcloud --set-env-vars" >&2
    return 1
  fi
  return 0
}

# di_build_iap_audience computes the IAP audience path.
# NOTE: Uses "services" (not "instances") — this is IAP's fixed vocabulary.
# Arguments: project_number region name
# FORMAT STRING: /projects/%s/locations/%s/services/%s
di_build_iap_audience() {
  echo "/projects/$1/locations/$2/services/$3"
}

# di_build_instance_url computes the Cloud Run Instance URL.
# Arguments: name project_number region
# FORMAT STRING: https://%s-%s.%s.run.app
di_build_instance_url() {
  echo "https://$1-$2.$3.run.app"
}

# di_build_iap_patch_url constructs the REST v2 PATCH URL for enabling IAP.
# Arguments: region project name
di_build_iap_patch_url() {
  local region="$1" project="$2" name="$3"
  # _DI_API_BASE is the same TEST-ONLY seam the preflight GET uses, and the
  # preflight validates its host (see di_validate_override_url) before any
  # mutation runs. It is honoured here so a test can pin the property that
  # broke in the field: that this PATCH carries the very token the preflight
  # validated, rather than a freshly minted second one.
  local api_base="${_DI_API_BASE:-https://${region}-run.googleapis.com}"
  echo "${api_base}/v2/projects/${project}/locations/${region}/instances/${name}?updateMask=iapEnabled,invokerIamDisabled"
}

# di_iap_patch_body returns the JSON body for the IAP enable PATCH.
# Invariant: invokerIamDisabled: true is NEVER sent without iapEnabled: true.
di_iap_patch_body() {
  echo '{"iapEnabled":true,"invokerIamDisabled":true}'
}

# ---------------------------------------------------------------------------
# Preflight checks — verify prerequisites before any side effects
# ---------------------------------------------------------------------------

# di_check_gcloud_instances probes whether 'gcloud beta run instances' is
# available in the installed SDK. On older versions (measured absent at
# 575.0.0, measured present at 582.0.0) the noun does not exist and gcloud
# suggests 'gcloud alpha run instances' as a fallback — but the alpha
# surface uses 'create' not 'deploy' and has no --sandbox-launcher, so
# following that advice produces a broken Instance.
#
# This function probes the capability rather than parsing a version number
# because the exact first version that ships 'instances' is not established.
# Returns 0 if the capability is present, 1 with a diagnostic message if not.
di_check_gcloud_instances() {
  if gcloud beta run instances --help </dev/null &>/dev/null; then
    return 0
  fi
  cat >&2 <<'ERRMSG'
Error: 'gcloud beta run instances' is not available in your Cloud SDK.

This deploy requires 'gcloud beta run instances deploy --sandbox-launcher',
which is not present in all SDK versions. Measured: absent at 575.0.0,
present at 582.0.0.

Update your SDK:

  gcloud components update
  # or, on Debian/Ubuntu:
  sudo apt-get update && sudo apt-get --only-upgrade install google-cloud-cli

DO NOT use 'gcloud alpha run instances' instead. gcloud may suggest it, but
the alpha surface uses 'create' (not 'deploy') and does not support
--sandbox-launcher. An Instance created via alpha will lack sandbox support
and the server will crash on startup.
ERRMSG
  return 1
}

# di_validate_override_url rejects a test-only URL override that would send an
# access token somewhere other than Google or the local machine.
#
# Both _DI_TOKENINFO_URL and _DI_API_BASE are TEST-ONLY seams, and both now
# carry the ADC token: tokeninfo takes it in a query string, and the API base
# takes it in a Bearer header on a GET *and* on the step 3b PATCH — a mutating
# call. One rule covers both: the host must be one of Google's own or loopback
# (a stub that cannot move the token off the machine). Two seams under one rule
# beats two seams under two rules.
#
# Arguments: var_name url
# Returns 0 if the host is permitted, 1 with a diagnostic otherwise.
di_validate_override_url() {
  local var_name="$1"
  local url="$2"

  local host="${url#*://}"
  host="${host%%/*}"
  # Strip a :port suffix. Matching on the whole host first keeps a bare IPv6
  # literal like [::1] (colons, no port) from being mangled.
  if [[ "$host" =~ ^(.*):[0-9]+$ ]]; then
    host="${BASH_REMATCH[1]}"
  fi

  case "$host" in
    *.googleapis.com | 127.0.0.1 | localhost | '[::1]') return 0 ;;
  esac

  echo "Error: refusing to send an access token to host '$host'." >&2
  echo "$var_name is a test-only override; it must name a *.googleapis.com" >&2
  echo "host or loopback. Unset it and retry." >&2
  return 1
}

# di_preflight_rest_credential mints an Application Default Credential (ADC)
# token, validates it against the Cloud Run v2 API with a cheap GET, and
# compares the ADC identity with the active gcloud account.
#
# This runs BEFORE any resource is created or modified. If the token cannot
# be minted or the API rejects it, the deploy aborts with zero mutations —
# preventing a half-built deploy (Instance created, IAP not enabled).
#
# The validated token is stored in the caller's _di_adc_token variable
# (bash dynamic scope) for reuse in step 3b, avoiding a second mint.
#
# Arguments: gcloud_account region project
# Returns 0 on success, 1 on failure.
di_preflight_rest_credential() {
  local gcloud_account="$1"
  local region="$2"
  local project="$3"

  # --- Resolve and validate the TEST-ONLY endpoint seams ---
  # _DI_TOKENINFO_URL and _DI_API_BASE exist so the tests can point these calls
  # at a stub. Both are validated HERE, at the top of the preflight, before a
  # token is minted and before any resource is touched — so a bad override
  # means no token exists to leak and no Instance exists to strand. _DI_API_BASE
  # is validated here on behalf of step 3b too, which reads it via
  # di_build_iap_patch_url and runs strictly after this function.
  # Both URLs are echoed below, so a redirect is never invisible in the output.
  local tokeninfo_url="${_DI_TOKENINFO_URL:-https://oauth2.googleapis.com/tokeninfo}"
  if ! di_validate_override_url "_DI_TOKENINFO_URL" "$tokeninfo_url"; then
    return 1
  fi
  local api_base="${_DI_API_BASE:-https://${region}-run.googleapis.com}"
  if ! di_validate_override_url "_DI_API_BASE" "$api_base"; then
    return 1
  fi

  echo "    Minting ADC token..."

  # Capture stderr so we can print it on failure (never suppress with 2>/dev/null).
  local adc_stderr_file
  adc_stderr_file="$(mktemp)"

  local tok
  tok="$(gcloud auth application-default print-access-token 2>"$adc_stderr_file" | tr -d '[:space:]')" || {
    echo "Error: 'gcloud auth application-default print-access-token' failed." >&2
    echo "stderr from gcloud:" >&2
    cat "$adc_stderr_file" >&2
    rm -f "$adc_stderr_file"
    echo "" >&2
    echo "Fix: run 'gcloud auth application-default login' and retry." >&2
    return 1
  }

  if [[ -z "$tok" ]]; then
    echo "Error: ADC returned an empty access token." >&2
    if [[ -s "$adc_stderr_file" ]]; then
      echo "stderr from gcloud:" >&2
      cat "$adc_stderr_file" >&2
    fi
    rm -f "$adc_stderr_file"
    echo "" >&2
    echo "Fix: run 'gcloud auth application-default login' and retry." >&2
    return 1
  fi
  rm -f "$adc_stderr_file"

  echo "    ADC token minted (${#tok} chars, prefix: ${tok:0:4}...)"

  # --- Resolve ADC identity via tokeninfo ---
  echo "    Resolving ADC identity via $tokeninfo_url"
  local tokeninfo_resp
  tokeninfo_resp="$(curl -s "${tokeninfo_url}?access_token=${tok}" 2>&1)" || true

  # tokeninfo does not always carry "email": a service-account token scoped
  # only to cloud-platform returns azp/aud/scope and no email. azp is a
  # NUMERIC CLIENT ID, which can never equal an email address — comparing the
  # two would warn on every single service-account run (metadata server, GCE,
  # Cloud Shell, CI). So compare only when the email claim is present, and
  # otherwise report the client ID and say the comparison was skipped.
  local adc_email
  adc_email="$(echo "$tokeninfo_resp" | grep '"email"' | sed 's/.*"email"[[:space:]]*:[[:space:]]*"//;s/".*//')" || true
  local adc_azp
  adc_azp="$(echo "$tokeninfo_resp" | grep '"azp"' | sed 's/.*"azp"[[:space:]]*:[[:space:]]*"//;s/".*//')" || true

  if [[ -n "$adc_email" ]]; then
    echo "    ADC identity: $adc_email"
    # Warn on identity mismatch — not a hard failure (a deliberate mismatch
    # is legitimate), but it must be visible.
    if [[ "$adc_email" != "$gcloud_account" ]]; then
      echo "" >&2
      echo "    WARNING: ADC identity does not match the active gcloud account." >&2
      echo "      gcloud account: $gcloud_account" >&2
      echo "      ADC identity:   $adc_email" >&2
      echo "    Step 3a (gcloud) will run as the gcloud account." >&2
      echo "    Step 3b (REST PATCH) will run as the ADC identity." >&2
      echo "    If this is unintentional, run: gcloud auth application-default login" >&2
      echo ""
    fi
  elif [[ -n "$adc_azp" ]]; then
    echo "    ADC identity: client ID $adc_azp (no email claim — comparison with the gcloud account skipped)"
  else
    echo "    ADC identity: (could not resolve — tokeninfo returned no email or azp)"
  fi

  # --- Validate with one cheap GET against the v2 surface ---
  # A non-2xx here means the token will be rejected at step 3b.
  # Abort NOW, before step 3a creates an Instance we cannot configure.
  local list_url="${api_base}/v2/projects/${project}/locations/${region}/instances"
  echo "    Validating ADC token against Cloud Run API..."
  echo "    GET $list_url"

  local resp_file
  resp_file="$(mktemp)"
  local http_code
  http_code="$(curl -s -o "$resp_file" -w "%{http_code}" \
    -H "Authorization: Bearer ${tok}" \
    "$list_url")" || {
    echo "Error: could not connect to $list_url — check network connectivity" >&2
    rm -f "$resp_file"
    return 1
  }

  # Unreachable by construction today: every curl exit path that yields no
  # status also exits non-zero, and the || block above catches that first.
  # Kept as a belt-and-braces guard because the alternative — an empty
  # http_code falling into the numeric [[ -ge 300 ]] test below — is a silent
  # pass, and this check must never fail open. Deliberately untested: a test
  # would have to stub curl into a state curl does not produce.
  if [[ -z "$http_code" ]]; then
    echo "Error: curl returned no HTTP status for GET $list_url — treating as a failure" >&2
    rm -f "$resp_file"
    return 1
  fi

  if [[ "$http_code" -ge 300 ]]; then
    echo "Error: ADC credential check failed — GET $list_url returned HTTP $http_code:" >&2
    head -c 500 "$resp_file" >&2
    echo >&2
    rm -f "$resp_file"
    echo "" >&2
    echo "The ADC token was rejected by the Cloud Run v2 API before any resources were created." >&2
    if [[ "$http_code" == "403" ]]; then
      # A 403 means the token parsed fine and the identity simply lacks the
      # role. Re-authenticating as the same principal will not help, so name
      # the permission and the project first.
      echo "HTTP 403 means the credential is valid but its identity is not authorized." >&2
      echo "Fix: grant the ADC identity 'run.instances.list' on project '$project'" >&2
      echo "     (for example roles/run.viewer), or switch to an account that already" >&2
      echo "     has it: run 'gcloud auth application-default login' and retry." >&2
    else
      echo "Fix: run 'gcloud auth application-default login' and retry." >&2
    fi
    return 1
  fi
  rm -f "$resp_file"
  echo "    ADC credential validated successfully."

  # Store token for step 3b (bash dynamic scope — the caller declares
  # _di_adc_token as a local, and this assignment writes to it).
  # NEVER print the full token to stdout.
  _di_adc_token="$tok"
}

# ---------------------------------------------------------------------------
# Gate functions — testable with stub HTTP servers
# ---------------------------------------------------------------------------

# di_assert_perimeter fetches the instance URL with NO credential and requires
# a 302 to accounts.google.com. FAILS if the app answers — this is the guard
# for the single point of failure. With invoker IAM disabled, iapEnabled=false
# leaves the Instance open to the internet with nothing but hub session auth.
#
# This gate doubles as the post-deploy smoke check: if the Instance is dead
# (wrong port, crash loop, missing binary, bad CMD), Cloud Run returns its
# own error (502/503), NOT the IAP 302. So a passing gate 2 proves both
# that IAP is enforcing AND that the Instance is serving behind it.
#
# Do NOT use curl -f. Do NOT use curl -L.
# curl -f exits non-zero on error codes we need to inspect.
# curl -L follows redirects, hiding the 302 we need to see.
#
# Five cases:
#   302 → accounts.google.com, IAP header present:  PASS
#   302 → accounts.google.com, no IAP header:       PASS (redirect alone proves IAP)
#   200 (app answers directly):                     FAIL — UNPROTECTED
#   302 to anywhere else:                           FAIL — not to accounts.google.com
#   502 or 503 (Cloud Run error page):              FAIL — not be serving, CMD
#
# Arguments: instance_url
# Returns 0 on pass, 1 on fail (with diagnostic on stderr).
di_assert_perimeter() {
  local instance_url="$1"
  local headers_file
  headers_file="$(mktemp)"

  local status_code
  status_code="$(curl -s -o /dev/null -D "$headers_file" -w "%{http_code}" \
    --max-time 15 \
    "$instance_url" 2>/dev/null)" || status_code="000"

  if [[ "$status_code" == "000" ]]; then
    rm -f "$headers_file"
    echo "SECURITY FAILURE: could not reach instance URL $instance_url" >&2
    return 1
  fi

  if [[ "$status_code" != "302" ]]; then
    rm -f "$headers_file"
    if [[ "$status_code" == "502" ]] || [[ "$status_code" == "503" ]]; then
      echo "SECURITY FAILURE: expected 302 redirect but got $status_code — the instance may not be serving (check Dockerfile CMD, port configuration, and container logs). Cloud Run returns $status_code when the container is unhealthy or not listening on port 8080" >&2
    else
      echo "SECURITY FAILURE: expected 302 redirect but got $status_code — IAP may not be enforcing! An unauthenticated request reached the app, which means the instance is UNPROTECTED" >&2
    fi
    return 1
  fi

  # Got 302 — verify it points to accounts.google.com
  # || location="" prevents set -e from killing the script when grep finds
  # no Location header. The downstream check still fails closed.
  local location
  location="$(grep -i '^location:' "$headers_file" | head -1 | sed 's/^[Ll]ocation:[[:space:]]*//' | tr -d '\r')" || location=""
  rm -f "$headers_file"

  if [[ "$location" != *"accounts.google.com"* ]]; then
    echo "SECURITY FAILURE: got 302 but not to accounts.google.com (Location: $location) — IAP may not be enforcing" >&2
    return 1
  fi

  return 0
}

# di_wait_for_iap polls the instance URL with an unauthenticated HTTP client
# until IAP responds with a 302 to accounts.google.com.
# Arguments: instance_url [max_wait_seconds] [poll_interval_seconds]
# Returns 0 on success, 1 on timeout.
di_wait_for_iap() {
  local instance_url="$1"
  local max_wait="${2:-180}"   # default 3 minutes
  local poll_interval="${3:-5}"
  local elapsed=0
  local last_status=""
  local headers_file
  headers_file="$(mktemp)"

  while [[ $elapsed -lt $max_wait ]]; do
    local probe_code
    probe_code="$(curl -s -o /dev/null -D "$headers_file" -w "%{http_code}" \
      --max-time 15 \
      "$instance_url" 2>/dev/null)" || probe_code="000"

    if [[ "$probe_code" == "302" ]]; then
      # || location="" prevents set -e from killing the deploy when grep
      # finds no Location header during a transient polling iteration.
      local location
      location="$(grep -i '^location:' "$headers_file" | head -1 | sed 's/^[Ll]ocation:[[:space:]]*//' | tr -d '\r')" || location=""
      if [[ "$location" == *"accounts.google.com"* ]]; then
        rm -f "$headers_file"
        return 0
      fi
    fi

    last_status="$probe_code"
    if [[ "$probe_code" == "000" ]]; then
      echo "    Polling... (not ready yet: connection failed)" >&2
    else
      echo "    Polling... (status $probe_code, waiting for IAP 302)" >&2
    fi
    sleep "$poll_interval"
    elapsed=$((elapsed + poll_interval))
  done
  rm -f "$headers_file" 2>/dev/null

  local diag="timed out after ${max_wait}s waiting for IAP to enforce on $instance_url"
  if [[ "$last_status" == "502" ]] || [[ "$last_status" == "503" ]]; then
    diag="$diag — last seen: $last_status (instance may not be serving — check CMD, port, and container health)"
  elif [[ "$last_status" == "000" ]]; then
    diag="$diag — last probe: connection failed (instance may not be serving on port 8080)"
  elif [[ -n "$last_status" ]]; then
    diag="$diag — last seen: HTTP $last_status"
  fi
  echo "Error: $diag" >&2
  return 1
}

# ---------------------------------------------------------------------------
# Main function — orchestrates all eight steps
# ---------------------------------------------------------------------------

di_main() {
  set -euo pipefail

  # Defaults (must match the Go command's defaults byte-for-byte)
  local DI_NAME=""
  local DI_IMAGE=""
  local DI_PROJECT=""
  local DI_REGION="us-east4"
  local DI_ADMIN_EMAIL=""
  local DI_SERVICE_ACCOUNT=""
  local DI_MEMORY="8Gi"
  local DI_CPU="4"
  local DI_IMAGE_REGISTRY=""

  # Parse flags
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --name)           DI_NAME="$2"; shift 2 ;;
      --image)          DI_IMAGE="$2"; shift 2 ;;
      --project)        DI_PROJECT="$2"; shift 2 ;;
      --region)         DI_REGION="$2"; shift 2 ;;
      --admin-email)    DI_ADMIN_EMAIL="$2"; shift 2 ;;
      --service-account) DI_SERVICE_ACCOUNT="$2"; shift 2 ;;
      --memory)         DI_MEMORY="$2"; shift 2 ;;
      --cpu)            DI_CPU="$2"; shift 2 ;;
      --image-registry) DI_IMAGE_REGISTRY="$2"; shift 2 ;;
      --help|-h)
        sed -n '/^# deploy\.sh/,/^[^#]/{ /^#/s/^# \?//p }' "${BASH_SOURCE[0]}"
        return 0
        ;;
      *)
        echo "Error: unknown flag: $1" >&2
        return 1
        ;;
    esac
  done

  # Validate required flags
  local missing=()
  [[ -z "$DI_NAME" ]]    && missing+=("--name")
  [[ -z "$DI_IMAGE" ]]   && missing+=("--image")
  [[ -z "$DI_PROJECT" ]] && missing+=("--project")
  if [[ ${#missing[@]} -gt 0 ]]; then
    echo "Error: missing required flag(s): ${missing[*]}" >&2
    return 1
  fi

  # ===================================================================
  # Preflight: verify gcloud has 'beta run instances' before any side
  # effects. Older SDKs lack this noun entirely and gcloud's own advice
  # ("Try: gcloud alpha run instances") produces a broken Instance.
  # ===================================================================
  if ! di_check_gcloud_instances; then
    return 1
  fi

  # ===================================================================
  # Step 1: Resolve identity
  # ===================================================================
  echo "==> Step 1: Resolving deployer identity..."
  local operator_email
  operator_email="$(gcloud config get account 2>/dev/null | tr -d '[:space:]')" || {
    echo "Error: 'gcloud config get account' failed — is gcloud configured and authenticated?" >&2
    return 1
  }
  if [[ -z "$operator_email" ]]; then
    echo "Error: gcloud returned empty account — is gcloud configured?" >&2
    return 1
  fi
  echo "    Deployer: $operator_email"

  # Determine admin email: use --admin-email override if provided, otherwise operator
  local admin_email="$operator_email"
  if [[ -n "$DI_ADMIN_EMAIL" ]]; then
    admin_email="$DI_ADMIN_EMAIL"
    echo "    Admin override: $admin_email"
  fi

  # Guard: comma in admin email breaks gcloud --set-env-vars
  if ! di_validate_admin_email "$admin_email"; then
    return 1
  fi

  # ===================================================================
  # Step 2: Resolve project number
  # ===================================================================
  echo "==> Step 2: Resolving project number..."
  local project_number
  project_number="$(gcloud projects describe "$DI_PROJECT" --format='value(projectNumber)' 2>/dev/null | tr -d '[:space:]')" || {
    echo "Error: 'gcloud projects describe $DI_PROJECT' failed — check that the project exists and you have access" >&2
    return 1
  }
  if [[ -z "$project_number" ]]; then
    echo "Error: gcloud returned empty project number for '$DI_PROJECT'" >&2
    return 1
  fi
  if ! di_validate_project_number "$project_number"; then
    return 1
  fi
  echo "    Project: $DI_PROJECT (number: $project_number)"

  # Compute the IAP audience and instance URL.
  local iap_audience
  iap_audience="$(di_build_iap_audience "$project_number" "$DI_REGION" "$DI_NAME")"
  local instance_url
  instance_url="$(di_build_instance_url "$DI_NAME" "$project_number" "$DI_REGION")"

  # Validate the computed URL before using it in gates and deploy.
  if ! di_validate_instance_url "$instance_url"; then
    return 1
  fi

  # Resolve the image registry: --image-registry override, or derived from --image.
  # The broker requires SCION_IMAGE_REGISTRY to pull agent images.
  local image_registry="$DI_IMAGE_REGISTRY"
  if [[ -z "$image_registry" ]]; then
    if ! image_registry="$(di_derive_registry "$DI_IMAGE")"; then
      echo "Use --image-registry to set it explicitly" >&2
      return 1
    fi
  fi
  echo "    Image registry: $image_registry"

  # ===================================================================
  # Preflight: Validate REST credential (ADC) before any mutations
  # The REST PATCH in step 3b requires an Application Default Credential.
  # If the token cannot be minted or the API rejects it, abort NOW —
  # before step 3a creates an Instance that 3b cannot configure.
  # ===================================================================
  echo "==> Preflight: Validating REST credential (ADC)..."

  local _di_adc_token=""
  if ! di_preflight_rest_credential "$operator_email" "$DI_REGION" "$DI_PROJECT"; then
    return 1
  fi

  # ===================================================================
  # Step 3a: Create/update the Instance via gcloud (v1 surface)
  # gcloud speaks v1, which is the ONLY surface that has sandboxLauncher.
  # REST v2 neither sets nor returns sandboxLauncher.
  # ===================================================================
  echo "==> Step 3a: Creating/updating Cloud Run Instance (gcloud, v1 surface)..."

  local gcloud_args=(
    beta run instances deploy "$DI_NAME"
    --image "$DI_IMAGE"
    --sandbox-launcher
    --region "$DI_REGION"
    --project "$DI_PROJECT"
    --set-env-vars "SCION_SERVER_MODE=hosted,SCION_SERVER_AUTH_MODE=proxy,SCION_SERVER_AUTH_PROXY_PROVIDER=iap,SCION_SERVER_AUTH_PROXY_IAP_AUDIENCE=${iap_audience},SCION_SERVER_HUB_ADMINEMAILS=${admin_email},SCION_IMAGE_REGISTRY=${image_registry}"
  )

  if [[ -n "$DI_SERVICE_ACCOUNT" ]]; then
    gcloud_args+=(--service-account "$DI_SERVICE_ACCOUNT")
  fi
  if [[ -n "$DI_MEMORY" ]]; then
    gcloud_args+=(--memory "$DI_MEMORY")
  fi
  if [[ -n "$DI_CPU" ]]; then
    gcloud_args+=(--cpu "$DI_CPU")
  fi

  echo "    gcloud ${gcloud_args[*]:0:6}"
  gcloud "${gcloud_args[@]}"
  echo "    Instance deployed successfully."

  # ===================================================================
  # Step 3b: Enable IAP via REST v2 PATCH
  # iapEnabled and invokerIamDisabled are v2-only fields, so we flip both
  # booleans with a single REST PATCH.
  #
  # DO NOT replace this with a gcloud flag. A --iap flag DOES exist in
  # gcloud 582, but it is registered on the SERVICES surface only
  # ('gcloud run deploy', 'gcloud run services update') and NOT on the
  # 'run instances' noun this script uses. Confusingly,
  # 'gcloud beta run instances deploy --help' describes --public as
  # "Equivalent to setting --no-invoker-iam-check and --no-iap", naming a
  # flag that surface does not expose. Grepping the help text will suggest
  # the PATCH is removable; it is not. The PATCH is the only way to enable
  # IAP on an Instance, and without it the tier's whole auth model is off.
  #
  # This PATCH is safe because it uses updateMask to touch ONLY the IAP
  # booleans, leaving all v1-only fields (like sandboxLauncher) untouched.
  #
  # Invariant: invokerIamDisabled: true is NEVER sent without iapEnabled: true.
  # ===================================================================
  echo "==> Step 3b: Enabling IAP (REST v2 PATCH)..."

  # Reuse the ADC token validated in preflight — never mint twice.
  local access_token
  access_token="$_di_adc_token"
  if [[ -z "$access_token" ]]; then
    echo "Error: ADC token from preflight is empty — this should not happen" >&2
    return 1
  fi

  local patch_url
  patch_url="$(di_build_iap_patch_url "$DI_REGION" "$DI_PROJECT" "$DI_NAME")"
  echo "    PATCH $patch_url"

  local patch_resp_file
  patch_resp_file="$(mktemp)"
  # shellcheck disable=SC2064
  trap "rm -f '$patch_resp_file'" RETURN
  local http_code
  http_code="$(curl -s -o "$patch_resp_file" -w "%{http_code}" \
    -X PATCH \
    -H "Authorization: Bearer ${access_token}" \
    -H "Content-Type: application/json" \
    -d "$(di_iap_patch_body)" \
    "$patch_url")" || {
    echo "Error: failed to connect to $patch_url — check network connectivity and project configuration" >&2
    return 1
  }

  if [[ "$http_code" -ge 300 ]]; then
    echo "Error: REST PATCH returned $http_code:" >&2
    head -c 500 "$patch_resp_file" >&2
    echo >&2
    return 1
  fi
  echo "    IAP enabled on instance."

  # ===================================================================
  # Step 4: Gate 1 — Wait for IAP reconcile
  # 30-75s observed. The API returns before enforcement is live.
  # ===================================================================
  echo "==> Step 4: Waiting for IAP enforcement to activate..."
  echo "    (This typically takes 30-75 seconds)"

  if ! di_wait_for_iap "$instance_url"; then
    return 1
  fi
  echo "    IAP enforcement is active."

  # ===================================================================
  # Step 5: Bind IAP access policy at the region level
  # ===================================================================
  echo "==> Step 5: Binding IAP access policy..."

  local member_prefix
  member_prefix="$(di_iam_member_prefix "$operator_email")"
  gcloud iap web add-iam-policy-binding \
    "--project=${DI_PROJECT}" \
    "--region=${DI_REGION}" \
    --resource-type=cloud-run \
    "--member=${member_prefix}${operator_email}" \
    --role=roles/iap.httpsResourceAccessor
  echo "    IAP access granted to $operator_email"

  # If admin-email differs from operator, also bind for the admin
  if [[ -n "$DI_ADMIN_EMAIL" ]] && [[ "$DI_ADMIN_EMAIL" != "$operator_email" ]]; then
    local admin_member_prefix
    admin_member_prefix="$(di_iam_member_prefix "$DI_ADMIN_EMAIL")"
    gcloud iap web add-iam-policy-binding \
      "--project=${DI_PROJECT}" \
      "--region=${DI_REGION}" \
      --resource-type=cloud-run \
      "--member=${admin_member_prefix}${DI_ADMIN_EMAIL}" \
      --role=roles/iap.httpsResourceAccessor
    echo "    IAP access granted to $DI_ADMIN_EMAIL"
  fi

  # ===================================================================
  # Step 6: Read back and print effective access
  # Both project-level and region-level, because project-level grants
  # inherit invisibly.
  # ===================================================================
  echo "==> Step 6: Reading effective access..."

  echo "    --- Region-level IAP bindings ---"
  local region_policy
  region_policy="$(gcloud iap web get-iam-policy \
    "--project=${DI_PROJECT}" \
    "--region=${DI_REGION}" \
    --resource-type=cloud-run 2>/dev/null)" || true
  if [[ -n "$region_policy" ]]; then
    while IFS= read -r line; do echo "    $line"; done <<< "$region_policy"
  else
    echo "    (no bindings)"
  fi

  echo "    --- Project-level IAP bindings (inherited) ---"
  local project_policy
  project_policy="$(gcloud projects get-iam-policy "$DI_PROJECT" \
    --format=yaml 2>/dev/null)" || true
  if [[ -n "$project_policy" ]]; then
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
  # ===================================================================
  echo "==> Step 7: Asserting IAP perimeter enforcement..."

  if ! di_assert_perimeter "$instance_url"; then
    return 1
  fi
  echo "    IAP perimeter verified: unauthenticated requests are blocked."
  echo "    Instance is serving and IAP-protected."

  # ===================================================================
  # Step 8: Print the URL
  # ===================================================================
  echo ""
  echo "=== Deploy Complete ==="
  echo "Instance URL: $instance_url"
  echo "Admin email:  $admin_email"
  echo ""
  echo "Open the URL in a browser to log in. The deployer is seeded as admin."
}

# ---------------------------------------------------------------------------
# Main guard: only run when executed directly, not when sourced.
# Sourcing this file has NO side effects.
# ---------------------------------------------------------------------------
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  di_main "$@"
fi
