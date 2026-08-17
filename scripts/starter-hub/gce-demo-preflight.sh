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

# scripts/starter-hub/gce-demo-preflight.sh - Validate prerequisites before deploying
#
# Checks local tools, gcloud auth, environment file, GCP APIs, IAM permissions,
# and DNS readiness. Collects all errors before reporting so the operator can fix
# everything in one pass.
#
# Usage:
#   ./scripts/starter-hub/gce-demo-preflight.sh          # standalone
#   Called automatically as Step 0 by gce-demo-deploy.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hub-config.sh
source "${SCRIPT_DIR}/hub-config.sh"

# ---------------------------------------------------------------------------
# Formatting
# ---------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

ERRORS=0
WARNINGS=0

section() {
    echo ""
    echo -e "${BOLD}${BLUE}--- $1 ---${NC}"
}

check_pass() {
    echo -e "  ${GREEN}[PASS]${NC} $1"
}

check_fail() {
    echo -e "  ${RED}[FAIL]${NC} $1"
    if [[ -n "${2:-}" ]]; then
        echo -e "         ${RED}Fix:${NC} $2"
    fi
    ERRORS=$((ERRORS + 1))
}

check_warn() {
    echo -e "  ${YELLOW}[WARN]${NC} $1"
    WARNINGS=$((WARNINGS + 1))
}

# Extract a variable's value from the env file without sourcing it.
get_env_value() {
    local key="$1"
    local file="$2"
    local raw
    raw=$(grep -E "^${key}=" "$file" 2>/dev/null | head -1 | cut -d'=' -f2-)
    # Strip surrounding quotes
    raw="${raw#\"}"
    raw="${raw%\"}"
    raw="${raw#\'}"
    raw="${raw%\'}"
    echo "$raw"
}

# Return 0 (true) if the value is empty or still a placeholder from the sample.
is_placeholder() {
    local val="$1"
    [[ -z "$val" ]] && return 0
    [[ "$val" == your-* ]] && return 0
    [[ "$val" == *example.com* ]] && return 0
    [[ "$val" == *"<generate-with"* ]] && return 0
    return 1
}

echo -e "${BOLD}=== Scion Hub Preflight: ${HUB_NAME} ===${NC}"

# ---------------------------------------------------------------------------
# 1. Local Tools
# ---------------------------------------------------------------------------
section "Local Tools"

for tool in gcloud git openssl; do
    if command -v "$tool" &>/dev/null; then
        check_pass "$tool"
    else
        case "$tool" in
            gcloud)  check_fail "$tool not found" "Install Google Cloud SDK: https://cloud.google.com/sdk/docs/install" ;;
            git)     check_fail "$tool not found" "Install git: https://git-scm.com/downloads" ;;
            openssl) check_fail "$tool not found" "Install openssl (needed to generate SESSION_SECRET)" ;;
        esac
    fi
done

# ---------------------------------------------------------------------------
# 2. gcloud Authentication & Project
# ---------------------------------------------------------------------------
section "gcloud Authentication & Project"

ACTIVE_ACCOUNT=""
if command -v gcloud &>/dev/null; then
    ACTIVE_ACCOUNT=$(gcloud auth list --filter="status:ACTIVE" --format="value(account)" 2>/dev/null | head -1)
fi

if [[ -n "$ACTIVE_ACCOUNT" ]]; then
    check_pass "Authenticated as ${ACTIVE_ACCOUNT}"
else
    check_fail "No active gcloud account" "Run: gcloud auth login"
fi

if [[ -n "$PROJECT_ID" ]]; then
    check_pass "PROJECT_ID = ${PROJECT_ID}"
else
    check_fail "PROJECT_ID is not set" "Run: gcloud config set project YOUR_PROJECT_ID"
fi

