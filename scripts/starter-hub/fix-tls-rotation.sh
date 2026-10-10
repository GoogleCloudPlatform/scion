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
#      certbot runs once after each successful renewal: it gives Caddy's
#      group read access to the new files and reloads Caddy (never the hub).
#      It is always installed. Other hooks that appear to reload Caddy are
#      reported, never removed: Caddy is then reloaded twice (harmless);
#   2. removes the inline renew_hook that older gce-certs.sh versions stored
#      in /etc/letsencrypt/renewal/<domain>.conf (it failed before reaching
#      the reload, see ptone/scion#4207);
#   3. enables the certbot renewal timer;
#   4. checks, as the user Caddy runs as, that it can read the certificate
#      and key, and changes group or mode only on the paths that block it;
#   5. runs `certbot renew` only when the certificate on disk expires within
#      --renew-days, and reloads Caddy only when the certificate it serves
#      differs from the one on disk;
#   6. prints a final check.
#
# Re-running it changes nothing once the hub is in order. Run --help for the
# options. Tests run it unprivileged against a fake root (--root DIR, or
# FIX_TLS_ROOT); that option is for tests only.

set -euo pipefail

usage() {
    cat <<'USAGE'
Usage:
  sudo scripts/starter-hub/fix-tls-rotation.sh [--dry-run | --check]
      [--domain <cert-domain>] [--host <served-host>]
      [--connect <addr:port>] [--renew-days <n>]

  --dry-run          Print every action without changing anything.
  --check            Read-only report: certificate notAfter and serial (on
                     disk and as served), reload hooks, renewal timer, and
                     whether Caddy's user can read the certificate and key.
                     Exits 1 if something needs fixing.
  --domain NAME      Certificate name, the directory under
                     /etc/letsencrypt/live/. Default: taken from the tls
                     line of /etc/caddy/Caddyfile, else the only directory
                     under /etc/letsencrypt/live/.
  --host NAME        Server name (SNI) used to fetch the served certificate.
                     Default: the first site address in the Caddyfile.
  --connect ADDR     Where to fetch the served certificate. Default
                     127.0.0.1:443.
  --renew-days N     Run `certbot renew` when the certificate on disk expires
                     within N days (1-30, default 30). certbot itself only
                     renews within its renew_before_expiry window, 30 days
                     by default, so larger values are rejected.
USAGE
}

MODE=apply
DOMAIN=""
SERVED_HOST=""
CONNECT="127.0.0.1:443"
RENEW_DAYS=30
ROOT="${FIX_TLS_ROOT:-}"
HOOK_NAME="scion-reload-caddy.sh"

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

if ! [[ "$RENEW_DAYS" =~ ^[1-9][0-9]?$ ]] || (( RENEW_DAYS > 30 )); then
    echo "Error: --renew-days must be a whole number from 1 to 30. certbot renews only" >&2
    echo "within its own renew_before_expiry window (30 days by default), so an earlier" >&2
    echo "'certbot renew' would do nothing." >&2
    exit 2
fi

LE_DIR="${ROOT}/etc/letsencrypt"
HOOK_DIR="${LE_DIR}/renewal-hooks/deploy"
HOOK_PATH="${HOOK_DIR}/${HOOK_NAME}"
CADDYFILE="${ROOT}/etc/caddy/Caddyfile"

# Other hooks are classified for the report only; the result never decides
# whether the scion hook is installed (it always is). Classification looks
# at hook_code output (comments, quoted strings and echo/printf removed): a
# hook "appears to reload Caddy" if that matches SYSTEMCTL_RELOAD_RE, or a
# `caddy reload` command segment (up to the next ; & or |) contains --force.
# A bare `caddy reload` is reported separately: Caddy skips it when the
# Caddyfile is unchanged.
SYSTEMCTL_RELOAD_RE='systemctl[[:space:]]+(reload|restart|reload-or-restart|try-reload-or-restart)[[:space:]]+caddy(\.service)?([[:space:];&|)]|$)'
CADDY_RELOAD_RE='caddy[[:space:]]+reload([[:space:]]|$)'
CADDY_RELOAD_SEGMENT_RE='caddy[[:space:]]+reload([[:space:]][^;&|]*)?'
FORCE_RE='(^|[[:space:]])(--force|-f)([[:space:])]|$)'
# The inline hook older gce-certs.sh runs stored (plain or configobj-quoted).
BROKEN_INLINE_RE='^renew_hook[[:space:]]*=.*RENEWED_DOMAINS%%,\*'
INLINE_RE='^renew_hook[[:space:]]*='

