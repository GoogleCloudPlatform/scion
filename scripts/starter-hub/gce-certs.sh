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

# scripts/starter-hub/gce-certs.sh - Setup Cloud DNS and obtain Let's Encrypt certificates

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/hub-config.sh"

DOMAIN="${CERT_DOMAIN}"
HUB_SUBDOMAIN="${HUB_DOMAIN}"
ZONE_NAME="${DNS_ZONE_NAME}"
GCE_ZONE="${ZONE}"
EMAIL="${CERT_EMAIL}"

if [[ -z "$PROJECT_ID" ]]; then
    echo "Error: PROJECT_ID is not set and could not be determined from gcloud config."
    exit 1
fi

echo "=== DNS and Certificate Setup for ${DOMAIN} ==="

# 1. Create Managed DNS Zone if it doesn't exist
if ! gcloud dns managed-zones describe "${ZONE_NAME}" &>/dev/null; then
    echo "Creating Cloud DNS managed zone: ${ZONE_NAME}..."
    gcloud dns managed-zones create "${ZONE_NAME}" \
        --dns-name="${DOMAIN}." \
        --description="Managed zone for scion-ai.dev sub-domain" \
        --visibility="public"
else
    echo "DNS zone ${ZONE_NAME} already exists."
fi

# 2. Display Nameservers
echo "--------------------------------------------------"
echo "Registrar Nameservers:"
gcloud dns managed-zones describe "${ZONE_NAME}" --format="value(nameServers)" | tr ';' '\n'
echo "--------------------------------------------------"

# 3. Add or Update A Record for the Hub
echo "Checking A record for ${HUB_SUBDOMAIN}..."
EXTERNAL_IP=$(gcloud compute instances describe "${INSTANCE_NAME}" --zone="${GCE_ZONE}" --format="get(networkInterfaces[0].accessConfigs[0].natIP)")

# Try to get the current IP of the record
CURRENT_RECORD_IP=$(gcloud dns record-sets list --zone="${ZONE_NAME}" --name="${HUB_SUBDOMAIN}." --type="A" --format="value(rrdatas[0])" 2>/dev/null || true)

if [[ -n "$CURRENT_RECORD_IP" ]]; then
    if [[ "$CURRENT_RECORD_IP" == "$EXTERNAL_IP" ]]; then
        echo "A record for ${HUB_SUBDOMAIN} already points to correct IP: ${EXTERNAL_IP}."
    else
        echo "Updating A record for ${HUB_SUBDOMAIN} from ${CURRENT_RECORD_IP} to ${EXTERNAL_IP}..."
        gcloud dns record-sets update "${HUB_SUBDOMAIN}." \
            --zone="${ZONE_NAME}" \
            --type="A" \
            --ttl="300" \
            --rrdatas="${EXTERNAL_IP}"
    fi
else
    echo "Creating A record for ${HUB_SUBDOMAIN} pointing to ${EXTERNAL_IP}..."
    gcloud dns record-sets create "${HUB_SUBDOMAIN}." \
        --zone="${ZONE_NAME}" \
        --type="A" \
        --ttl="300" \
        --rrdatas="${EXTERNAL_IP}"
fi

# 4. Obtain Wildcard Certificate via SSH on the instance
# This assumes the instance has the roles/dns.admin and python3-certbot-dns-google installed via provision/cloud-init
echo "Checking certificate status on ${INSTANCE_NAME}..."
if gcloud compute ssh "${INSTANCE_NAME}" --zone="${GCE_ZONE}" --command="sudo test -f /etc/letsencrypt/live/${DOMAIN}/fullchain.pem" &>/dev/null; then
    echo "Certificate for ${DOMAIN} already exists. Skipping acquisition."
else
    echo "Requesting wildcard certificate for ${DOMAIN}..."
    gcloud compute ssh "${INSTANCE_NAME}" --zone="${GCE_ZONE}" --command="sudo certbot certonly \
        --dns-google \
        --dns-google-propagation-seconds 60 \
        -d '${DOMAIN}' \
        -d '*.${DOMAIN}' \
        --email ${EMAIL} \
        --non-interactive \
        --agree-tos \
        --deploy-hook 'chown root:caddy /etc/letsencrypt/live /etc/letsencrypt/archive && chmod g+x /etc/letsencrypt/live /etc/letsencrypt/archive && chown -R root:caddy /etc/letsencrypt/live/\${RENEWED_DOMAINS%%,*} /etc/letsencrypt/archive/\${RENEWED_DOMAINS%%,*} && chmod -R g+rX /etc/letsencrypt/live/\${RENEWED_DOMAINS%%,*} /etc/letsencrypt/archive/\${RENEWED_DOMAINS%%,*} && (systemctl reload caddy || caddy reload --config /etc/caddy/Caddyfile)'"
fi

# 5. Reload Caddy if it's installed to pick up new/renewed certificates
if gcloud compute ssh "${INSTANCE_NAME}" --zone="${GCE_ZONE}" --command="command -v caddy" &>/dev/null; then
    echo "Reloading Caddy on ${INSTANCE_NAME}..."
    gcloud compute ssh "${INSTANCE_NAME}" --zone="${GCE_ZONE}" --command="sudo systemctl reload caddy || sudo caddy reload --config /etc/caddy/Caddyfile"
fi

echo ""
echo "=== Success ==="
echo "Certificates available on ${INSTANCE_NAME} at /etc/letsencrypt/live/${DOMAIN}/"
echo "Hub is accessible at https://${HUB_SUBDOMAIN} (once DNS propagates)"