if [[ -n "$PROJECT_ID" ]]; then
    if gcloud projects describe "${PROJECT_ID}" --format="value(projectId)" &>/dev/null; then
        check_pass "Project ${PROJECT_ID} is accessible"
    else
        check_fail "Project ${PROJECT_ID} not found or not accessible" \
            "Check the project ID and ensure your account has access"
    fi
fi

# ---------------------------------------------------------------------------
# 3. Environment File
# ---------------------------------------------------------------------------
section "Environment File (${HUB_ENV_FILE})"

if [[ ! -f "${HUB_ENV_FILE}" ]]; then
    check_fail "File not found: ${HUB_ENV_FILE}" \
        "Create it:\n         mkdir -p .scratch\n         cp scripts/starter-hub/hub.env.sample ${HUB_ENV_FILE}\n         # Then edit ${HUB_ENV_FILE} with your values"
else
    check_pass "File exists"

    REQUIRED_VARS=(
        SCION_HUB_STORAGE_BUCKET
        SESSION_SECRET
        SCION_SERVER_BASE_URL
        SCION_IMAGE_REGISTRY
    )

    OAUTH_VARS=(
        SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTID
        SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTSECRET
        SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTID
        SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTSECRET
        SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTID
        SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTSECRET
        SCION_SERVER_OAUTH_CLI_GITHUB_CLIENTID
        SCION_SERVER_OAUTH_CLI_GITHUB_CLIENTSECRET
        SCION_HUB_ENDPOINT
    )

    PROJECT_VARS=(
        SCION_GCP_PROJECT_ID
        GOOGLE_CLOUD_PROJECT
    )

    for var in "${REQUIRED_VARS[@]}"; do
        val=$(get_env_value "$var" "${HUB_ENV_FILE}")
        if is_placeholder "$val"; then
            hint="Set ${var} in ${HUB_ENV_FILE}"
            if [[ "$var" == "SESSION_SECRET" ]]; then
                hint="Generate with: openssl rand -base64 32"
            fi
            check_fail "${var} is missing or still a placeholder" "$hint"
        else
            check_pass "${var}"
        fi
    done

    for var in "${OAUTH_VARS[@]}"; do
        val=$(get_env_value "$var" "${HUB_ENV_FILE}")
        if is_placeholder "$val"; then
            check_warn "${var} is missing or still a placeholder (authentication will be disabled for this provider)"
        else
            check_pass "${var}"
        fi
    done

    for var in "${PROJECT_VARS[@]}"; do
        val=$(get_env_value "$var" "${HUB_ENV_FILE}")
        if is_placeholder "$val"; then
            check_warn "${var} is missing or still a placeholder (telemetry may be disabled)"
        else
            check_pass "${var}"
        fi
    done

    # Optional vars — warn if missing
    for var in SCION_SERVER_AUTH_AUTHORIZEDDOMAINS SCION_SERVER_HUB_ADMINEMAILS; do
        val=$(get_env_value "$var" "${HUB_ENV_FILE}")
        if [[ -z "$val" ]]; then
            check_warn "${var} is not set (optional — see hub.env.sample)"
        else
            check_pass "${var}"
        fi
    done
fi

# ---------------------------------------------------------------------------
# 4. GCP APIs
# ---------------------------------------------------------------------------
section "GCP APIs"