if [[ "$MODE" == "apply" && -z "$ROOT" && "${EUID}" -ne 0 ]]; then
    echo "Error: run as root (sudo), or use --dry-run or --check." >&2
    exit 1
fi

CHANGES=0
PROBLEMS=0

say() { echo "$*"; }
found() { echo "  found:   $*"; }
ok() { echo "  ok:      $*"; }
note() { echo "  note:    $*"; }
warn() { echo "  WARN:    $*"; }
problem() { echo "  PROBLEM: $*"; PROBLEMS=$((PROBLEMS + 1)); }
show() { printf '%s' "${1#"${ROOT}"}"; }

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

# reload_snippet -- shell code that reloads Caddy, shared by the deploy hook
# and by this script's own reload.
reload_snippet() {
    cat <<'RELOAD'
caddyfile=/etc/caddy/Caddyfile
rc=0
caddy_has_force() { caddy reload --help 2>&1 | grep -q -- '--force'; }
reload_failed() {
    echo "scion-reload-caddy: ERROR: reload failed ($1); Caddy may still serve the old certificate" >&2
    rc=1
}
if systemctl is-active --quiet caddy 2>/dev/null; then
    # Caddy skips a reload whose configuration is unchanged unless it is
    # forced (caddy reload --force). Some packaged caddy.service files reload
    # without --force; then the new certificate files would not be read.
    if systemctl show -p ExecReload caddy 2>/dev/null | grep -q -- '--force'; then
        if systemctl reload caddy; then
            echo "scion-reload-caddy: reloaded Caddy (systemctl reload caddy)"
        else
            reload_failed "systemctl reload caddy"
        fi
    elif caddy_has_force; then
        if caddy reload --config "$caddyfile" --force; then
            echo "scion-reload-caddy: reloaded Caddy (caddy reload --force)"
        else
            reload_failed "caddy reload --force"
        fi
    elif systemctl reload caddy; then
        echo "scion-reload-caddy: reloaded Caddy (systemctl reload caddy, without --force)"
        echo "scion-reload-caddy: WARNING: caddy.service reloads without --force and this caddy has no 'reload --force'; with an unchanged Caddyfile it may keep serving the old certificate. Check the served certificate; restarting Caddy (not the hub) makes it re-read the files." >&2
    else
        reload_failed "systemctl reload caddy"
    fi
elif systemctl cat caddy >/dev/null 2>&1; then
    echo "scion-reload-caddy: caddy.service is not running; it reads the new files when it starts"
elif command -v caddy >/dev/null 2>&1; then
    # Caddy without systemd: reload it through its admin endpoint.
    if caddy_has_force && caddy reload --config "$caddyfile" --force; then
        echo "scion-reload-caddy: reloaded Caddy (not under systemd)"
    else
        reload_failed "caddy reload --config $caddyfile --force"
    fi
else
    echo "scion-reload-caddy: Caddy is not installed, nothing to reload"
fi
exit "$rc"
RELOAD
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
# certificate from that directory only at start and on reload, so give
# Caddy's group read access to the files the live links now point to, then
# reload Caddy. A failed permission step only warns: the reload always runs.
# The hub itself is never restarted.
set -u

lineage="${RENEWED_LINEAGE:-}"
if [ -z "$lineage" ] || [ ! -d "$lineage" ]; then
    echo "scion-reload-caddy: RENEWED_LINEAGE is unset or not a directory" >&2
    exit 1
fi
le_dir="$(dirname "$(dirname "$lineage")")"

caddy_user="$(systemctl show -p User --value caddy 2>/dev/null || true)"
caddy_user="${caddy_user:-caddy}"
if group="$(id -gn "$caddy_user" 2>/dev/null)"; then
    perm() { # MODE PATH: chgrp to Caddy's group and add MODE; warn on failure
        chgrp "$group" "$2" && chmod "$1" "$2" \
            || echo "scion-reload-caddy: WARNING: could not give group $group access to $2" >&2
    }
    perm g+x "$le_dir/live"
    perm g+x "$le_dir/archive"
    perm g+x "$lineage"
    for f in cert chain fullchain privkey; do
        [ -e "$lineage/$f.pem" ] || continue
        target="$(readlink -f "$lineage/$f.pem")"
        perm g+x "$(dirname "$target")"
        perm g+r "$target"
    done
else
    echo "scion-reload-caddy: no user $caddy_user, permissions left as they are" >&2
fi

HOOK
    reload_snippet
}

