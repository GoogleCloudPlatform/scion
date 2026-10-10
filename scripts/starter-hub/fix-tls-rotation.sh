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

# scripts/starter-hub/fix-tls-rotation.sh - make certbot renewals reach Caddy.
#
# Runs ON the hub VM, as root. gce-certs.sh copies it to the VM and runs it
# for new hubs; run it by hand to repair an existing hub.
#
# Caddy serves the certificate files under /etc/letsencrypt/live/<domain>/
# (see the Caddyfile that gce-start-hub.sh writes) and reads them only at
# start and on reload. certbot renews those files on disk, so without a
# reload Caddy keeps serving the old certificate until it expires. This
# script:
#
#   1. installs the certbot deploy hook
#      /etc/letsencrypt/renewal-hooks/deploy/scion-reload-caddy.sh, which
#      certbot runs once after each successful renewal: it gives group caddy
#      read access to the new files and reloads Caddy (never the hub);
#   2. removes the inline renew_hook that older gce-certs.sh versions stored
#      in /etc/letsencrypt/renewal/<domain>.conf (it failed before reaching
#      the reload, see ptone/scion#4207);
#   3. enables the certbot renewal timer;
#   4. repairs group caddy read access on the certificate files;
#   5. runs `certbot renew` only when the certificate on disk expires within
#      --renew-days, and reloads Caddy only when the certificate it serves
#      differs from the one on disk;
#   6. prints a final check.
#
# Re-running it changes nothing once the hub is in order.
#
# Usage:
#   sudo scripts/starter-hub/fix-tls-rotation.sh [--dry-run | --check]
#       [--domain <cert-domain>] [--host <served-host>]
#       [--connect <addr:port>] [--renew-days <n>] [--root <dir>]
#
#   --dry-run          Print every action without changing anything.
#   --check            Read-only report: certificate notAfter and serial (on
#                      disk and as served), hook and timer status. Exits 1 if
#                      something needs fixing.
#   --domain NAME      Certificate name, the directory under
#                      /etc/letsencrypt/live/. Default: taken from the tls
#                      line of /etc/caddy/Caddyfile, else the only directory
#                      under /etc/letsencrypt/live/.
#   --host NAME        Server name (SNI) used to fetch the served certificate.
#                      Default: the first site address in the Caddyfile.
#   --connect ADDR     Where to fetch the served certificate. Default
#                      127.0.0.1:443.
#   --renew-days N     Renew when the certificate on disk expires within N
#                      days. Default 30, the certbot default.
#   --root DIR         Treat DIR as the filesystem root (for tests).

set -euo pipefail

MODE=apply
DOMAIN=""
SERVED_HOST=""
CONNECT="127.0.0.1:443"
RENEW_DAYS=30
ROOT="${FIX_TLS_ROOT:-}"
CADDY_GROUP="${CADDY_GROUP:-caddy}"
HOOK_NAME="scion-reload-caddy.sh"

usage() {
    sed -n '/^# Usage:/,/^# *--root/p' "$0" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --dry-run) MODE=dry-run ;;
        --check) MODE=check ;;
        --domain) DOMAIN="${2:?--domain needs a value}"; shift ;;
        --host) SERVED_HOST="${2:?--host needs a value}"; shift ;;
        --connect) CONNECT="${2:?--connect needs a value}"; shift ;;
        --renew-days) RENEW_DAYS="${2:?--renew-days needs a value}"; shift ;;
        --root) ROOT="${2:?--root needs a value}"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "Unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
    shift
done

if ! [[ "$RENEW_DAYS" =~ ^[0-9]+$ ]]; then
    echo "Error: --renew-days must be a whole number of days" >&2
    exit 2
fi

LE_DIR="${ROOT}/etc/letsencrypt"
HOOK_DIR="${LE_DIR}/renewal-hooks/deploy"
HOOK_PATH="${HOOK_DIR}/${HOOK_NAME}"
CADDYFILE="${ROOT}/etc/caddy/Caddyfile"

if [[ "$MODE" == "apply" && -z "$ROOT" && "${EUID}" -ne 0 ]]; then
    echo "Error: run as root (sudo), or use --dry-run or --check." >&2
    exit 1
