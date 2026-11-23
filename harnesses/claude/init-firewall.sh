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

set -euo pipefail  # Exit on error, undefined vars, and pipeline failures
IFS=$'\n\t'       # Stricter word splitting

valid_ipv4() {
    local ip="$1"
    local o1 o2 o3 o4 o
    [[ "$ip" =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
    o1="${BASH_REMATCH[1]}"
    o2="${BASH_REMATCH[2]}"
    o3="${BASH_REMATCH[3]}"
    o4="${BASH_REMATCH[4]}"
    for o in "$o1" "$o2" "$o3" "$o4"; do
        [[ "$o" == "0" || "$o" != 0* ]] || return 1
        (( 10#$o <= 255 )) || return 1
    done
    return 0
}

valid_ipv4_cidr() {
    local cidr="$1"
    local ip prefix
    [[ "$cidr" == */* ]] || return 1
    ip="${cidr%/*}"
    prefix="${cidr#*/}"
    valid_ipv4 "$ip" || return 1
    [[ "$prefix" =~ ^[0-9]{1,2}$ ]] || return 1
    [[ "$prefix" == "0" || "$prefix" != 0* ]] || return 1
    (( 10#$prefix <= 32 )) || return 1
    return 0
}

valid_ipv6() {
    local ip="$1"
    local without_double
    [[ ${#ip} -le 39 ]] || return 1
    [[ "$ip" =~ ^[0-9a-fA-F:]+$ ]] || return 1
    [[ "$ip" == *:*:* ]] || return 1
    [[ "$ip" != *:::* ]] || return 1
    [[ "$ip" =~ [0-9a-fA-F] ]] || return 1
    without_double="${ip/::/}"
    [[ "$without_double" != *::* ]] || return 1
    return 0
}

ipv6_enabled() {
    local proc_root="${SCION_FW_PROC_ROOT:-/proc}"
    local f n val
    [ -d "$proc_root/sys/net/ipv6/conf" ] || return 1
    for f in "$proc_root"/sys/net/ipv6/conf/*/disable_ipv6; do
        [ -e "$f" ] || continue
        n=$(basename "$(dirname "$f")")
        case "$n" in
            all|default|lo) continue ;;
        esac
        val=$(cat "$f" 2>/dev/null || true)
        [ "$val" = "0" ] && return 0
    done
    return 1
}

resolv_conf_ipv6_nameservers() {
    local resolv_conf="${SCION_FW_RESOLV_CONF:-/etc/resolv.conf}"
    [ -r "$resolv_conf" ] || return 0
    local line kw addr
    while IFS= read -r line; do
        line="${line%%#*}"
        line="${line%%;*}"
        IFS=$' \t' read -r kw addr _ <<< "$line" || continue
        if [ "${kw:-}" = "nameserver" ] && [ -n "${addr:-}" ] && valid_ipv6 "$addr"; then
            printf '%s\n' "$addr"
        fi
    done < "$resolv_conf"
}

# 0. Validate optional policy environment inputs before touching any rules or network
NAME_RE='^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
EXTRA_DOMAINS=()
if [ -n "${SCION_INIT_FIREWALL_ALLOW_DOMAINS:-}" ]; then
    raw_allow_domains="${SCION_INIT_FIREWALL_ALLOW_DOMAINS}"
    if [[ ! "$raw_allow_domains" =~ ^[a-z0-9.,-]+$ ]] \
        || [[ "$raw_allow_domains" == ,* ]] \
        || [[ "$raw_allow_domains" == *, ]] \
        || [[ "$raw_allow_domains" == *,,* ]]; then
        echo "ERROR: invalid allow-domain"
        exit 1
    fi
    IFS=',' read -r -a EXTRA_DOMAINS <<< "$raw_allow_domains"
    if [ "${#EXTRA_DOMAINS[@]}" -gt 128 ]; then
        echo "ERROR: allow-domain list exceeds 128 entries"
        exit 1
    fi
    for d in "${EXTRA_DOMAINS[@]}"; do
        if [[ ${#d} -gt 253 ]] \
            || [[ ! "$d" =~ $NAME_RE ]] \
            || [[ "$d" =~ \.[0-9]+$ ]] \
            || [[ "$d" == *.localhost ]]; then
            echo "ERROR: invalid allow-domain: $d"
            exit 1
        fi
    done
fi

ALLOW_SSH=0
case "${SCION_INIT_FIREWALL_ALLOW_SSH:-}" in
    "") ;;
    1|true) ALLOW_SSH=1 ;;
    *)
        echo "ERROR: invalid ALLOW_SSH"
        exit 1
        ;;
esac

# 1. Configure IPv6 default-drop first so IPv6 is never left open during setup
IPV6_FILTERED=0
if command -v ip6tables >/dev/null 2>&1 && ip6tables -w -S >/dev/null 2>&1; then
    ip6tables -w -F
    ip6tables -w -X
    ip6tables -w -A INPUT -i lo -j ACCEPT
    ip6tables -w -A OUTPUT -o lo -j ACCEPT
    ip6tables -w -A INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT
    ip6tables -w -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT
    while IFS= read -r ns; do
        [ -n "$ns" ] || continue
        ip6tables -w -A OUTPUT -p udp -d "$ns" --dport 53 -j ACCEPT
    done < <(resolv_conf_ipv6_nameservers)
    ip6tables -w -P INPUT DROP
    ip6tables -w -P FORWARD DROP
    ip6tables -w -P OUTPUT DROP
    ip6tables -w -A OUTPUT -j REJECT --reject-with icmp6-adm-prohibited
    IPV6_FILTERED=1
elif ipv6_enabled; then
    echo "ERROR: IPv6 is enabled in this network namespace but ip6tables is unavailable; refusing"
    exit 1
else
    echo "IPv6 disabled and ip6tables unavailable; skipping IPv6 rules"
fi

# 2. Extract Docker DNS info BEFORE any IPv4 flushing
DOCKER_DNS_RULES=$(iptables-save -t nat | grep "127\.0\.0\.11" || true)

# Flush existing IPv4 rules and delete existing ipsets
iptables -F
iptables -X
iptables -t nat -F
iptables -t nat -X
iptables -t mangle -F
iptables -t mangle -X
ipset destroy allowed-domains 2>/dev/null || true

# Selectively restore ONLY internal Docker DNS resolution
if [ -n "$DOCKER_DNS_RULES" ]; then
    echo "Restoring Docker DNS rules..."
    iptables -t nat -N DOCKER_OUTPUT 2>/dev/null || true
    iptables -t nat -N DOCKER_POSTROUTING 2>/dev/null || true
    echo "$DOCKER_DNS_RULES" | xargs -L 1 iptables -t nat
else
    echo "No Docker DNS rules to restore"
fi

# Allow DNS and localhost before any restrictions
# Allow outbound DNS
iptables -A OUTPUT -p udp --dport 53 -j ACCEPT
# Allow inbound DNS responses
iptables -A INPUT -p udp --sport 53 -j ACCEPT
# Allow outbound SSH only when explicitly enabled
if [ "$ALLOW_SSH" -eq 1 ]; then
    iptables -A OUTPUT -p tcp --dport 22 -j ACCEPT
fi
# Allow localhost
iptables -A INPUT -i lo -j ACCEPT
iptables -A OUTPUT -o lo -j ACCEPT

# Create ipset with CIDR support
ipset create allowed-domains hash:net

# Fetch GitHub meta information and aggregate + add their IP ranges
echo "Fetching GitHub IP ranges..."
gh_ranges=$(curl -s https://api.github.com/meta)
if [ -z "$gh_ranges" ]; then
    echo "ERROR: Failed to fetch GitHub IP ranges"
    exit 1
fi

if ! echo "$gh_ranges" | jq -e '.web and .api and .git' >/dev/null; then
    echo "ERROR: GitHub API response missing required fields"
    exit 1
fi

echo "Processing GitHub IPs..."
while IFS= read -r cidr; do
    [ -n "$cidr" ] || continue
    if ! valid_ipv4_cidr "$cidr"; then
        echo "ERROR: Invalid CIDR range from GitHub meta: $cidr"
        exit 1
    fi
    echo "Adding GitHub range $cidr"
    ipset -exist add allowed-domains "$cidr"
done < <(echo "$gh_ranges" | jq -r '(.web + .api + .git)[]' | aggregate -q)

# 3. Resolve and add required, optional, and operator-allowed domains
resolve_and_add_domain() {
    local domain="$1"
    local required="$2"

    echo "Resolving $domain..."
    local raw_ips ips
    if ! raw_ips=$(getent ahostsv4 "$domain"); then
        ips=""
    else
        ips=$(printf '%s\n' "$raw_ips" | awk '{print $1}' | sort -u)
    fi
    if [ -z "$ips" ]; then
        if [ "$required" = "true" ]; then
            echo "ERROR: Failed to resolve required domain $domain" >&2
            exit 1
        fi
        echo "WARNING: Failed to resolve optional domain $domain; skipping" >&2
        return 0
    fi

    local ip
    while IFS= read -r ip; do
        [ -n "$ip" ] || continue
        if ! valid_ipv4 "$ip"; then
            echo "ERROR: Invalid IP from DNS for $domain: $ip" >&2
            exit 1
        fi
        echo "Adding $ip for $domain"
        ipset -exist add allowed-domains "$ip"
    done <<< "$ips"
}

REQUIRED_DOMAINS=(
    "api.anthropic.com"
)

OPTIONAL_DOMAINS=(
    "registry.npmjs.org"
    "sentry.io"
    "statsig.com"
    "marketplace.visualstudio.com"
    "vscode.blob.core.windows.net"
    "update.code.visualstudio.com"
)

for domain in "${REQUIRED_DOMAINS[@]}"; do
    resolve_and_add_domain "$domain" "true"
done

for domain in "${OPTIONAL_DOMAINS[@]}"; do
    resolve_and_add_domain "$domain" "false"
done

if [ "${#EXTRA_DOMAINS[@]}" -gt 0 ]; then
    for domain in "${EXTRA_DOMAINS[@]}"; do
        resolve_and_add_domain "$domain" "true"
    done
fi

# 4. Allow only the default gateway host (/32) and distinct broker host aliases
HOST_IP=$(ip -4 route show default | awk '{print $3; exit}' || true)
if ! valid_ipv4 "$HOST_IP"; then
    echo "ERROR: Failed to detect host IP"
    exit 1
fi
echo "Host gateway detected as: $HOST_IP/32"

iptables -A INPUT -s "$HOST_IP/32" -j ACCEPT
iptables -A OUTPUT -d "$HOST_IP/32" -j ACCEPT
ALLOWED_HOST_IPS=("$HOST_IP")

# Broker-written host alias (Docker host-gateway / Podman), if it differs from the gateway
for alias in host.docker.internal host.containers.internal; do
    alias_ip=$(getent ahostsv4 "$alias" 2>/dev/null | awk '{print $1; exit}' || true)
    if [ -n "$alias_ip" ] && valid_ipv4 "$alias_ip"; then
        seen=0
        for existing in "${ALLOWED_HOST_IPS[@]}"; do
            if [ "$existing" = "$alias_ip" ]; then
                seen=1
                break
            fi
        done
        if [ "$seen" -eq 0 ]; then
            echo "Host alias $alias detected as: $alias_ip/32"
            iptables -A INPUT -s "$alias_ip/32" -j ACCEPT
            iptables -A OUTPUT -d "$alias_ip/32" -j ACCEPT
            ALLOWED_HOST_IPS+=("$alias_ip")
        fi
    fi
done

# Set default policies to DROP first
iptables -P INPUT DROP
iptables -P FORWARD DROP
iptables -P OUTPUT DROP

# First allow established connections for already approved traffic
iptables -A INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT
iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT

# Then allow only specific outbound traffic to allowed domains
iptables -A OUTPUT -m set --match-set allowed-domains dst -j ACCEPT

# Explicitly REJECT all other outbound traffic for immediate feedback
iptables -A OUTPUT -j REJECT --reject-with icmp-admin-prohibited

echo "Firewall configuration complete"
echo "Verifying firewall rules..."
if curl --connect-timeout 5 https://example.com >/dev/null 2>&1; then
    echo "ERROR: Firewall verification failed - was able to reach https://example.com"
    exit 1
else
    echo "Firewall verification passed - unable to reach https://example.com as expected"
fi

if [ "$IPV6_FILTERED" -eq 1 ] && ipv6_enabled; then
    if curl -6 --connect-timeout 5 https://example.com >/dev/null 2>&1; then
        echo "ERROR: Firewall verification failed - was able to reach https://example.com over IPv6"
        exit 1
    else
        echo "Firewall verification passed - unable to reach https://example.com over IPv6 as expected"
    fi
fi

# Verify GitHub API access
if ! curl --connect-timeout 5 https://api.github.com/zen >/dev/null 2>&1; then
    echo "ERROR: Firewall verification failed - unable to reach https://api.github.com"
    exit 1
else
    echo "Firewall verification passed - able to reach https://api.github.com as expected"
fi
