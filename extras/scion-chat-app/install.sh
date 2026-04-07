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

# extras/scion-chat-app/install.sh — Install the chat app alongside a
# provisioned Scion Hub (via scripts/starter-hub/).
#
# Idempotent: safe to re-run after hub updates that overwrite the Caddyfile
# or settings.yaml.
#
# Usage:
#   make install          (builds first, then runs this script)
#   ./install.sh          (skip build, install only)

set -euo pipefail

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCION_HOME="/home/scion"
SCION_DIR="${SCION_HOME}/.scion"
INSTALL_BIN="/usr/local/bin"
CADDYFILE="/etc/caddy/Caddyfile"
SETTINGS_FILE="${SCION_DIR}/settings.yaml"
HUB_ENV="${SCION_DIR}/hub.env"
CHAT_ENV="${SCION_DIR}/chat-app.env"
CONFIG_FILE="${SCION_DIR}/scion-chat-app.yaml"
SYSTEMD_UNIT="/etc/systemd/system/scion-chat-app.service"

LISTEN_PORT="${CHAT_APP_LISTEN_PORT:-8443}"

# Temp directory for staging files before installing them.
TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
step()    { echo "=> $*"; }
substep() { echo "   $*"; }

need_file() {
    if [[ ! -f "$1" ]]; then
        echo "ERROR: required file not found: $1" >&2
        echo "       $2" >&2
        exit 1
    fi
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
need_file "${HUB_ENV}" "Run scripts/starter-hub/gce-start-hub.sh --full first."
need_file "${CHAT_ENV}" "Copy extras/scion-chat-app/chat-app.env.sample to ${CHAT_ENV} and fill in values."

# Source env files (hub.env first, chat-app.env may reference hub vars).
set -a
# shellcheck source=/dev/null
source "${HUB_ENV}"
# shellcheck source=/dev/null
source "${CHAT_ENV}"
set +a

# Resolve the hub endpoint — prefer SCION_HUB_ENDPOINT, fall back to
# SCION_SERVER_BASE_URL which is set by the starter-hub provisioning.
SCION_HUB_ENDPOINT="${SCION_HUB_ENDPOINT:-${SCION_SERVER_BASE_URL:-}}"
if [[ -z "${SCION_HUB_ENDPOINT}" ]]; then
    echo "ERROR: neither SCION_HUB_ENDPOINT nor SCION_SERVER_BASE_URL is set in ${HUB_ENV}" >&2
    exit 1
fi

# Derive the external URL from the hub endpoint.
EXTERNAL_URL="${SCION_HUB_ENDPOINT}/chat/events"

# ---------------------------------------------------------------------------
# Chat API preflight — verify the service account can call the Chat API.
# The Google Chat API uses the OAuth scope https://www.googleapis.com/auth/chat.bot
# (not IAM roles). On a GCE VM using ADC, the VM's OAuth scopes must include
# chat.bot — the broad cloud-platform scope does NOT cover Workspace APIs.
# ---------------------------------------------------------------------------
step "Checking Chat API prerequisites"

PREFLIGHT_FAILED=0

# warn_prereq prints a warning block. In interactive mode it prompts to
# continue; in non-interactive mode (e.g. ssh --command) it records the
# failure and continues so the full set of issues is reported at once.
warn_prereq() {
    echo "" >&2
    echo "WARNING: $1" >&2
    shift
    for line in "$@"; do
        echo "   ${line}" >&2
    done
    echo "" >&2
    if [[ -t 0 ]]; then
        read -r -p "Continue installation anyway? [y/N] " REPLY
        if [[ ! "${REPLY}" =~ ^[Yy]$ ]]; then
            exit 1
        fi
    else
        PREFLIGHT_FAILED=1
    fi
}

# 1. Check that the Google Chat API is enabled on the project.
if command -v gcloud &>/dev/null; then
    SVC_OUTPUT="$(gcloud services list --enabled --project="${CHAT_APP_PROJECT_ID}" \
            --filter="name:chat.googleapis.com" --format="value(name)" 2>&1)" \
        && SVC_OK=true || SVC_OK=false

    if [[ "${SVC_OK}" == "true" ]]; then
        if echo "${SVC_OUTPUT}" | grep -q 'chat.googleapis.com'; then
            substep "Google Chat API is enabled on project ${CHAT_APP_PROJECT_ID}"
        else
            warn_prereq \
                "The Google Chat API does not appear to be enabled on project ${CHAT_APP_PROJECT_ID}." \
                "Enable it with:" \
                "  gcloud services enable chat.googleapis.com --project=${CHAT_APP_PROJECT_ID}"
        fi
    else
        substep "Could not query enabled services (permission issue?), skipping API check"
    fi
else
    substep "gcloud CLI not found, skipping API enablement check"
fi

# 2. Check VM OAuth scopes when using ADC (no explicit credentials file).
#    The Google Chat API requires the chat.bot scope. The broad cloud-platform
#    scope does NOT cover Workspace APIs, so we must check for chat.bot
#    specifically.
REQUIRED_SCOPE="https://www.googleapis.com/auth/chat.bot"

if [[ -z "${CHAT_APP_CREDENTIALS:-}" ]]; then
    METADATA_URL="http://metadata.google.internal/computeMetadata/v1"
    if VM_SCOPES="$(curl -sf -H 'Metadata-Flavor: Google' \
            "${METADATA_URL}/instance/service-accounts/default/scopes" 2>/dev/null)"; then
        if ! echo "${VM_SCOPES}" | grep -qF "${REQUIRED_SCOPE}"; then
            SA_EMAIL="$(curl -sf -H 'Metadata-Flavor: Google' \
                "${METADATA_URL}/instance/service-accounts/default/email" 2>/dev/null || echo '<SA_EMAIL>')"
            warn_prereq \
                "The GCE VM's OAuth scopes do not include ${REQUIRED_SCOPE}." \
                "The chat app will fail with 403 ACCESS_TOKEN_SCOPE_INSUFFICIENT at runtime." \
                "The cloud-platform scope does NOT cover Google Workspace APIs like Chat." \
                "" \
                "To fix, stop the VM and update its scopes (run from your workstation):" \
                "  VM_NAME=\$(hostname)   # ${HOSTNAME:-}" \
                "  VM_ZONE=\$(gcloud compute instances list --filter=\"name=\${VM_NAME}\" --format='value(zone)' --project=${CHAT_APP_PROJECT_ID})" \
                "  gcloud compute instances stop \${VM_NAME} --zone=\${VM_ZONE} --project=${CHAT_APP_PROJECT_ID}" \
                "  gcloud compute instances set-service-account \${VM_NAME} --zone=\${VM_ZONE} --project=${CHAT_APP_PROJECT_ID} \\" \
                "    --service-account=${SA_EMAIL} \\" \
                "    --scopes=https://www.googleapis.com/auth/cloud-platform,${REQUIRED_SCOPE}" \
                "  gcloud compute instances start \${VM_NAME} --zone=\${VM_ZONE} --project=${CHAT_APP_PROJECT_ID}" \
                "" \
                "Alternatively, set CHAT_APP_CREDENTIALS in chat-app.env to a service" \
                "account key file path to bypass VM scopes entirely."
        else
            substep "VM OAuth scopes include ${REQUIRED_SCOPE}"
        fi
    else
        substep "Not running on GCE (metadata unavailable), skipping scope check"
    fi
fi

if [[ "${PREFLIGHT_FAILED}" -eq 1 ]]; then
    echo "" >&2
    echo "ERROR: Chat API preflight checks failed (see warnings above)." >&2
    echo "   Fix the issues and re-run the installer, or run interactively to skip." >&2
    exit 1
fi

step "Installing scion-chat-app"

# ---------------------------------------------------------------------------
# 1. Binary
# ---------------------------------------------------------------------------
BINARY="${SCRIPT_DIR}/scion-chat-app"
need_file "${BINARY}" "Run 'make build' first."

substep "Installing binary to ${INSTALL_BIN}"
sudo install -m 755 "${BINARY}" "${INSTALL_BIN}/scion-chat-app"

# ---------------------------------------------------------------------------
# 2. Config file
# ---------------------------------------------------------------------------
substep "Writing config to ${CONFIG_FILE}"
cat > "${TMPDIR}/scion-chat-app.yaml" <<EOF
hub:
  endpoint: "${SCION_HUB_ENDPOINT}"
  user: "${CHAT_APP_HUB_USER}"
  signing_key: "${CHAT_APP_HUB_SIGNING_KEY:-}"
  signing_key_secret: "${CHAT_APP_HUB_SIGNING_KEY_SECRET:-}"

plugin:
  listen_address: "localhost:9090"

platforms:
  google_chat:
    enabled: true
    project_id: "${CHAT_APP_PROJECT_ID}"
    credentials: "${CHAT_APP_CREDENTIALS:-}"
    listen_address: ":${LISTEN_PORT}"
    external_url: "${EXTERNAL_URL}"
    service_account_email: "${CHAT_APP_SERVICE_ACCOUNT_EMAIL:-}"
    command_id_map:
      "1": "scion"

state:
  database: "${SCION_DIR}/scion-chat-app.db"

notifications:
  trigger_activities:
    - COMPLETED
    - WAITING_FOR_INPUT
    - ERROR
    - STALLED
    - LIMITS_EXCEEDED

logging:
  level: "${CHAT_APP_LOG_LEVEL:-info}"
  format: "json"
EOF
sudo install -m 600 -o scion -g scion "${TMPDIR}/scion-chat-app.yaml" "${CONFIG_FILE}"

# ---------------------------------------------------------------------------
# 3. Systemd unit
# ---------------------------------------------------------------------------
substep "Installing systemd unit"
cat > "${TMPDIR}/scion-chat-app.service" <<EOF
[Unit]
Description=Scion Chat App
After=network.target scion-hub.service
Wants=scion-hub.service

[Service]
User=scion
Group=scion
Environment="HOME=${SCION_HOME}"
StandardOutput=journal
StandardError=journal
ExecStart=${INSTALL_BIN}/scion-chat-app -config ${CONFIG_FILE}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
sudo install -m 644 "${TMPDIR}/scion-chat-app.service" "${SYSTEMD_UNIT}"
sudo systemctl daemon-reload
sudo systemctl enable scion-chat-app

# ---------------------------------------------------------------------------
# 4. Patch Caddyfile
# ---------------------------------------------------------------------------
step "Patching Caddyfile"

if [[ ! -f "${CADDYFILE}" ]]; then
    substep "No Caddyfile found at ${CADDYFILE}, skipping"
else
    # Extract the domain line and tls directive from the existing Caddyfile.
    # The starter-hub generates a simple single-site block; we rewrite it
    # to add path-based routing for the chat app.
    DOMAIN="$(head -1 "${CADDYFILE}" | sed 's/ *{$//')"
    TLS_LINE="$(grep '^\s*tls ' "${CADDYFILE}" || true)"

    cat > "${TMPDIR}/Caddyfile" <<EOF
${DOMAIN} {
    handle /chat/* {
        reverse_proxy localhost:${LISTEN_PORT}
    }
    handle {
        reverse_proxy localhost:8080
    }
    ${TLS_LINE}
}
EOF

    if ! diff -q "${TMPDIR}/Caddyfile" "${CADDYFILE}" >/dev/null 2>&1; then
        sudo install -m 644 -o caddy -g caddy "${TMPDIR}/Caddyfile" "${CADDYFILE}"
        sudo systemctl reload caddy
        substep "Caddyfile updated, Caddy reloaded"
    else
        substep "Caddyfile already up to date"
    fi
fi

# ---------------------------------------------------------------------------
# 5. Patch Hub settings.yaml — add broker plugin entry and enable message broker
# ---------------------------------------------------------------------------
step "Patching Hub settings.yaml"

HUB_SETTINGS_CHANGED=0

if [[ ! -f "${SETTINGS_FILE}" ]]; then
    substep "No settings.yaml found at ${SETTINGS_FILE}, skipping"
else
    # 5a. Add the googlechat plugin entry under server.plugins.broker.
    # The plugins key must be nested under "server:" in settings.yaml,
    # NOT at the root level, to match the V1ServerConfig schema.

    # First, fix any stale root-level plugins: block left by earlier
    # install versions that incorrectly placed it outside server:.
    if grep -q '^plugins:' "${SETTINGS_FILE}"; then
        substep "removing stale root-level plugins: block (must be under server:)"
        sudo sed -i '/^plugins:/,/^[^ ]/{ /^[^ ]/!d; /^plugins:/d; }' "${SETTINGS_FILE}"
        HUB_SETTINGS_CHANGED=1
    fi

    # Check if googlechat is correctly nested under server.plugins.
    if grep -qE '^\s+plugins:' "${SETTINGS_FILE}" && grep -q 'googlechat' "${SETTINGS_FILE}"; then
        substep "settings.yaml already has googlechat plugin config under server"
    else
        # Insert under server: (plugins must be a child of server in the schema).
        sudo sed -i '/^\s*server:/a\    plugins:\n        broker:\n            googlechat:\n                self_managed: true\n                address: "localhost:9090"' "${SETTINGS_FILE}"
        substep "settings.yaml updated with googlechat plugin config under server"
        HUB_SETTINGS_CHANGED=1
    fi

    # 5b. Enable the message broker with type "googlechat" so the hub
    # routes outbound messages through the broker plugin.
    # Check for message_broker with enabled+type already configured.
    if grep -q 'message_broker' "${SETTINGS_FILE}" && \
       grep -A2 'message_broker' "${SETTINGS_FILE}" | grep -q 'enabled: true'; then
        substep "settings.yaml already has message_broker enabled"
    else
        # Replace the empty "message_broker: {}" or bare "message_broker:"
        # with the full enabled block. If neither exists, insert under server:.
        if grep -qE '^\s+message_broker:\s*\{\}' "${SETTINGS_FILE}"; then
            sudo sed -i 's/^\(\s*\)message_broker:\s*{}/\1message_broker:\n\1    enabled: true\n\1    type: "googlechat"/' "${SETTINGS_FILE}"
        elif grep -qE '^\s+message_broker:\s*$' "${SETTINGS_FILE}"; then
            # Bare key with no value — add children on subsequent lines.
            sudo sed -i '/^\s*message_broker:\s*$/a\        enabled: true\n        type: "googlechat"' "${SETTINGS_FILE}"
        elif ! grep -q 'message_broker' "${SETTINGS_FILE}"; then
            # No message_broker key at all — insert under server:.
            sudo sed -i '/^\s*server:/a\    message_broker:\n        enabled: true\n        type: "googlechat"' "${SETTINGS_FILE}"
        else
            substep "WARNING: message_broker exists but is not enabled — please verify manually"
        fi
        if grep -A2 'message_broker' "${SETTINGS_FILE}" | grep -q 'enabled: true'; then
            substep "settings.yaml updated: message_broker enabled with type googlechat"
            HUB_SETTINGS_CHANGED=1
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 6. Start / restart
# ---------------------------------------------------------------------------
step "Restarting services"

# Restart the hub first if settings changed (it needs to load the broker
# plugin before the chat app connects).
if [[ "${HUB_SETTINGS_CHANGED}" -eq 1 ]]; then
    substep "Hub settings changed — restarting scion-hub"
    sudo systemctl restart scion-hub
    # Give the hub a moment to start the broker plugin listener before the
    # chat app tries to connect.
    sleep 2
fi

sudo systemctl restart scion-chat-app
substep "Done — check status with: journalctl -u scion-chat-app -f"