fi

CHANGES=0
PROBLEMS=0

say() { echo "$*"; }
found() { echo "  found:   $*"; }
ok() { echo "  ok:      $*"; }
warn() { echo "  WARN:    $*"; }
problem() { echo "  PROBLEM: $*"; PROBLEMS=$((PROBLEMS + 1)); }

# act DESCRIPTION CMD... -- run CMD, or in --dry-run only print it.
act() {
    local desc="$1"
    shift
    CHANGES=$((CHANGES + 1))
    if [[ "$MODE" == "dry-run" ]]; then
        if declare -F "$1" >/dev/null; then
            echo "  [dry-run] would ${desc}"
        else
            echo "  [dry-run] would ${desc}: $*"
        fi
    else
        echo "  change:  ${desc}"
        "$@"
    fi
}

# hook_content -- the deploy hook, byte for byte as installed.
hook_content() {
    cat <<'HOOK'
#!/bin/bash
# Installed by scripts/starter-hub/fix-tls-rotation.sh (Scion starter hub).
# Do not edit: re-running that script replaces this file.
#
# certbot runs each executable in /etc/letsencrypt/renewal-hooks/deploy/ once
# per successfully renewed certificate, with RENEWED_LINEAGE set to its live
# directory, for example /etc/letsencrypt/live/example.com. Caddy reads the
# certificate from that directory only at start and on reload, so give group
# caddy read access to the new files, then reload Caddy. The hub itself is
# never restarted.
set -euo pipefail

CADDY_GROUP="${CADDY_GROUP:-caddy}"
lineage="${RENEWED_LINEAGE:-}"
if [ -z "$lineage" ] || [ ! -d "$lineage" ]; then
    echo "scion-reload-caddy: RENEWED_LINEAGE is unset or not a directory" >&2
    exit 1
fi
name="$(basename "$lineage")"
le_dir="$(dirname "$(dirname "$lineage")")"

if getent group "$CADDY_GROUP" >/dev/null 2>&1; then
    chgrp "$CADDY_GROUP" "$le_dir/live" "$le_dir/archive"
    chmod g+x "$le_dir/live" "$le_dir/archive"
    chgrp -R "$CADDY_GROUP" "$le_dir/live/$name" "$le_dir/archive/$name"
    chmod -R g+rX "$le_dir/live/$name" "$le_dir/archive/$name"
else
    echo "scion-reload-caddy: no group $CADDY_GROUP, permissions left as they are" >&2
fi

if systemctl is-active --quiet caddy 2>/dev/null; then
    # Packages older than Caddy 2.5 reload without --force, which Caddy skips
    # when the Caddyfile is unchanged, so the new files would not be read.
    if systemctl show -p ExecReload caddy 2>/dev/null | grep -q -- '--force'; then
        systemctl reload caddy
    else
        caddy reload --config /etc/caddy/Caddyfile --force
    fi
    echo "scion-reload-caddy: reloaded Caddy for $name"
elif command -v caddy >/dev/null 2>&1 && caddy reload --config /etc/caddy/Caddyfile --force; then
    echo "scion-reload-caddy: reloaded Caddy (not under systemd) for $name"
else
    echo "scion-reload-caddy: Caddy is not running, nothing to reload"
fi
HOOK
}

# cert_field FILE FIELD -- prints serial or notAfter of the first cert in FILE.
cert_field() {
    openssl x509 -in "$1" -noout "-$2" 2>/dev/null | sed 's/^[^=]*=//'
}

# served_cert -- prints the PEM certificate served at $CONNECT for
# $SERVED_HOST, or nothing.
served_cert() {
    local -a to=()
    if command -v timeout >/dev/null 2>&1; then to=(timeout 15); fi
    "${to[@]+"${to[@]}"}" openssl s_client -connect "$CONNECT" -servername "$SERVED_HOST" \
        </dev/null 2>/dev/null | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' || true
}

# timer_unit -- prints the certbot renewal timer unit name, or nothing.
timer_unit() {
    local u
    for u in certbot.timer snap.certbot.renew.timer; do
        if systemctl cat "$u" >/dev/null 2>&1; then
            echo "$u"
            return
        fi
    done
}

