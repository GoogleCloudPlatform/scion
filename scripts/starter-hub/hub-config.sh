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

# scripts/starter-hub/hub-config.sh - Shared configuration for all starter-hub scripts
#
# Set HUB_NAME before sourcing this file to parameterize all scripts for a
# specific deployment. For example:
#
#   export HUB_NAME=staging
#   ./scripts/starter-hub/gce-demo-deploy.sh
#
# All resource names, domains, and file paths are derived from HUB_NAME and
# BASE_DOMAIN. Override any individual variable via the environment if the
# derived default doesn't fit your setup.

# --- Primary Configuration ---
# HUB_NAME drives all resource naming. Defaults to "demo".
HUB_NAME="${HUB_NAME:-demo}"
BASE_DOMAIN="${BASE_DOMAIN:-scion-ai.dev}"

# --- Feature Flags ---
# Set to "false" to skip GKE cluster creation, credential setup, and
# container.admin IAM role. The hub will run with Docker as the default runtime.
ENABLE_GKE="${ENABLE_GKE:-false}"

# --- Derived: GCP Resources ---
INSTANCE_NAME="${INSTANCE_NAME:-scion-${HUB_NAME}}"
SERVICE_ACCOUNT_NAME="${SERVICE_ACCOUNT_NAME:-scion-${HUB_NAME}-sa}"
FIREWALL_RULE="${FIREWALL_RULE:-scion-${HUB_NAME}-allow-http-https}"
CLUSTER_NAME="${CLUSTER_NAME:-scion-${HUB_NAME}-cluster}"

# --- Derived: Domain & DNS ---
# CERT_DOMAIN is the zone used for wildcard certs (e.g., "demo.scion-ai.dev")
CERT_DOMAIN="${CERT_DOMAIN:-${HUB_NAME}.${BASE_DOMAIN}}"
# HUB_DOMAIN is the full hostname for the hub (e.g., "hub.demo.scion-ai.dev")
HUB_DOMAIN="${HUB_DOMAIN:-hub.${CERT_DOMAIN}}"
# DNS_ZONE_NAME is the Cloud DNS managed zone name (e.g., "demo-scion-ai-dev")
DNS_ZONE_NAME="${DNS_ZONE_NAME:-$(echo "${CERT_DOMAIN}" | tr '.' '-')}"

# --- Derived: Region / Zone ---
REGION="${REGION:-us-central1}"
ZONE="${ZONE:-us-central1-a}"

# --- Derived: Project ---
PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"

# --- Derived: Paths ---
HUB_ENV_FILE="${HUB_ENV_FILE:-.scratch/hub-${HUB_NAME}.env}"
REPO_DIR="${REPO_DIR:-/home/scion/scion}"
SCION_BIN="${SCION_BIN:-/usr/local/bin/scion}"

# --- Agent images ---
# IMAGE_REGISTRY is the image_registry gce-start-hub.sh --full writes to the
# Hub's settings.yaml. The Hub's runtime broker refuses to start without one.
# SCION_IMAGE_REGISTRY in hub.env, or a registry saved in the Hub, takes
# precedence over this value.
# The default, localhost/scion, is the local image store of the VM, the value
# the single-node VM deploy script uses. The starter scripts do not build agent
# images: build them on the VM (image-build/README.md), for example from the
# repository checkout with
#   sudo bash image-build/scripts/build-images.sh --builder local-docker \
#     --registry localhost/scion --target all
# or set SCION_IMAGE_REGISTRY in hub.env to a registry that holds them.
# With ENABLE_GKE=true the registry must not be a loopback address: GKE nodes
# cannot pull from the VM's local store.
IMAGE_REGISTRY="${IMAGE_REGISTRY:-localhost/scion}"

# DNS_ZONE_DESCRIPTION is the description gce-certs.sh sets on a new zone.
DNS_ZONE_DESCRIPTION="${DNS_ZONE_DESCRIPTION:-Scion Hub zone for ${CERT_DOMAIN}}"

# --- Feature Flags: TLS ---
# Set to "true" for an internal deployment with no certificates: gce-demo-deploy.sh
# skips gce-certs.sh, and gce-start-hub.sh skips the Caddy/TLS step (same as
# gce-start-hub.sh --no-tls). See gce-start-hub.sh for details.
SKIP_TLS="${SKIP_TLS:-false}"

