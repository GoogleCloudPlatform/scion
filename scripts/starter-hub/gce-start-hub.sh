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

# scripts/starter-hub/gce-start-hub.sh - Build and start Scion Hub on GCE with Caddy Reverse Proxy
#
# Usage: scripts/starter-hub/gce-start-hub.sh [--full] [--reset-db]
#
#   Default (fast): push → pull → build → restart → health check
#   --full:         Also uploads config files, installs Caddy, updates systemd/Caddy
#   --reset-db:     Deletes the hub database before starting (works in both modes)

set -euo pipefail

INSTANCE_NAME="scion-demo"
ZONE=${ZONE:-"us-central1-a"}
PROJECT_ID=${PROJECT_ID:-"deploy-demo-test"}
DOMAIN="hub.demo.scion-ai.dev"
REPO_DIR="/home/scion/scion-agent"
SCION_BIN="/usr/local/bin/scion"
RESET_DB=false
FULL_DEPLOY=false

# --- Timing & Reporting Helpers ---

STEP_NUM=0
TOTAL_STEPS=0
STEP_START=0
declare -a STEP_NAMES=()
declare -a STEP_TIMES=()

step() {
    local now=$SECONDS
    # Record elapsed time for the previous step
    if (( STEP_NUM > 0 )); then
        STEP_TIMES+=( $(( now - STEP_START )) )
    fi
    STEP_NUM=$(( STEP_NUM + 1 ))
    STEP_START=$now
    STEP_NAMES+=( "$1" )
    echo ""
    echo "==> [${STEP_NUM}/${TOTAL_STEPS}] $1"
}

substep() {
    echo "  -> $1"
}

print_summary() {
    local now=$SECONDS
    # Capture final step time
    if (( STEP_NUM > 0 )); then
        STEP_TIMES+=( $(( now - STEP_START )) )
    fi
    local total=$SECONDS
    echo ""
    echo "=== Deploy completed in ${total}s ==="
    for i in "${!STEP_NAMES[@]}"; do
        printf "  %-34s %3ss\n" "${STEP_NAMES[$i]}" "${STEP_TIMES[$i]}"
    done
}

# --- Argument Parsing ---

while [[ $# -gt 0 ]]; do
    case $1 in
        --reset-db)
            RESET_DB=true
            shift
            ;;
        --full)
            FULL_DEPLOY=true
            shift
            ;;
        *)
            echo "Unknown option: $1"
            echo "Usage: $0 [--full] [--reset-db]"
            exit 1
            ;;
    esac
done

if [[ -z "$PROJECT_ID" ]]; then
    echo "Error: PROJECT_ID is not set and could not be determined from gcloud config."
    exit 1
fi

# Compute total steps based on mode
if $FULL_DEPLOY; then
    TOTAL_STEPS=4  # push, upload, remote-session, remote-health
    echo "=== Full Deploy: Scion Hub on ${INSTANCE_NAME} ==="
else
    TOTAL_STEPS=3  # push, remote-session, remote-health
    echo "=== Fast Deploy: Scion Hub on ${INSTANCE_NAME} ==="
fi

# --- Step: Push ---

step "Pushing to origin..."
git push origin main

# --- Step: Upload config files (full mode only) ---

if $FULL_DEPLOY; then
    step "Uploading config files..."

    # Prepare all temp files locally
    UPLOAD_DIR=$(mktemp -d)
    trap "rm -rf $UPLOAD_DIR" EXIT

    # hub.env
    HAS_HUB_ENV=false
    if [ -f ".scratch/hub.env" ]; then
        cp ".scratch/hub.env" "$UPLOAD_DIR/hub.env"
        HAS_HUB_ENV=true
        substep "Prepared hub.env"
    fi

    # settings.yaml
    cat <<'SETTINGS_EOF' > "$UPLOAD_DIR/scion-settings.yaml"
schema_version: "1"
default_runtime: kubernetes
hub:
  endpoint: "https://hub.demo.scion-ai.dev"
telemetry:
  enabled: false
  cloud:
    enabled: true
    endpoint: "cloudtrace.googleapis.com:443"
    protocol: "grpc"
    batch:
      max_size: 256
      timeout: "5s"
  local:
    enabled: true
  filter:
    events:
      exclude:
        - "agent.user.prompt"
    attributes:
      redact:
        - "prompt"
        - "user.email"
        - "tool_output"
        - "tool_input"
      hash:
        - "session_id"
SETTINGS_EOF
    substep "Prepared settings.yaml"

    # systemd service file
    printf "[Unit]
Description=Scion Hub API Server
After=network.target