# perms_need_fix -- true if a certificate file is not readable by CADDY_GROUP.
perms_need_fix() {
    local d
    for d in "${LE_DIR}/live" "${LE_DIR}/archive"; do
        [[ "$(stat -c %G "$d")" == "$CADDY_GROUP" ]] || return 0
        [[ -n "$(find "$d" -maxdepth 0 -perm -g=x)" ]] || return 0
    done
    [[ -n "$(find "${LE_DIR}/live/${DOMAIN}" "${LE_DIR}/archive/${DOMAIN}" \
        \( ! -group "$CADDY_GROUP" -o ! -perm -g=r \) ! -type l -print -quit)" ]]
}

fix_perms() {
    chgrp "$CADDY_GROUP" "${LE_DIR}/live" "${LE_DIR}/archive"
    chmod g+x "${LE_DIR}/live" "${LE_DIR}/archive"
    chgrp -R "$CADDY_GROUP" "${LE_DIR}/live/${DOMAIN}" "${LE_DIR}/archive/${DOMAIN}"
    chmod -R g+rX "${LE_DIR}/live/${DOMAIN}" "${LE_DIR}/archive/${DOMAIN}"
}

install_hook() {
    mkdir -p "${HOOK_DIR}"
    local tmp
    tmp="$(mktemp "${HOOK_DIR}/.${HOOK_NAME}.XXXXXX")"
    hook_content > "$tmp"
    chmod 0755 "$tmp"
    mv -f "$tmp" "$HOOK_PATH"
}

remove_inline_hook() {
    local conf="$1"
    cp -p "$conf" "${conf}.bak-fix-tls-rotation"
    sed -i '/^renew_hook = .*RENEWED_DOMAINS%%,\*/d' "$conf"
}

run_hook() {
    RENEWED_LINEAGE="${LE_DIR}/live/${DOMAIN}" CADDY_GROUP="$CADDY_GROUP" "$HOOK_PATH"
}

# ---------------------------------------------------------------------------
say "=== Starter hub TLS rotation (${MODE}) ==="

