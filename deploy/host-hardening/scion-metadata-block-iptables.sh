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
#
# Host-level defense-in-depth iptables (DOCKER-USER) script blocking bridged
# container HTTP access to cloud instance metadata endpoints (IPv4 and IPv6),
# while keeping GCE DNS (UDP/TCP 53 on 169.254.169.254) allowed.
#
# SCOPE CAVEAT: This applies to EVERY Docker-bridged container on the host,
# including Scion agents in `passthrough` mode and non-Scion containers that
# rely on the host VM's metadata identity. Enable only on hosts dedicated to
# `block` and `assign` agents.
#
# For hosts that also run Podman or CNI bridges, prefer scion-metadata-block.nft.

set -euo pipefail

ensure_rule() {
    local tool="$1"
    shift
    if ! "$tool" -C "$@" 2>/dev/null; then
        "$tool" -I "$@"
    fi
}

# Verify DOCKER-USER exists (created by dockerd).
if ! iptables -L DOCKER-USER -n >/dev/null 2>&1; then
    echo "error: iptables chain DOCKER-USER does not exist (is dockerd running?)" >&2
    exit 1
fi

# Drop HTTP (TCP/80) to IPv4 cloud metadata endpoint.
# Inserted first so the DNS RETURN rules inserted after sit ahead of it.
ensure_rule iptables DOCKER-USER -d 169.254.169.254/32 -p tcp --dport 80 -j DROP

# Keep GCE metadata-server DNS (UDP/TCP 53) allowed.
ensure_rule iptables DOCKER-USER -d 169.254.169.254/32 -p tcp --dport 53 -j RETURN
ensure_rule iptables DOCKER-USER -d 169.254.169.254/32 -p udp --dport 53 -j RETURN

# Drop HTTP (TCP/80) to IPv6 cloud metadata endpoints (GCE, AWS) when ip6tables
# and the IPv6 DOCKER-USER chain are present.
if command -v ip6tables >/dev/null 2>&1 && ip6tables -L DOCKER-USER -n >/dev/null 2>&1; then
    ensure_rule ip6tables DOCKER-USER -d fd20:ce::254/128 -p tcp --dport 80 -j DROP
    ensure_rule ip6tables DOCKER-USER -d fd00:ec2::254/128 -p tcp --dport 80 -j DROP
fi