reload_caddy() {
    if ! bash -c "$(reload_snippet)"; then
        problem "reloading Caddy failed (see the ERROR above); the hub was not touched"
    fi
}

renew_cert() {
    if ! certbot renew --cert-name "$DOMAIN" --no-random-sleep-on-renew; then
        problem "certbot renew failed or a renewal hook failed; see /var/log/letsencrypt/letsencrypt.log"
    fi
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

# own_ok KIND PATH -- true if PATH's own owner, group and mode let
# CADDY_USER traverse it (KIND dir) or read it (KIND file). Ancestors are
# checked separately, so each blocking path is found on its own.
own_ok() {
    local need=4 owner group mode
    [[ "$1" == "dir" ]] && need=1
    read -r owner group mode < <(stat -c '%U %G %a' "$2")
    mode=$((8#$mode))
    if [[ "$owner" == "$CADDY_USER" ]]; then
        (( (mode >> 6) & need ))
    elif [[ " ${CADDY_GROUPS} " == *" ${group} "* ]]; then
        (( (mode >> 3) & need ))
    else
        (( mode & need ))
    fi
}

# fix_one KIND PATH -- give CADDY_GROUP traverse (dir) or read (file) access.
fix_one() {
    chgrp "$CADDY_GROUP" "$2"
    if [[ "$1" == "dir" ]]; then chmod g+x "$2"; else chmod g+r "$2"; fi
}

# can_read_as_caddy PATH -- the real test, run as CADDY_USER.
can_read_as_caddy() {
    runuser -u "$CADDY_USER" -- test -r "$1"
}

# hook_code -- the code of a hook read on stdin, with full-line and trailing
# # comments, quoted strings and echo/printf commands removed, so that text
# which only mentions a reload is not reported as one. It is a heuristic
# (here-documents, for example, are not understood) and is used only for
# the report about other hooks; the scion hook is installed regardless.
hook_code() {
    sed -E \
        -e 's/^[[:space:]]*#.*//' \
        -e "s/'[^']*'//g" \
        -e 's/"[^"]*"//g' \
        -e 's/(^|[[:space:]])#.*$//' \
        -e 's/(^|[;&|(]|then|else|do)([[:space:]]*)(echo|printf)([[:space:]][^;&|)]*)?/\1\2/g'
}

# inline_hook_code FILE -- the code of the inline renew_hook in FILE, with
# the configobj quotes around the whole value removed.
inline_hook_code() {
    sed -n -E 's/^renew_hook[[:space:]]*=[[:space:]]*//p' "$1" \
        | sed -E -e 's/^"(.*)"[[:space:]]*$/\1/' -e "s/^'(.*)'[[:space:]]*\$/\\1/" \
        | hook_code
}

# reload_kind -- reads hook code on stdin; prints yes, noforce or no.
reload_kind() {
    local code
    code="$(cat)"
    if grep -Eq "$SYSTEMCTL_RELOAD_RE" <<<"$code"; then
        echo yes
    elif grep -Eo "$CADDY_RELOAD_SEGMENT_RE" <<<"$code" | grep -Eq -- "$FORCE_RE"; then
        echo yes
    elif grep -Eq "$CADDY_RELOAD_RE" <<<"$code"; then
        echo noforce
    else
        echo no
    fi
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
    sed -i -E "/${BROKEN_INLINE_RE}/d" "$conf"
}

# ---------------------------------------------------------------------------
say "=== Starter hub TLS rotation (${MODE}) ==="

# --- What Caddy serves ---
say ""
say "Caddy configuration:"
caddy_live_path=true
caddy_unit=false
if systemctl cat caddy >/dev/null 2>&1; then caddy_unit=true; fi
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
        warn "no 'tls <cert> <key>' line in $(show "$CADDYFILE")"
    fi
else
    warn "no $(show "$CADDYFILE")"
fi
CADDY_USER=""
if $caddy_unit; then
    CADDY_USER="$(systemctl show -p User --value caddy 2>/dev/null || true)"
    if systemctl show -p ExecReload caddy 2>/dev/null | grep -q -- '--force'; then
        ok "caddy.service reloads with --force"
    else
        warn "caddy.service reloads without --force; Caddy may skip a reload when the Caddyfile is unchanged. The deploy hook uses 'caddy reload --force' instead when caddy supports it."
    fi
else
    warn "no caddy systemd unit (Caddy not installed yet?)"
fi
CADDY_USER="${CADDY_USER:-caddy}"

if [[ -d "${LE_DIR}/live" && ! -x "${LE_DIR}/live" ]]; then
    echo "Error: cannot read $(show "${LE_DIR}/live") (run with sudo)." >&2
    exit 1
fi
if [[ -z "$DOMAIN" && -d "${LE_DIR}/live" ]]; then
    mapfile -t lives < <(find "${LE_DIR}/live" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort)
    if [[ ${#lives[@]} -eq 1 ]]; then
        DOMAIN="${lives[0]}"
    elif [[ ${#lives[@]} -gt 1 ]]; then
        echo "Error: several certificates under $(show "${LE_DIR}/live") (${lives[*]}); pass --domain." >&2
        exit 1
    fi
fi
if [[ -z "$DOMAIN" ]]; then
    echo "Error: no certificate found under $(show "${LE_DIR}/live"); pass --domain, or run gce-certs.sh first." >&2
    exit 1
fi
[[ -n "$SERVED_HOST" ]] || SERVED_HOST="$DOMAIN"
LIVE="${LE_DIR}/live/${DOMAIN}"
RENEWAL_CONF="${LE_DIR}/renewal/${DOMAIN}.conf"
found "certificate name ${DOMAIN}, served host ${SERVED_HOST}"

if [[ ! -e "${LIVE}/fullchain.pem" ]]; then
    if [[ -d "$LIVE" && ! -x "$LIVE" ]] || [[ -L "${LIVE}/fullchain.pem" ]]; then
        echo "Error: cannot read $(show "${LIVE}/fullchain.pem") (run with sudo)." >&2
    else
        echo "Error: $(show "${LIVE}/fullchain.pem") does not exist." >&2
    fi
    exit 1
fi
if [[ ! -r "${LIVE}/fullchain.pem" ]]; then
    echo "Error: cannot read $(show "${LIVE}/fullchain.pem") (run with sudo)." >&2
    exit 1
fi
has_lineage=false
if [[ -f "$RENEWAL_CONF" ]]; then
    has_lineage=true
elif [[ -d "${LE_DIR}/renewal" && ! -x "${LE_DIR}/renewal" ]]; then
    echo "Error: cannot read $(show "${LE_DIR}/renewal") (run with sudo)." >&2
    exit 1
else
    warn "no $(show "$RENEWAL_CONF"): certbot does not manage this certificate, so it is never renewed automatically"
fi

# --- Reload hooks ---
say ""
say "certbot reload hooks:"
# Other hooks are reported by name only, never by content, and never decide
# whether the scion hook is installed.
MOVE_ASIDE="Caddy will be reloaded twice per renewal, which is harmless; you may move it aside"
if [[ -d "$HOOK_DIR" ]]; then
    while IFS= read -r -d '' f; do
        base="$(basename "$f")"
        [[ "$base" == "$HOOK_NAME" || "$base" == ".${HOOK_NAME}."* ]] && continue
        if [[ ! -x "$f" ]]; then
            found "deploy hook ${base} is not executable, so certbot ignores it; left alone"
            continue
        fi
        code="$(hook_code < "$f" 2>/dev/null || true)"
        case "$(reload_kind <<<"$code")" in
            yes)
                found "deploy hook ${base}; left alone"
                note "existing hook ${base} also appears to reload Caddy; ${MOVE_ASIDE}: sudo mv $(show "$HOOK_DIR")/${base} /root/${base}.bak"
                ;;
            noforce)
                found "deploy hook ${base}; left alone"
                note "existing hook ${base} runs 'caddy reload' without --force, which Caddy skips when the Caddyfile is unchanged"
                ;;
            *)
                found "deploy hook ${base} does not appear to reload Caddy; left alone"
                ;;
        esac
    done < <(find "$HOOK_DIR" -mindepth 1 -maxdepth 1 \( -type f -o -type l \) -print0 | sort -z)
fi

if $has_lineage; then
    if grep -Eq "$BROKEN_INLINE_RE" "$RENEWAL_CONF"; then
        found "inline renew_hook from an older gce-certs.sh in $(show "$RENEWAL_CONF") (it fails before reloading Caddy)"
        if [[ "$MODE" == "check" ]]; then problem "broken inline renew_hook present"
        else act "remove it (backup $(show "$RENEWAL_CONF").bak-fix-tls-rotation)" remove_inline_hook "$RENEWAL_CONF"; fi
    elif grep -Eq "$INLINE_RE" "$RENEWAL_CONF"; then
        # Inline hooks can carry credentials: never print their content.
        found "an inline renew_hook is present (content not shown); left alone"
        case "$(reload_kind <<<"$(inline_hook_code "$RENEWAL_CONF")")" in
            yes) note "the inline renew_hook also appears to reload Caddy; ${MOVE_ASIDE} by removing the renew_hook line from $(show "$RENEWAL_CONF") after copying the file" ;;
            noforce) note "the inline renew_hook runs 'caddy reload' without --force, which Caddy skips when the Caddyfile is unchanged" ;;
        esac
    else
        ok "no inline renew_hook in $(show "$RENEWAL_CONF")"
    fi
fi

if [[ -f "$HOOK_PATH" ]] && cmp -s "$HOOK_PATH" <(hook_content) && [[ -x "$HOOK_PATH" ]]; then
    ok "$(show "$HOOK_PATH") is installed and current"
elif [[ -f "$HOOK_PATH" ]]; then
    found "$(show "$HOOK_PATH") is out of date or not executable"
    if [[ "$MODE" == "check" ]]; then problem "deploy hook $(show "$HOOK_PATH") out of date"
    else act "update the deploy hook $(show "$HOOK_PATH")" install_hook; fi
else
    found "$(show "$HOOK_PATH") is missing"
    if [[ "$MODE" == "check" ]]; then problem "the scion deploy hook is not installed, so nothing is known to reload Caddy after a renewal"
    else act "install the deploy hook $(show "$HOOK_PATH")" install_hook; fi
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
say "Certificate file permissions (as the user Caddy runs as):"
if ! id -u "$CADDY_USER" >/dev/null 2>&1; then
    warn "no user ${CADDY_USER} (Caddy not installed yet?); gce-start-hub.sh --full sets permissions when it installs Caddy"
else
    CADDY_GROUP="$(id -gn "$CADDY_USER")"
    CADDY_GROUPS="$(id -Gn "$CADDY_USER")"
    found "Caddy runs as user ${CADDY_USER} (group ${CADDY_GROUP})"
    # Paths Caddy needs, ancestors first: directories to traverse, then the
    # files the live symlinks point to.
    targets=()
    for d in "$LE_DIR" "${LE_DIR}/live" "$LIVE" "${LE_DIR}/archive" "${LE_DIR}/archive/${DOMAIN}"; do
        [[ -d "$d" ]] && targets+=("dir $d")
    done
    for f in fullchain.pem privkey.pem; do
        if [[ -e "${LIVE}/${f}" ]]; then
            real="$(readlink -f "${LIVE}/${f}")"
            parent="$(dirname "$real")"
            if [[ " ${targets[*]} " != *" dir ${parent} "* ]]; then targets+=("dir ${parent}"); fi
            targets+=("file ${real}")
        else
            problem "$(show "${LIVE}/${f}") does not exist"
        fi
    done
    # unreadable_files -- the live files CADDY_USER cannot read: tested as
    # that user with runuser when possible, else judged from owner, group and
    # mode along the way.
    can_runuser=false
    if [[ "${EUID}" -eq 0 || -n "$ROOT" ]] && command -v runuser >/dev/null 2>&1; then
        can_runuser=true
    fi
    unreadable_files() {
        local f t
        if $can_runuser; then
            for f in fullchain.pem privkey.pem; do
                can_read_as_caddy "${LIVE}/${f}" || show "${LIVE}/${f} "
            done
        else
            for t in "${targets[@]}"; do
                if ! own_ok "${t%% *}" "${t#* }"; then
                    show "${LIVE}/fullchain.pem "
                    show "${LIVE}/privkey.pem "
                    return
                fi
            done
        fi
    }
    if $can_runuser; then
        how="tested as that user"
    elif [[ "${EUID}" -eq 0 || -n "$ROOT" ]]; then
        how="runuser not found, so judged from owner, group and mode only"
    else
        how="judged from owner, group and mode only; run with sudo to test as that user"
    fi

    unreadable="$(unreadable_files)"
    if [[ -z "$unreadable" ]]; then
        # Readable already: change nothing (chmod on a path with an ACL
        # would change its mask).
        ok "user ${CADDY_USER} can read the certificate and key (${how})"
    else
        blocked=0
        for t in "${targets[@]}"; do
            kind="${t%% *}"
            p="${t#* }"
            own_ok "$kind" "$p" && continue
            blocked=$((blocked + 1))
            read -r og om < <(stat -c '%G %a' "$p")
            if [[ "$kind" == "dir" ]]; then verb="traverse"; nm=$(( 8#$om | 8#010 )); else verb="read"; nm=$(( 8#$om | 8#040 )); fi
            nm="$(printf '%o' "$nm")"
            found "user ${CADDY_USER} cannot ${verb} $(show "$p") (group ${og}, mode ${om})"
            if [[ "$MODE" != "check" ]]; then
                act "set $(show "$p"): group ${og} -> ${CADDY_GROUP}, mode ${om} -> ${nm}" fix_one "$kind" "$p"
            fi
        done
        if [[ $blocked -eq 0 ]]; then
            problem "user ${CADDY_USER} cannot read ${unreadable% } (not explained by owner, group or mode; check ACLs or security modules)"
        elif [[ "$MODE" == "check" ]]; then
            problem "user ${CADDY_USER} cannot read ${unreadable% } (blocked by the paths above)"
        elif [[ "$MODE" == "dry-run" ]]; then
            note "the changes above give user ${CADDY_USER} access; the real run checks it again"
        else
            unreadable="$(unreadable_files)"
            if [[ -z "$unreadable" ]]; then
                ok "user ${CADDY_USER} can read the certificate and key (${how})"
            else
                problem "user ${CADDY_USER} still cannot read ${unreadable% } after the changes above"
            fi
        fi
    fi
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
        act "renew it (the deploy hook reloads Caddy): certbot renew --cert-name ${DOMAIN} --no-random-sleep-on-renew" renew_cert
        if [[ "$MODE" == "apply" ]]; then
            before_serial="$disk_serial"
            disk_serial="$(cert_field "${LIVE}/fullchain.pem" serial)"
            disk_end="$(cert_field "${LIVE}/fullchain.pem" enddate)"
            if [[ "$disk_serial" == "$before_serial" ]]; then
                warn "certbot did not renew it: not yet due under renew_before_expiry in $(show "$RENEWAL_CONF"); the timer renews it when it is due"
            else
                found "on disk after renew: serial ${disk_serial:-?}, notAfter ${disk_end:-?}"
            fi
        fi
    fi
else
    ok "valid for more than ${RENEW_DAYS} days; no renewal needed"
fi

if $caddy_live_path; then
    served="$(served_cert)"
    if [[ -z "$served" ]]; then
        if [[ -f "$CADDYFILE" ]] && $caddy_unit; then
            problem "could not fetch the served certificate from ${CONNECT} (SNI ${SERVED_HOST}), though Caddy is configured; is Caddy running?"
        else
            warn "could not fetch the served certificate from ${CONNECT} (SNI ${SERVED_HOST}); Caddy is not set up yet"
        fi
    else
        served_serial="$(printf '%s\n' "$served" | openssl x509 -noout -serial 2>/dev/null | sed 's/^[^=]*=//')"
        served_end="$(printf '%s\n' "$served" | openssl x509 -noout -enddate 2>/dev/null | sed 's/^[^=]*=//')"
        found "served:  serial ${served_serial:-?}, notAfter ${served_end:-?}"
        if [[ "$served_serial" == "$disk_serial" ]]; then
            ok "Caddy serves the certificate on disk"
        elif [[ "$MODE" == "check" ]]; then
            problem "Caddy serves an older certificate than the one on disk; a reload fixes it"
        else
            act "reload Caddy (the hub is not restarted)" reload_caddy
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