# --- Shared Defaults ---
GITHUB_REPO="${GITHUB_REPO:-GoogleCloudPlatform/scion}"
# CERT_EMAIL is the contact address Let's Encrypt uses for certificate notices.
# There is no default: set it to your own address before running gce-certs.sh
# or gce-demo-deploy.sh (for example, export CERT_EMAIL=admin@example.com).
CERT_EMAIL="${CERT_EMAIL:-}"
CLOUD_INIT_FILE="${CLOUD_INIT_FILE:-scripts/starter-hub/gce-demo-cloud-init.yaml}"

# --- Shared Helpers ---

# Exit with a clear message when CERT_EMAIL is not set. Call this before any
# step that requests certificates, so the script fails before changing anything.
require_cert_email() {
    if [[ -z "${CERT_EMAIL}" ]]; then
        echo "Error: CERT_EMAIL is not set." >&2
        echo "  Let's Encrypt needs a contact address for certificate notices." >&2
        echo "  Set it to your own address and rerun, for example:" >&2
        echo "    export CERT_EMAIL=admin@example.com" >&2
        echo "  Behind an upstream TLS terminator, set SKIP_TLS=true and HUB_BASE_URL=https://<terminator host> instead." >&2
        exit 1
    fi
}

# hub_env_value KEY prints KEY from HUB_ENV_FILE, parsed as systemd's
# EnvironmentFile= does for these simple cases: the last assignment wins,
# spaces around "=" are allowed, and surrounding whitespace, a trailing CR
# and surrounding quotes are removed. Prints nothing when the file does not
# exist or does not set it. KEY must be a plain variable name.
hub_env_value() {
    local key="$1" raw=""
    if [[ -f "${HUB_ENV_FILE}" ]]; then
        raw=$(grep -E "^[[:space:]]*${key}[[:space:]]*=" "${HUB_ENV_FILE}" 2>/dev/null | tail -n 1 | cut -d'=' -f2- || true)
    fi
    raw="${raw%$'\r'}"
    # Trim surrounding whitespace.
    raw="${raw#"${raw%%[![:space:]]*}"}"
    raw="${raw%"${raw##*[![:space:]]}"}"
    raw="${raw#\"}"
    raw="${raw%\"}"
    raw="${raw#\'}"
    raw="${raw%\'}"
    printf '%s' "$raw"
}

# hub_env_base_url prints SCION_SERVER_BASE_URL from HUB_ENV_FILE (see
# hub_env_value).
hub_env_base_url() {
    hub_env_value SCION_SERVER_BASE_URL
}

# registry_is_valid REGISTRY succeeds when REGISTRY is a plain registry path,
# safe to write into a quoted YAML string: letters, digits and . _ : / -.
registry_is_valid() {
    [[ "$1" =~ ^[A-Za-z0-9._:/-]+$ ]]
}

# registry_is_loopback REGISTRY succeeds when the host of REGISTRY is a
# loopback address. It is the same test as _hybrid_registry_is_loopback in
# scripts/single-node-vm/hybrid-tier.sh, used by deploy.sh to refuse such a
# registry for GKE (tests/run.sh checks the two agree): the host[:port] is
# the part before the first "/", an IPv6 literal's brackets are stripped
# first, then a ":port" suffix, and localhost, 127.*, 0.0.0.0 and ::1 are
# loopback.
registry_is_loopback() {
    local registry="$1" host
    host="${registry%%/*}"
    if [[ "$host" == \[*\]* ]]; then
        host="${host#\[}"
        host="${host%%\]*}"
    else
        host="${host%%:*}"
    fi
    case "$host" in
        localhost|127.*|0.0.0.0|::1) return 0 ;;
        *) return 1 ;;
    esac
}

# effective_image_registry prints the registry the Hub's broker uses after
# --full: SCION_IMAGE_REGISTRY from HUB_ENV_FILE when it sets one (the node
# environment outranks settings.yaml), else IMAGE_REGISTRY. A registry saved
# in the Hub is not visible here.
effective_image_registry() {
    local v
    v="$(hub_env_value SCION_IMAGE_REGISTRY)"
    printf '%s' "${v:-${IMAGE_REGISTRY}}"
}