# --- What Caddy serves ---
say ""
say "Caddy configuration:"
caddy_live_path=true
if [[ -f "$CADDYFILE" ]]; then
    tls_cert="$(awk '$1 == "tls" && NF >= 3 {print $2; exit}' "$CADDYFILE")"
    if [[ -z "$SERVED_HOST" ]]; then
        SERVED_HOST="$(awk '/^[^#[:space:]].*\{[[:space:]]*$/ && $1 != "{" {print $1; exit}' "$CADDYFILE")"
    fi
    if [[ -n "$tls_cert" ]]; then
        found "Caddyfile serves ${tls_cert}"
        if [[ "$tls_cert" == /etc/letsencrypt/live/*/fullchain.pem ]]; then
            ok "Caddy reads the certbot live path directly, so a renewal needs only a reload"
            caddy_domain="${tls_cert#/etc/letsencrypt/live/}"
            caddy_domain="${caddy_domain%%/*}"
            [[ -n "$DOMAIN" ]] || DOMAIN="$caddy_domain"
        else
            caddy_live_path=false
            problem "Caddy does not read /etc/letsencrypt/live/<domain>/; certbot renewals never reach it. Point the tls line at the live path (gce-start-hub.sh --full writes it that way)."
        fi
    else
        warn "no 'tls <cert> <key>' line in ${CADDYFILE}"
    fi
else
    warn "no ${CADDYFILE}"
fi

if [[ -z "$DOMAIN" && -d "${LE_DIR}/live" ]]; then
    mapfile -t lives < <(find "${LE_DIR}/live" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort)
    if [[ ${#lives[@]} -eq 1 ]]; then
        DOMAIN="${lives[0]}"
    elif [[ ${#lives[@]} -gt 1 ]]; then
        echo "Error: several certificates under ${LE_DIR}/live (${lives[*]}); pass --domain." >&2
        exit 1
    fi
fi
if [[ -z "$DOMAIN" ]]; then
    echo "Error: no certificate found under ${LE_DIR}/live; pass --domain, or run gce-certs.sh first." >&2
    exit 1
fi
[[ -n "$SERVED_HOST" ]] || SERVED_HOST="$DOMAIN"
LIVE="${LE_DIR}/live/${DOMAIN}"
RENEWAL_CONF="${LE_DIR}/renewal/${DOMAIN}.conf"
found "certificate name ${DOMAIN}, served host ${SERVED_HOST}"

if [[ ! -f "${LIVE}/fullchain.pem" ]]; then
    echo "Error: ${LIVE}/fullchain.pem does not exist." >&2
    exit 1
fi
has_lineage=false
if [[ -f "$RENEWAL_CONF" ]]; then
    has_lineage=true
else
    warn "no ${RENEWAL_CONF}: certbot does not manage this certificate, so it is never renewed automatically"
fi

# --- Deploy hook ---
say ""
say "certbot deploy hook:"
if [[ -f "$HOOK_PATH" ]] && cmp -s "$HOOK_PATH" <(hook_content) && [[ -x "$HOOK_PATH" ]]; then
    ok "${HOOK_PATH#"${ROOT}"} is installed and current"
else
    if [[ -f "$HOOK_PATH" ]]; then found "${HOOK_PATH#"${ROOT}"} is out of date or not executable"
    else found "${HOOK_PATH#"${ROOT}"} is missing"; fi
    if [[ "$MODE" == "check" ]]; then problem "deploy hook missing or out of date"
    else act "install the deploy hook ${HOOK_PATH#"${ROOT}"}" install_hook; fi
fi
other_hooks="$(find "$HOOK_DIR" -mindepth 1 -maxdepth 1 ! -name "$HOOK_NAME" ! -name ".${HOOK_NAME}.*" -printf '%f ' 2>/dev/null || true)"
[[ -z "$other_hooks" ]] || found "other deploy hooks, left alone: ${other_hooks}"

if $has_lineage; then
    if grep -q '^renew_hook = .*RENEWED_DOMAINS%%,\*' "$RENEWAL_CONF"; then
        found "inline renew_hook from an older gce-certs.sh in ${RENEWAL_CONF#"${ROOT}"} (fails before reloading Caddy)"
        if [[ "$MODE" == "check" ]]; then problem "broken inline renew_hook present"
        else act "remove it (backup ${RENEWAL_CONF#"${ROOT}"}.bak-fix-tls-rotation)" remove_inline_hook "$RENEWAL_CONF"; fi
    elif grep -q '^renew_hook = ' "$RENEWAL_CONF"; then
        found "other inline renew_hook in ${RENEWAL_CONF#"${ROOT}"}, left alone: $(sed -n 's/^renew_hook = //p' "$RENEWAL_CONF")"
    else
        ok "no inline renew_hook in ${RENEWAL_CONF#"${ROOT}"}"
    fi
fi

# --- Renewal timer ---
say ""
say "certbot renewal timer:"
TIMER="$(timer_unit)"
if [[ -z "$TIMER" ]]; then
    problem "no certbot.timer or snap.certbot.renew.timer; install certbot from the distribution package (apt-get install certbot)"
else
    enabled="$(systemctl is-enabled "$TIMER" 2>/dev/null || true)"
    active="$(systemctl is-active "$TIMER" 2>/dev/null || true)"
    found "${TIMER}: ${enabled:-unknown}, ${active:-unknown}"
    if [[ "$enabled" == "enabled" && "$active" == "active" ]]; then
        ok "${TIMER} is enabled and active"
    elif [[ "$MODE" == "check" ]]; then
        problem "${TIMER} is not enabled and active"
    else
        act "enable and start ${TIMER}" systemctl enable --now "$TIMER"
    fi
fi

# --- Permissions ---
say ""
say "Certificate file permissions:"
if ! getent group "$CADDY_GROUP" >/dev/null 2>&1; then
    warn "no group ${CADDY_GROUP} (Caddy not installed yet?); gce-start-hub.sh --full sets permissions when it installs Caddy"
elif [[ ! -d "${LE_DIR}/archive/${DOMAIN}" ]]; then
    warn "no ${LE_DIR}/archive/${DOMAIN}; skipping permission check"
elif perms_need_fix; then
    found "some certificate files are not readable by group ${CADDY_GROUP}"
    if [[ "$MODE" == "check" ]]; then problem "certificate files not readable by group ${CADDY_GROUP}"
    else act "give group ${CADDY_GROUP} read access to ${DOMAIN} under live/ and archive/" fix_perms; fi
else
    ok "group ${CADDY_GROUP} can read the certificate files"
fi

# --- Certificate on disk vs served ---
say ""
say "Certificate:"
disk_serial="$(cert_field "${LIVE}/fullchain.pem" serial)"
disk_end="$(cert_field "${LIVE}/fullchain.pem" enddate)"
found "on disk: serial ${disk_serial:-?}, notAfter ${disk_end:-?}"

if ! openssl x509 -in "${LIVE}/fullchain.pem" -noout -checkend $((RENEW_DAYS * 86400)) >/dev/null 2>&1; then
    found "the certificate on disk expires within ${RENEW_DAYS} days"
    if [[ "$MODE" == "check" ]]; then
        problem "certificate on disk expires within ${RENEW_DAYS} days"
    elif ! $has_lineage; then
        problem "cannot renew: certbot does not manage ${DOMAIN}"
    else
        act "renew it (the deploy hook reloads Caddy)" certbot renew --cert-name "$DOMAIN" --no-random-sleep-on-renew
        if [[ "$MODE" == "apply" ]]; then
            disk_serial="$(cert_field "${LIVE}/fullchain.pem" serial)"
            disk_end="$(cert_field "${LIVE}/fullchain.pem" enddate)"
            found "on disk after renew: serial ${disk_serial:-?}, notAfter ${disk_end:-?}"
        fi
    fi
else
    ok "valid for more than ${RENEW_DAYS} days; no renewal needed"
fi

if $caddy_live_path; then
    served="$(served_cert)"
    if [[ -z "$served" ]]; then
        warn "could not fetch the served certificate from ${CONNECT} (SNI ${SERVED_HOST}); is Caddy running?"
    else
        served_serial="$(printf '%s\n' "$served" | openssl x509 -noout -serial 2>/dev/null | sed 's/^[^=]*=//')"
        served_end="$(printf '%s\n' "$served" | openssl x509 -noout -enddate 2>/dev/null | sed 's/^[^=]*=//')"
        found "served:  serial ${served_serial:-?}, notAfter ${served_end:-?}"
        if [[ "$served_serial" == "$disk_serial" ]]; then
            ok "Caddy serves the certificate on disk"
        elif [[ "$MODE" == "check" ]]; then
            problem "Caddy serves an older certificate than the one on disk; a reload fixes it"
        else
            act "reload Caddy through the deploy hook (the hub is not restarted)" run_hook
        fi
    fi
fi

# --- Final check ---
if [[ "$MODE" == "apply" && $CHANGES -gt 0 ]]; then
    say ""
    say "Final check:"
    if exec_out="$(bash "$0" --check --domain "$DOMAIN" --host "$SERVED_HOST" --connect "$CONNECT" \
        --renew-days "$RENEW_DAYS" ${ROOT:+--root "$ROOT"} 2>&1)"; then
        printf '%s\n' "$exec_out" | sed 's/^/  | /'
    else
        printf '%s\n' "$exec_out" | sed 's/^/  | /'
        PROBLEMS=$((PROBLEMS + 1))
    fi
fi

say ""
case "$MODE" in
    check)
        if [[ $PROBLEMS -eq 0 ]]; then say "Result: OK, nothing to fix."
        else say "Result: ${PROBLEMS} problem(s). Run without --check (as root) to fix them."; fi
        ;;
    dry-run)
        if [[ $CHANGES -eq 0 ]]; then say "Result: nothing to change."
        else say "Result: ${CHANGES} change(s) planned; nothing was changed (dry run)."; fi
        ;;
    apply)
        if [[ $CHANGES -eq 0 ]]; then say "Result: nothing to change."
        else say "Result: ${CHANGES} change(s) applied."; fi
        ;;
esac
[[ $PROBLEMS -eq 0 ]]