[Service]
User=scion
Group=scion
WorkingDirectory=%s
EnvironmentFile=/home/scion/.scion/hub.env
Environment=\"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\"
# Use journald for log management
StandardOutput=journal
StandardError=journal
ExecStartPre=/usr/bin/env
ExecStart=%s --global server start --foreground --production --debug --enable-hub --enable-runtime-broker --enable-web --runtime-broker-port 9800 --web-port 8080 --storage-bucket \${SCION_HUB_STORAGE_BUCKET} --session-secret \${SESSION_SECRET} --auto-provide
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
" "${REPO_DIR}" "${SCION_BIN}" > "$UPLOAD_DIR/scion-hub.service"
    substep "Prepared systemd unit file"

    # Caddyfile
    cat <<EOF > "$UPLOAD_DIR/Caddyfile"
hub.demo.scion-ai.dev {
    # In combined mode, Hub API and Web UI are served on a single port
    reverse_proxy localhost:8080
    tls /etc/letsencrypt/live/demo.scion-ai.dev/fullchain.pem /etc/letsencrypt/live/demo.scion-ai.dev/privkey.pem
}
EOF
    substep "Prepared Caddyfile"

    # Single SCP to upload all files, then single SSH to place them
    substep "Uploading files to instance..."
    gcloud compute scp "$UPLOAD_DIR"/* "${INSTANCE_NAME}:/tmp/" --zone="${ZONE}"

    # Prepare directories and move hub.env into place via a single SSH call
    PLACE_HUB_ENV=""
    if $HAS_HUB_ENV; then
        PLACE_HUB_ENV='
        sudo mv /tmp/hub.env /home/scion/.scion/hub.env
        sudo chown scion:scion /home/scion/.scion/hub.env
        sudo chmod 600 /home/scion/.scion/hub.env
        echo "  -> Installed hub.env"'
    fi

    gcloud compute ssh "${INSTANCE_NAME}" --zone="${ZONE}" --command "
        set -euo pipefail
        sudo mkdir -p /home/scion/.scion
        sudo chown scion:scion /home/scion/.scion
        ${PLACE_HUB_ENV}
        if [ ! -f /home/scion/.scion/settings.yaml ]; then
            sudo mv /tmp/scion-settings.yaml /home/scion/.scion/settings.yaml
            sudo chown scion:scion /home/scion/.scion/settings.yaml
            echo '  -> Installed settings.yaml'
        else
            echo '  -> settings.yaml already exists, skipping'
            rm -f /tmp/scion-settings.yaml
        fi
    "
    substep "Config files placed on instance"
fi

# --- Remote: Pull, Build, Restart, Health Check (single SSH session) ---

# Build the conditional full-deploy remote commands
FULL_REMOTE_COMMANDS=""
if $FULL_DEPLOY; then
    FULL_REMOTE_COMMANDS='
    # Install Caddy if missing
    if ! command -v caddy &>/dev/null; then
        echo "  -> Installing Caddy..."
        sudo apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
        curl -1sLf "https://dl.cloudsmith.io/public/caddy/stable/gpg.key" | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
        curl -1sLf "https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt" | sudo tee /etc/apt/sources.list.d/caddy-stable.list
        sudo apt-get update
        sudo apt-get install -y caddy
    fi
'
fi

FULL_POST_INSTALL_COMMANDS=""
if $FULL_DEPLOY; then
    FULL_POST_INSTALL_COMMANDS='
    # Update systemd unit file if changed
    echo ""
    echo "==> Installing infrastructure config..."
    if ! diff -q /tmp/scion-hub.service /etc/systemd/system/scion-hub.service >/dev/null 2>&1; then
        sudo mv /tmp/scion-hub.service /etc/systemd/system/scion-hub.service
        echo "  -> Systemd unit file updated, reloading daemon..."
        sudo systemctl daemon-reload
    else
        echo "  -> Systemd unit file unchanged"
    fi

    # Fix certificate permissions for Caddy
    echo "  -> Fixing certificate permissions..."
    sudo chown -R root:caddy /etc/letsencrypt/live
    sudo chown -R root:caddy /etc/letsencrypt/archive
    sudo chmod -R g+rX /etc/letsencrypt/live
    sudo chmod -R g+rX /etc/letsencrypt/archive

    # Update Caddyfile if changed
    if ! diff -q /tmp/Caddyfile /etc/caddy/Caddyfile >/dev/null 2>&1; then
        sudo mv /tmp/Caddyfile /etc/caddy/Caddyfile
        sudo chown caddy:caddy /etc/caddy/Caddyfile
        sudo chmod 644 /etc/caddy/Caddyfile
        echo "  -> Caddyfile updated, restarting Caddy..."
        sudo systemctl restart caddy
    else
        echo "  -> Caddyfile unchanged"
    fi

    # Install kubectl if missing
    if ! command -v kubectl &>/dev/null; then
        echo "  -> Installing kubectl..."
        sudo apt-get update && sudo apt-get install -y kubectl || sudo snap install kubectl --classic || echo "  -> Failed to install kubectl automatically"
    fi
'
fi

step "Remote: pull, build, restart..."

gcloud compute ssh "${INSTANCE_NAME}" --zone="${ZONE}" --command '
    set -euo pipefail
    RESET_DB='"${RESET_DB}"'
    FULL_DEPLOY='"${FULL_DEPLOY}"'
    REMOTE_START=$SECONDS

    '"${FULL_REMOTE_COMMANDS}"'

    # Pull
    echo ""
    echo "==> Pulling latest code..."
    PULL_START=$SECONDS
    sudo -u scion sh -c "cd /home/scion/scion-agent && git pull"
    echo "  -> Pull took $(( SECONDS - PULL_START ))s"

    # Build web assets
    echo ""
    echo "==> Building web assets..."
    WEB_START=$SECONDS
    sudo -u scion sh -c "cd /home/scion/scion-agent && make web"
    echo "  -> Web build took $(( SECONDS - WEB_START ))s"

    # Build binary
    echo ""
    echo "==> Building scion binary..."
    BUILD_START=$SECONDS
    sudo -u scion sh -c "cd /home/scion/scion-agent && /usr/local/go/bin/go build -o scion ./cmd/scion"
    echo "  -> Binary build took $(( SECONDS - BUILD_START ))s"

    # Configure GKE credentials if full deploy
    if [ "$FULL_DEPLOY" = "true" ]; then
        echo ""
        echo "==> Configuring GKE credentials..."
        sudo -u scion sh -c "gcloud container clusters get-credentials scion-demo-cluster --region us-central1 --project deploy-demo-test || true"
    fi

    # Stop existing service
    if systemctl is-active --quiet scion-hub; then
        sudo systemctl stop scion-hub
        echo "  -> Service stopped"
    else
        echo "  -> Service was not running"
    fi

    # Reset database if requested
    if [ "$RESET_DB" = "true" ]; then
        echo "  -> Resetting hub database..."
        sudo rm -f /home/scion/.scion/hub.db
        echo "  -> Database deleted"
    fi

    # Install binary
    echo "  -> Installing binary..."
    sudo mv /home/scion/scion-agent/scion /usr/local/bin/scion
    sudo chmod +x /usr/local/bin/scion

    '"${FULL_POST_INSTALL_COMMANDS}"'

    # Start service
    echo ""
    echo "==> Starting scion-hub service..."
    sudo systemctl enable scion-hub
    sudo systemctl start scion-hub

    # Wait for service to be active
    for i in {1..10}; do
        if systemctl is-active --quiet scion-hub; then
            echo "  -> Service is active"
            break
        fi
        echo "  -> Waiting for service... (${i}/10)"
        sleep 2
    done

    if ! systemctl is-active --quiet scion-hub; then
        echo "Error: Service failed to start."
        sudo journalctl -u scion-hub -n 20
        exit 1
    fi

    # Local health check
    echo ""
    echo "==> Local health check..."
    for i in {1..10}; do
        HEALTH_RESP=$(curl -s http://localhost:8080/healthz || true)
        if echo "$HEALTH_RESP" | grep -q "\"status\":\"healthy\""; then
            echo "  -> Local health check passed: $HEALTH_RESP"
            break
        fi
        echo "  -> Waiting for health check... (${i}/10)"
        sleep 2
        if [ "$i" -eq 10 ]; then
             echo "Error: Local health check failed."
             exit 1
        fi
    done

    echo ""
    echo "  -> Remote session took $(( SECONDS - REMOTE_START ))s"
'

# --- Step: Remote Health Check ---

step "Remote health check..."
echo "  -> Checking https://${DOMAIN}/healthz..."
for i in {1..12}; do
    if curl -s -k "https://${DOMAIN}/healthz" | grep -q '"status":"healthy"'; then
        echo "  -> Hub is healthy!"
        curl -s -k "https://${DOMAIN}/healthz"
        echo ""
        print_summary
        exit 0
    fi
    echo "  -> Still waiting... ($i/12)"
    sleep 5
done

echo "Error: Remote health check failed after 60 seconds."
exit 1