# require_gke_reachable_registry exits 1 when ENABLE_GKE=true and the
# effective image registry is a loopback address, which GKE nodes cannot
# pull from. Call it before anything is provisioned or uploaded.
require_gke_reachable_registry() {
    local reg
    [[ "${ENABLE_GKE}" == "true" ]] || return 0
    reg="$(effective_image_registry)"
    if registry_is_loopback "$reg"; then
        echo "Error: ENABLE_GKE=true, but the image registry '${reg}' is a loopback address on the VM. GKE nodes cannot pull from it." >&2
        echo "  Set SCION_IMAGE_REGISTRY in ${HUB_ENV_FILE} (or IMAGE_REGISTRY) to a registry the cluster can pull from, for example us-docker.pkg.dev/<project>/<repo>." >&2
        exit 1
    fi
}

# require_no_tls_https_base_url checks the base URL that will take effect in
# a --no-tls (SKIP_TLS=true) deployment, before anything is provisioned or
# uploaded, and exits 1 with an explanation when it is not https. Without
# Caddy the hub listens on 0.0.0.0:8080 itself, and it refuses to start
# with an http base URL on a non-loopback listener, so the base URL must be
# the https:// URL of the TLS terminator in front of it. A
# SCION_SERVER_BASE_URL in HUB_ENV_FILE wins over the unit's value
# (EnvironmentFile= overrides Environment=), so that value is the one
# checked when the file sets it; if HUB_BASE_URL is also set, the two must
# agree. On success HUB_BASE_URL holds the effective URL.
require_no_tls_https_base_url() {
    local file_url url source
    file_url="$(hub_env_base_url)"
    url="${HUB_BASE_URL:-}"
    source="HUB_BASE_URL"
    if [[ -n "$file_url" ]]; then
        if [[ -n "$url" && "$url" != "$file_url" ]]; then
            echo "Error: HUB_BASE_URL (${url}) and SCION_SERVER_BASE_URL in ${HUB_ENV_FILE} (${file_url}) differ." >&2
            echo "  The hub.env value takes effect (it overrides the systemd unit's value). Make the two agree, or unset one." >&2
            exit 1
        fi
        url="$file_url"
        source="SCION_SERVER_BASE_URL in ${HUB_ENV_FILE}"
    fi
    case "$url" in
        [Hh][Tt][Tt][Pp][Ss]://*) ;;
        *)
            echo "Error: --no-tls (SKIP_TLS=true) needs HUB_BASE_URL set to the https:// URL of the TLS terminator in front of the hub${url:+ (got ${url} from ${source})}." >&2
            echo "  The hub refuses to start with an http base URL on a non-loopback listener: OAuth callbacks and session cookies would cross the network unencrypted." >&2
            echo "  Set HUB_BASE_URL=https://<terminator host> (or SCION_SERVER_BASE_URL in ${HUB_ENV_FILE}), or drop --no-tls / SKIP_TLS to let Caddy terminate TLS." >&2
            exit 1
            ;;
    esac
    HUB_BASE_URL="$url"
}

# Wait for the instance to be reachable via SSH and for cloud-init to finish.
# Call this before the first SSH-dependent step after provisioning.
wait_for_cloud_init() {
    echo "=== Waiting for VM to be ready (SSH + cloud-init) ==="
    local max_wait=600  # 10 minutes
    local interval=15
    local elapsed=0

    while (( elapsed < max_wait )); do
        local result
        result=$(gcloud compute ssh "${INSTANCE_NAME}" \
            --project="${PROJECT_ID}" \
            --zone="${ZONE}" \
            --ssh-flag="-o ConnectTimeout=10" \
            --command "cloud-init status 2>/dev/null || echo 'status: unknown'" \
            2>/dev/null) || result="SSH_UNREACHABLE"

        if [[ "$result" == "SSH_UNREACHABLE" ]]; then
            echo "  -> SSH not available yet... (${elapsed}s elapsed)"
        elif echo "$result" | grep -q "status: done"; then
            echo "  -> VM ready: cloud-init complete (${elapsed}s elapsed)"
            return 0
        elif echo "$result" | grep -q "status: error"; then
            echo "  -> Warning: cloud-init finished with errors (${elapsed}s elapsed)"
            echo "     Check: sudo cat /var/log/cloud-init-output.log"
            return 0
        else
            local status_text
            status_text=$(echo "$result" | head -1)
            echo "  -> SSH available, cloud-init: ${status_text} (${elapsed}s elapsed)"
        fi

        sleep "$interval"
        elapsed=$(( elapsed + interval ))
    done

    echo "Error: VM did not become ready after ${max_wait}s"
    return 1
}
