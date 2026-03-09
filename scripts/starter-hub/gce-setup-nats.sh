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

# scripts/starter-hub/gce-setup-nats.sh - Setup NATS Server with systemd on GCE demo instance
#
# ARCHIVED: This script is superseded by the in-process ChannelEventPublisher
# (pkg/hub/events.go). The Go binary now handles real-time event distribution
# directly, eliminating the need for a standalone NATS server. This script is
# retained for historical reference only.

set -euo pipefail

INSTANCE_NAME="scion-demo"
ZONE="us-central1-a"

# --- Create systemd unit file ---
TMP_SERVICE=$(mktemp)
cat <<'EOF' > "$TMP_SERVICE"
[Unit]
Description=NATS Server
After=network-online.target ntp.service

[Service]
PrivateTmp=true
Type=simple
ExecStart=/usr/sbin/nats-server -c /etc/nats-server.conf
ExecReload=/bin/kill -s HUP $MAINPID

# The nats-server uses SIGUSR2 to trigger Lame Duck Mode (LDM) shutdown
# https://docs.nats.io/running-a-nats-service/nats_admin/lame_duck_mode
ExecStop=/bin/kill -s SIGUSR2 $MAINPID

# This should be `lame_duck_duration` + some buffer to finish the shutdown.
# By default, `lame_duck_duration` is 2 mins.
TimeoutStopSec=150

Restart=on-failure

User=nats
Group=nats

[Install]
WantedBy=multi-user.target
EOF

# --- Create NATS config file ---
TMP_CONF=$(mktemp)
cat <<'EOF' > "$TMP_CONF"
# /etc/nats-server.conf - Scion Demo NATS Server Configuration

port: 4222
monitor_port: 8222

server_name: scion-demo

jetstream {
    store_dir: /var/lib/nats/jetstream
}
EOF

# --- Upload files ---
gcloud compute scp "$TMP_SERVICE" "${INSTANCE_NAME}:/tmp/nats-server.service" --zone="${ZONE}"
gcloud compute scp "$TMP_CONF" "${INSTANCE_NAME}:/tmp/nats-server.conf" --zone="${ZONE}"
rm "$TMP_SERVICE" "$TMP_CONF"

# --- Install and configure on instance ---
gcloud compute ssh "${INSTANCE_NAME}" --zone="${ZONE}" --command '
    set -euo pipefail

    # 1. Create nats user/group if they do not exist
    if ! id -u nats &>/dev/null; then
        echo "Creating nats user..."
        sudo useradd --system --no-create-home --shell /usr/sbin/nologin nats
    else
        echo "User nats already exists."
    fi

    # 2. Create data directories
    sudo mkdir -p /var/lib/nats/jetstream
    sudo chown -R nats:nats /var/lib/nats

    # 3. Install config file
    echo "Installing NATS config..."
    sudo mv /tmp/nats-server.conf /etc/nats-server.conf
    sudo chown root:root /etc/nats-server.conf
    sudo chmod 644 /etc/nats-server.conf

    # 4. Stop existing service if running
    if systemctl is-active --quiet nats-server; then
        echo "Stopping existing nats-server..."
        sudo systemctl stop nats-server
    fi

    # 5. Install systemd unit file
    echo "Installing systemd unit..."
    sudo mv /tmp/nats-server.service /etc/systemd/system/nats-server.service
    sudo chown root:root /etc/systemd/system/nats-server.service
    sudo chmod 644 /etc/systemd/system/nats-server.service
    sudo systemctl daemon-reload

    # 6. Enable and start
    echo "Starting nats-server..."
    sudo systemctl enable nats-server
    sudo systemctl start nats-server

    # 7. Verify
    echo "Waiting for NATS to start..."
    for i in {1..10}; do
        if systemctl is-active --quiet nats-server; then
            echo "NATS server is active."
            break
        fi
        echo "Still waiting... ${i}"
        sleep 1
    done

    if ! systemctl is-active --quiet nats-server; then
        echo "Error: NATS server failed to start."
        sudo journalctl -u nats-server -n 20 --no-pager
        exit 1
    fi

    echo ""
    sudo systemctl status nats-server --no-pager
'

echo ""
echo "=== NATS Server Setup Complete ==="