if [[ -n "$PROJECT_ID" ]]; then
    # Get enabled services as a clean list of IDs.
    ENABLED_APIS=$(gcloud services list --enabled --project "${PROJECT_ID}" --format="value(config.name)" 2>/dev/null || true)

    # Critical APIs required for the infrastructure provisioning itself.
    # Failure to have these enabled often indicates a permissions issue or 
    # an uninitialized project.
    CRITICAL_APIS=(
        compute.googleapis.com
        iam.googleapis.com
        iamcredentials.googleapis.com
        serviceusage.googleapis.com
    )
    
    # Application-level APIs that the provisioning script will attempt to enable.
    # We warn if missing so the user is aware of what will be activated.
    PROVISIONABLE_APIS=(
        cloudtrace.googleapis.com
        monitoring.googleapis.com
        logging.googleapis.com
        secretmanager.googleapis.com
        dns.googleapis.com
    )
    if [[ "${ENABLE_GKE}" == "true" ]]; then
        PROVISIONABLE_APIS+=(container.googleapis.com)
    fi

    for api in "${CRITICAL_APIS[@]}"; do
        if echo "${ENABLED_APIS}" | grep -Fqx "${api}" >/dev/null; then
            check_pass "${api}"
        else
            check_fail "${api} is not enabled" \
                "gcloud services enable ${api} --project ${PROJECT_ID}"
        fi
    done

    for api in "${PROVISIONABLE_APIS[@]}"; do
        if echo "${ENABLED_APIS}" | grep -Fqx "${api}" >/dev/null; then
            check_pass "${api}"
        else
            check_warn "${api} is not enabled (gce-demo-provision.sh will attempt to enable it)"
        fi
    done
else
    check_warn "Skipping API checks (PROJECT_ID not set)"
fi

# ---------------------------------------------------------------------------
# 5. GCP IAM Permissions
# ---------------------------------------------------------------------------
section "GCP IAM Permissions"

if [[ -n "$PROJECT_ID" ]] && [[ -n "$ACTIVE_ACCOUNT" ]]; then
    # Probe with lightweight read-only commands
    if gcloud compute instances list --project "${PROJECT_ID}" --limit=1 &>/dev/null; then
        check_pass "Compute Engine access"
    else
        check_warn "Could not verify Compute Engine access (may be missing roles/compute.admin or API is disabled)"
    fi

    if gcloud iam service-accounts list --project "${PROJECT_ID}" --limit=1 &>/dev/null; then
        check_pass "IAM service account access"
    else
        check_warn "Could not verify IAM access (may be missing roles/iam.admin)"
    fi

    if gcloud dns managed-zones list --project "${PROJECT_ID}" --limit=1 &>/dev/null; then
        check_pass "Cloud DNS access"
    else
        check_warn "Could not verify Cloud DNS access (may be missing roles/dns.admin)"
    fi

    if [[ "${ENABLE_GKE}" == "true" ]]; then
        if gcloud container clusters list --project "${PROJECT_ID}" --limit=1 &>/dev/null; then
            check_pass "GKE access"
        else
            check_warn "Could not verify GKE access (may be missing roles/container.admin)"
        fi
    fi
else
    check_warn "Skipping IAM checks (PROJECT_ID or active account not set)"
fi

# ---------------------------------------------------------------------------
# 6. DNS Zone
# ---------------------------------------------------------------------------
section "DNS Zone"

if [[ -n "$PROJECT_ID" ]]; then
    if gcloud dns managed-zones describe "${DNS_ZONE_NAME}" --project "${PROJECT_ID}" &>/dev/null; then
        check_pass "Cloud DNS zone ${DNS_ZONE_NAME} exists"
    else
        check_warn "Cloud DNS zone ${DNS_ZONE_NAME} does not exist yet (gce-certs.sh will create it)"
        echo -e "         Ensure NS records for ${CERT_DOMAIN} are delegated to Google Cloud DNS at your registrar"
    fi
else
    check_warn "Skipping DNS check (PROJECT_ID not set)"
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
if [[ $ERRORS -gt 0 ]]; then
    echo -e "${BOLD}${RED}Preflight failed: ${ERRORS} error(s), ${WARNINGS} warning(s)${NC}"
    echo "Fix the errors above and re-run this script."
    exit 1
else
    if [[ $WARNINGS -gt 0 ]]; then
        echo -e "${BOLD}${GREEN}Preflight passed${NC} with ${YELLOW}${WARNINGS} warning(s)${NC}"
    else
        echo -e "${BOLD}${GREEN}Preflight passed: all checks OK${NC}"
    fi
    exit 0
fi
