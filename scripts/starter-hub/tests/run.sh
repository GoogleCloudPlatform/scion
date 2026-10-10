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

# scripts/starter-hub/tests/run.sh - tests for the starter-hub scripts.
#
# This never contacts GCP. It puts tests/lib (stub gcloud, curl and sleep)
# first on PATH, runs each script as a real subprocess from the repository
# root, and asserts on its exit code, its output, and what it asked gcloud
# and curl to do. The fix-tls-rotation.sh tests also put tests/lib-tls
# (stub systemctl, certbot, caddy, runuser, id, and an openssl that only
# fakes s_client) first on PATH and run against a fake root in a temp
# directory. Needs bash and openssl.
#
# Usage:
#   scripts/starter-hub/tests/run.sh

# Deliberately no -e: assertions run commands whose non-zero exit is expected.
set -u

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STARTER_DIR="$(cd "${TESTS_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${STARTER_DIR}/../.." && pwd)"

PASS=0
FAIL=0
CURRENT=""
EXTRA_ENV=()

fail() {
    echo "  FAIL [${CURRENT}]: $1"
    FAIL=$((FAIL + 1))
}

pass() {
    PASS=$((PASS + 1))
}

assert_eq() { # want got message
    if [[ "$1" == "$2" ]]; then pass; else fail "$3 (want '$1', got '$2')"; fi
}

assert_ne() { # notwant got message
    if [[ "$1" != "$2" ]]; then pass; else fail "$3 (got '$2')"; fi
}

assert_contains() { # haystack needle message
    if [[ "$1" == *"$2"* ]]; then pass; else fail "$3 (missing '$2')"; fi
}

assert_not_contains() { # haystack needle message
    if [[ "$1" != *"$2"* ]]; then pass; else fail "$3 (found '$2')"; fi
}

# fresh_state -- new empty stub logs for one test.
fresh_state() {
    STATE_DIR="$(mktemp -d)"
    export STUB_LOG="${STATE_DIR}/calls.log"
    export STUB_SSH_LOG="${STATE_DIR}/ssh.log"
    export STUB_SCP_DIR="${STATE_DIR}/scp"
    mkdir -p "${STUB_SCP_DIR}"
    : > "${STUB_LOG}"
    : > "${STUB_SSH_LOG}"
}

# run_script SCRIPT [ARGS...] -- runs a starter-hub script from the repo
# root against the stubs, with the configuration variables these tests
# care about cleared. Extra environment comes from the caller (VAR=x
# run_script ...). Sets OUT (stdout+stderr) and RC.
run_script() {
    local script="$1"
    shift
    OUT="$(cd "${REPO_ROOT}" && env \
        -u CERT_EMAIL -u SKIP_TLS -u HEALTH_CHECK_INSECURE -u HUB_BASE_URL \
        -u DNS_ZONE_DESCRIPTION -u PROJECT_ID -u HUB_NAME -u BASE_DOMAIN \
        -u HUB_DOMAIN -u CERT_DOMAIN -u ENABLE_GKE \
        PATH="${TESTS_DIR}/lib:${PATH}" \
        HUB_ENV_FILE="${STATE_DIR}/no-such-hub.env" \
        "${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"}" \
        bash "scripts/starter-hub/${script}" "$@" 2>&1)"
    RC=$?
    CALLS="$(cat "${STUB_LOG}")"
    # Top-level curl calls only; the logged ssh command text also mentions curl.
    CURL_CALLS="$(grep '^curl ' "${STUB_LOG}")"
}

# --- hub-config.sh / CERT_EMAIL (#3359) ---

test_hub_config_has_no_cert_email_default() {
    fresh_state
    local got
    # shellcheck disable=SC2016 # expands in the child bash, not here
    got="$(env -u CERT_EMAIL PATH="${TESTS_DIR}/lib:${PATH}" bash -c \
        'source "$1"; printf "%s" "${CERT_EMAIL}"' _ "${STARTER_DIR}/hub-config.sh")"
    assert_eq "" "$got" "CERT_EMAIL is empty unless the operator sets it"
}

test_hub_config_zone_description_is_generic() {
    fresh_state
    local got
    # shellcheck disable=SC2016 # expands in the child bash, not here
    got="$(env -u DNS_ZONE_DESCRIPTION -u CERT_DOMAIN -u HUB_NAME -u BASE_DOMAIN \
        PATH="${TESTS_DIR}/lib:${PATH}" HUB_NAME=staging BASE_DOMAIN=example.com bash -c \
        'source "$1"; printf "%s" "${DNS_ZONE_DESCRIPTION}"' _ "${STARTER_DIR}/hub-config.sh")"
    assert_eq "Scion Hub zone for staging.example.com" "$got" "zone description follows CERT_DOMAIN"
}

test_certs_requires_cert_email() {
    fresh_state
    EXTRA_ENV=()
    run_script gce-certs.sh
    assert_ne 0 "$RC" "gce-certs.sh fails without CERT_EMAIL"
    assert_contains "$OUT" "CERT_EMAIL is not set" "clear error message"
    assert_not_contains "$CALLS" "dns managed-zones" "fails before touching DNS"
    assert_not_contains "$CALLS" "compute ssh" "fails before requesting a certificate"
}

test_certs_uses_cert_email_and_zone_description() {
    fresh_state
    EXTRA_ENV=(CERT_EMAIL=admin@example.com)
    run_script gce-certs.sh
    assert_eq 0 "$RC" "gce-certs.sh succeeds with CERT_EMAIL set"
    assert_contains "$CALLS" "--description=Scion Hub zone for demo.scion-ai.dev" "default zone description"

    fresh_state
    EXTRA_ENV=(CERT_EMAIL=admin@example.com "DNS_ZONE_DESCRIPTION=My zone")
    run_script gce-certs.sh
    assert_contains "$CALLS" "--description=My zone" "DNS_ZONE_DESCRIPTION override"
}

test_deploy_requires_cert_email_before_provisioning() {
    fresh_state
    EXTRA_ENV=()
    run_script gce-demo-deploy.sh
    assert_ne 0 "$RC" "gce-demo-deploy.sh fails without CERT_EMAIL"
    assert_contains "$OUT" "CERT_EMAIL is not set" "clear error message"
    assert_not_contains "$OUT" "Step 0" "fails before the first step"
    assert_not_contains "$CALLS" "compute " "no compute calls"
}

test_deploy_skip_tls_does_not_require_cert_email() {
    fresh_state
    EXTRA_ENV=(SKIP_TLS=true)
    run_script gce-demo-deploy.sh
    # The sub-steps (preflight first) run for real against the stubs and may
    # stop the run; this test only checks that the CERT_EMAIL guard lets
    # SKIP_TLS=true through to the first step.
    assert_not_contains "$OUT" "CERT_EMAIL is not set" "SKIP_TLS=true does not need CERT_EMAIL"
    assert_contains "$OUT" "Step 0" "reaches the first step"

    # Wiring: under SKIP_TLS the script skips gce-certs.sh and passes --no-tls.
    local deploy
    deploy="$(cat "${STARTER_DIR}/gce-demo-deploy.sh")"
    assert_contains "$deploy" "SKIP_TLS=true: skipping DNS and certificate setup (gce-certs.sh)." \
        "gce-certs.sh is guarded by SKIP_TLS"
    assert_contains "$deploy" "gce-start-hub.sh --full --no-tls" "passes --no-tls under SKIP_TLS"
}

# --- gce-start-hub.sh final health check (#3360) ---

HEALTHY='{"status":"healthy"}'

test_health_check_verifies_tls_by_default() {
    fresh_state
    EXTRA_ENV=("CURL_STUB_BODY=${HEALTHY}")
    run_script gce-start-hub.sh
    assert_eq 0 "$RC" "healthy hub passes"
    assert_contains "$CURL_CALLS" "curl -s https://hub.demo.scion-ai.dev/healthz" "HTTPS check runs"
    assert_not_contains "$CURL_CALLS" "curl -s -k" "certificate is verified (no -k)"
}

test_health_check_fails_clearly_on_tls_error() {
    fresh_state
    EXTRA_ENV=(CURL_STUB_RC=60)
    run_script gce-start-hub.sh
    assert_eq 1 "$RC" "invalid certificate fails the check"
    assert_contains "$OUT" "TLS is not valid for https://hub.demo.scion-ai.dev" "names the TLS problem"
    assert_contains "$OUT" "--insecure-health-check" "names the explicit opt-out"
}

test_health_check_insecure_is_explicit() {
    fresh_state
    EXTRA_ENV=("CURL_STUB_BODY=${HEALTHY}")
    run_script gce-start-hub.sh --insecure-health-check
    assert_eq 0 "$RC" "insecure check passes"
    assert_contains "$CURL_CALLS" "curl -s -k https://" "flag adds -k"
    assert_contains "$OUT" "TLS certificate is NOT verified" "output says verification is off"

    fresh_state
    EXTRA_ENV=("CURL_STUB_BODY=${HEALTHY}" HEALTH_CHECK_INSECURE=true)
    run_script gce-start-hub.sh
    assert_contains "$CURL_CALLS" "curl -s -k https://" "HEALTH_CHECK_INSECURE=true adds -k"
}

# --- gce-start-hub.sh --full --no-tls (#4059) ---

test_full_default_writes_caddyfile() {
    fresh_state
    EXTRA_ENV=("CURL_STUB_BODY=${HEALTHY}")
    run_script gce-start-hub.sh --full
    assert_eq 0 "$RC" "full deploy passes"
    if [[ -f "${STUB_SCP_DIR}/Caddyfile" ]]; then pass; else fail "Caddyfile uploaded"; fi
    assert_contains "$(cat "${STUB_SCP_DIR}/scion-hub.service")" \
        'SCION_SERVER_BASE_URL=https://hub.demo.scion-ai.dev"' "HTTPS base URL"
    assert_contains "$(cat "${STUB_SSH_LOG}")" "SKIP_TLS=false" "remote session keeps Caddy"
}

test_full_no_tls_skips_caddy() {
    local mode
    for mode in flag env; do
        fresh_state
        if [[ "$mode" == flag ]]; then
            EXTRA_ENV=()
            run_script gce-start-hub.sh --full --no-tls
        else
            EXTRA_ENV=(SKIP_TLS=true)
            run_script gce-start-hub.sh --full
        fi
        assert_eq 0 "$RC" "${mode}: --full without TLS completes"
        if [[ -f "${STUB_SCP_DIR}/Caddyfile" ]]; then fail "${mode}: no Caddyfile uploaded"; else pass; fi
        if [[ -f "${STUB_SCP_DIR}/scion-hub.service" ]]; then pass; else fail "${mode}: unit file still uploaded"; fi
        assert_contains "$(cat "${STUB_SCP_DIR}/scion-hub.service")" \
            'SCION_SERVER_BASE_URL=http://hub.demo.scion-ai.dev:8080"' "${mode}: plain HTTP base URL"
        assert_contains "$(cat "${STUB_SSH_LOG}")" "SKIP_TLS=true" "${mode}: remote session skips Caddy"
        assert_eq "" "$CURL_CALLS" "${mode}: no HTTPS health check"
        assert_contains "$OUT" "Skipping the HTTPS health check" "${mode}: says the check is skipped"
    done
}

test_start_hub_help_mentions_new_flags() {
    fresh_state
    EXTRA_ENV=()
    run_script gce-start-hub.sh --help
    assert_eq 0 "$RC" "--help exits 0"
    assert_contains "$OUT" "--no-tls" "help documents --no-tls"
    assert_contains "$OUT" "--insecure-health-check" "help documents --insecure-health-check"
}

# --- fix-tls-rotation.sh (ptone/scion#4207) ---
#
# Runs fix-tls-rotation.sh against a fake root in a temp directory, with the
# stub systemctl, certbot, caddy, openssl, runuser and id from tests/lib-tls
# first on PATH. Certificates are throwaway self-signed ones made here. The
# Caddy user is a made-up user (TLS_CADDY_USER) that owns nothing; its group
# is a real group of the test user, so chgrp works without root.

TLS_DOMAIN="example.com"
TLS_CADDY_USER="caddy-test"

# tls_caddy_group -- a named secondary group of the test user if there is
# one (so a group change shows up), else the primary group.
tls_caddy_group() {
    local primary g
    primary="$(id -gn)"
    for g in $(id -Gn 2>/dev/null); do
        if [[ "$g" != "$primary" ]] && getent group "$g" >/dev/null 2>&1; then
            echo "$g"
            return
        fi
    done
    echo "$primary"
}
TLS_CADDY_GROUP="$(tls_caddy_group)"

# tls_cert DIR N DAYS -- writes archive version N, valid for DAYS days.
tls_cert() {
    openssl req -x509 -newkey rsa:2048 -nodes -days "$3" -subj "/CN=${TLS_DOMAIN}" \
        -keyout "$1/privkey$2.pem" -out "$1/cert$2.pem" 2>/dev/null
    chmod 0600 "$1/privkey$2.pem"
    cp "$1/cert$2.pem" "$1/fullchain$2.pem"
    cp "$1/cert$2.pem" "$1/chain$2.pem"
}

# tls_fake_root DAYS STALE -- a hub as an older gce-certs.sh left it: the
# live certificate is valid for DAYS days, the broken inline renew_hook is in
# the renewal config, no deploy hook, certbot.timer disabled, live/ and
# archive/ 0700 and private keys 0600 (Caddy cannot read them). STALE=true:
# Caddy still serves an older certificate.
tls_fake_root() {
    fresh_state
    TLS_ROOT="${STATE_DIR}/root"
    export STUB_TLS_STATE="${STATE_DIR}/tls"
    TLS_LE="${TLS_ROOT}/etc/letsencrypt"
    TLS_CONF="${TLS_LE}/renewal/${TLS_DOMAIN}.conf"
    TLS_HOOKS="${TLS_LE}/renewal-hooks/deploy"
    local arch="${TLS_LE}/archive/${TLS_DOMAIN}"
    mkdir -p "${arch}" "${TLS_LE}/live/${TLS_DOMAIN}" "${TLS_LE}/renewal" \
        "${TLS_HOOKS}" "${TLS_ROOT}/etc/caddy" "${STUB_TLS_STATE}"
    tls_cert "${arch}" 1 1
    tls_cert "${arch}" 2 "$1"
    for f in cert chain fullchain privkey; do
        ln -s "../../archive/${TLS_DOMAIN}/${f}2.pem" "${TLS_LE}/live/${TLS_DOMAIN}/${f}.pem"
    done
    chmod 0700 "${TLS_LE}/live" "${TLS_LE}/archive"
    # shellcheck disable=SC2016 # literal text as certbot stores it
    printf '%s\n' '[renewalparams]' 'authenticator = dns-google' \
        'renew_hook = chown root:caddy /etc/letsencrypt/live /etc/letsencrypt/archive && chown -R root:caddy /etc/letsencrypt/live/${RENEWED_DOMAINS%%,*} && (systemctl reload caddy)' \
        > "${TLS_CONF}"
    printf '%s\n' "hub.${TLS_DOMAIN} {" '    reverse_proxy localhost:8080' \
        "    tls /etc/letsencrypt/live/${TLS_DOMAIN}/fullchain.pem /etc/letsencrypt/live/${TLS_DOMAIN}/privkey.pem" \
        '}' > "${TLS_ROOT}/etc/caddy/Caddyfile"
    : > "${STUB_TLS_STATE}/unit-certbot.timer"
    : > "${STUB_TLS_STATE}/unit-caddy"
    : > "${STUB_TLS_STATE}/active-caddy"
    if [[ "$2" == "true" ]]; then
        cp "${arch}/fullchain1.pem" "${STUB_TLS_STATE}/served.pem"
    else
        cp "${arch}/fullchain2.pem" "${STUB_TLS_STATE}/served.pem"
    fi
}

# tls_make_readable -- the permissions a working hub has.
tls_make_readable() {
    chgrp -R "${TLS_CADDY_GROUP}" "${TLS_LE}/live" "${TLS_LE}/archive"
    chmod 0750 "${TLS_LE}/live" "${TLS_LE}/archive"
    chmod 0640 "${TLS_LE}/archive/${TLS_DOMAIN}/"privkey*.pem
}

# run_fix [ARGS...] -- sets OUT, RC, CALLS. Extra environment via EXTRA_ENV.
run_fix() {
    OUT="$(env PATH="${TESTS_DIR}/lib-tls:${PATH}" STUB_REAL_OPENSSL="$(command -v openssl)" \
        FIX_TLS_ROOT="${TLS_ROOT}" STUB_CADDY_USER="${TLS_CADDY_USER}" \
        STUB_CADDY_GROUP="${TLS_CADDY_GROUP}" \
        STUB_TLS_LIVE="${TLS_LE}/live/${TLS_DOMAIN}/fullchain.pem" \
        "${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"}" \
        bash "${STARTER_DIR}/fix-tls-rotation.sh" --root "${TLS_ROOT}" "$@" 2>&1)"
    RC=$?
    CALLS="$(cat "${STUB_LOG}")"
}

# tls_snapshot -- every path, mode, group, link target and content hash
# under the fake root, plus the served certificate.
tls_snapshot() {
    (cd "${TLS_ROOT}" && find . -printf '%p %m %g %l\n' | sort
     find . -type f -exec sha256sum {} + | sort
     sha256sum "${STUB_TLS_STATE}/served.pem" 2>/dev/null; ls "${STUB_TLS_STATE}")
}

# mutating_calls -- stub calls that would change the VM.
mutating_calls() {
    grep -E '^(systemctl (enable|reload|restart)|certbot renew|caddy reload --config)' "${STUB_LOG}"
}

# tls_perms -- mode and group of everything under live/ and archive/.
tls_perms() {
    (cd "${TLS_LE}" && find live archive -printf '%p %m %g\n' | sort)
}

tls_serial() {
    openssl x509 -noout -serial -in "$1" | sed 's/^serial=//'
}

test_tls_fix_check_reports_problems_read_only() {
    EXTRA_ENV=()
    tls_fake_root 60 true
    local before
    before="$(tls_snapshot)"
    run_fix --check
    assert_eq 1 "$RC" "--check exits 1 on a broken hub"
    assert_contains "$OUT" "on disk: serial $(tls_serial "${TLS_LE}/archive/${TLS_DOMAIN}/cert2.pem")" "reports the disk serial"
    assert_contains "$OUT" "notAfter" "reports notAfter"
    assert_contains "$OUT" "PROBLEM: the scion deploy hook is not installed" "reports the missing hook"
    assert_contains "$OUT" "certbot.timer: disabled" "reports the timer state"
    assert_contains "$OUT" "broken inline renew_hook present" "reports the broken inline hook"
    assert_contains "$OUT" "Caddy runs as user ${TLS_CADDY_USER}" "finds the Caddy user"
    assert_contains "$OUT" "user ${TLS_CADDY_USER} cannot traverse /etc/letsencrypt/live (group" "names the blocking directory"
    assert_contains "$OUT" "user ${TLS_CADDY_USER} cannot read /etc/letsencrypt/live/${TLS_DOMAIN}/fullchain.pem /etc/letsencrypt/live/${TLS_DOMAIN}/privkey.pem" "names the files it cannot read"
    assert_contains "$OUT" "Caddy serves an older certificate" "reports the stale served certificate"
    assert_eq "$before" "$(tls_snapshot)" "--check changes no file"
    assert_eq "" "$(mutating_calls)" "--check makes no mutating call"
}

test_tls_fix_dry_run_changes_nothing() {
    EXTRA_ENV=()
    tls_fake_root 60 true
    local before
    before="$(tls_snapshot)"
    run_fix --dry-run
    assert_eq 0 "$RC" "--dry-run exits 0"
    assert_contains "$OUT" "[dry-run] would install the deploy hook" "plans the hook"
    assert_contains "$OUT" "[dry-run] would remove it" "plans removing the inline hook"
    assert_contains "$OUT" "[dry-run] would enable and start certbot.timer" "plans the timer"
    assert_contains "$OUT" "[dry-run] would set /etc/letsencrypt/live: group " "plans the live/ fix with old and new group"
    assert_contains "$OUT" ", mode 700 -> 710" "prints old and new mode"
    assert_contains "$OUT" "privkey2.pem: group " "plans the key fix"
    assert_contains "$OUT" ", mode 600 -> 640" "key mode 600 -> 640"
    assert_contains "$OUT" "[dry-run] would reload Caddy" "plans the reload"
    assert_not_contains "$OUT" "would renew" "no renewal for a certificate with 60 days left"
    assert_contains "$OUT" "nothing was changed (dry run)" "says nothing changed"
    assert_eq "$before" "$(tls_snapshot)" "--dry-run changes no file"
    assert_eq "" "$(mutating_calls)" "--dry-run makes no mutating call"
}

test_tls_fix_repairs_stale_hub_and_is_idempotent() {
    EXTRA_ENV=()
    tls_fake_root 60 true
    local hook="${TLS_HOOKS}/scion-reload-caddy.sh"
    run_fix
    assert_eq 0 "$RC" "first run succeeds"
    if [[ -x "${hook}" ]]; then pass; else fail "deploy hook installed and executable"; fi
    assert_eq "" "$(grep '^renew_hook' "${TLS_CONF}")" "inline renew_hook removed"
    if [[ -f "${TLS_CONF}.bak-fix-tls-rotation" ]]; then pass; else fail "renewal config backed up"; fi
    if [[ -f "${STUB_TLS_STATE}/enabled-certbot.timer" ]]; then pass; else fail "timer enabled"; fi
    assert_eq "710" "$(stat -c %a "${TLS_LE}/live")" "live/ gets only g+x"
    assert_eq "${TLS_CADDY_GROUP}" "$(stat -c %G "${TLS_LE}/live")" "live/ gets Caddy's group"
    assert_eq "640" "$(stat -c %a "${TLS_LE}/archive/${TLS_DOMAIN}/privkey2.pem")" "the served key gets g+r"
    assert_eq "600" "$(stat -c %a "${TLS_LE}/archive/${TLS_DOMAIN}/privkey1.pem")" "an old key nobody serves is left alone"
    assert_contains "$OUT" "user ${TLS_CADDY_USER} can read the certificate and key" "verdict after the fix"
    assert_contains "$CALLS" "systemctl reload caddy" "Caddy reloaded"
    assert_not_contains "$CALLS" "restart" "nothing restarted"
    assert_not_contains "$CALLS" "certbot renew" "no renewal needed"
    assert_eq "$(tls_serial "${TLS_LE}/live/${TLS_DOMAIN}/fullchain.pem")" \
        "$(tls_serial "${STUB_TLS_STATE}/served.pem")" "Caddy now serves the disk certificate"
    assert_contains "$OUT" "Result: OK, nothing to fix." "final check passes"

    local before
    before="$(tls_snapshot)"
    : > "${STUB_LOG}"
    run_fix
    assert_eq 0 "$RC" "second run succeeds"
    assert_contains "$OUT" "Result: nothing to change." "second run changes nothing"
    assert_eq "$before" "$(tls_snapshot)" "second run leaves every file alone"
    assert_eq "" "$(mutating_calls)" "second run makes no mutating call"

    run_fix --check
    assert_eq 0 "$RC" "--check passes after the fix"
}

test_tls_fix_renews_near_expiry_and_hook_reloads() {
    EXTRA_ENV=()
    tls_fake_root 10 false
    local old
    old="$(tls_serial "${STUB_TLS_STATE}/served.pem")"
    run_fix
    assert_eq 0 "$RC" "run succeeds"
    assert_contains "$CALLS" "certbot renew --cert-name ${TLS_DOMAIN}" "renews a certificate with 10 days left"
    assert_not_contains "$CALLS" "--force-renewal" "never forces a renewal"
    assert_contains "$OUT" "scion-reload-caddy: reloaded Caddy (systemctl reload caddy)" "the deploy hook ran and reloaded Caddy"
    assert_ne "$old" "$(tls_serial "${STUB_TLS_STATE}/served.pem")" "Caddy serves a new serial"
    assert_eq "$(tls_serial "${TLS_LE}/live/${TLS_DOMAIN}/fullchain.pem")" \
        "$(tls_serial "${STUB_TLS_STATE}/served.pem")" "served serial matches the renewed certificate"
    if [[ -n "$(find "${TLS_LE}/archive/${TLS_DOMAIN}/privkey3.pem" -perm -g=r)" ]]; then pass
    else fail "the renewed key is group-readable"; fi
    assert_eq "600" "$(stat -c %a "${TLS_LE}/archive/${TLS_DOMAIN}/privkey1.pem")" "the hook leaves old keys alone"

    : > "${STUB_LOG}"
    run_fix
    assert_contains "$OUT" "Result: nothing to change." "second run changes nothing"
    assert_eq "" "$(mutating_calls)" "second run makes no mutating call"
}

test_tls_fix_existing_reload_hook_is_kept() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    tls_make_readable
    tls_hook reload-caddy.sh 'systemctl reload caddy'
    local note="note:    existing hook reload-caddy.sh also appears to reload Caddy; Caddy will be reloaded twice per renewal, which is harmless; you may move it aside: sudo mv /etc/letsencrypt/renewal-hooks/deploy/reload-caddy.sh /root/reload-caddy.sh.bak"

    local before
    before="$(tls_snapshot)"
    run_fix --check
    assert_eq 1 "$RC" "--check fails while our hook is missing"
    assert_contains "$OUT" "${note}" "--check notes the existing hook"
    assert_contains "$OUT" "PROBLEM: the scion deploy hook is not installed" "our missing hook is always a problem"
    assert_eq "$before" "$(tls_snapshot)" "--check changes no file"
    assert_eq "" "$(mutating_calls)" "--check makes no mutating call"

    run_fix --dry-run
    assert_contains "$OUT" "[dry-run] would install the deploy hook" "--dry-run plans our hook anyway"
    assert_eq "$before" "$(tls_snapshot)" "--dry-run changes no file"
    assert_eq "" "$(mutating_calls)" "--dry-run makes no mutating call"

    run_fix
    assert_eq 0 "$RC" "run succeeds"
    if [[ -x "${TLS_HOOKS}/scion-reload-caddy.sh" ]]; then pass; else fail "our hook is installed anyway"; fi
    if [[ -x "${TLS_HOOKS}/reload-caddy.sh" ]]; then pass; else fail "existing hook left in place"; fi
    assert_contains "$OUT" "${note}" "the run notes the existing hook"

    # Both hooks present: a plain run is a no-op and --check passes.
    before="$(tls_snapshot)"
    : > "${STUB_LOG}"
    run_fix
    assert_eq 0 "$RC" "second run succeeds"
    assert_contains "$OUT" "Result: nothing to change." "second run changes nothing"
    assert_eq "$before" "$(tls_snapshot)" "second run leaves every file alone"
    assert_eq "" "$(mutating_calls)" "second run makes no mutating call"
    run_fix --check
    assert_eq 0 "$RC" "--check passes with both hooks present"
    assert_contains "$OUT" "${note}" "--check still notes their hook"

    run_fix --replace-hook
    assert_eq 2 "$RC" "--replace-hook is gone (always the default now)"
}

test_tls_fix_inline_hook_content_is_not_printed() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    tls_make_readable
    printf '%s\n' '[renewalparams]' 'authenticator = dns-google' \
        'renew_hook = curl -fsS -H "Authorization: Bearer TOPSECRET-TOKEN-123" https://hooks.example.invalid/x' \
        > "${TLS_CONF}"
    local mode
    for mode in --check --dry-run ""; do
        run_fix ${mode:+"$mode"}
        assert_not_contains "$OUT" "TOPSECRET" "${mode:-apply}: inline hook content not shown"
        assert_contains "$OUT" "an inline renew_hook is present (content not shown); left alone" "${mode:-apply}: names it without content"
        assert_not_contains "$OUT" "inline renew_hook also appears to reload" "${mode:-apply}: not taken for a reload"
    done
    assert_contains "$(cat "${TLS_CONF}")" "TOPSECRET-TOKEN-123" "the unrelated inline hook is left alone"
}

test_tls_fix_readable_hub_keeps_permissions() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    tls_make_readable
    local before
    before="$(tls_perms)"
    run_fix
    assert_eq 0 "$RC" "run succeeds"
    assert_contains "$OUT" "user ${TLS_CADDY_USER} can read the certificate and key" "reports the key is readable"
    assert_not_contains "$OUT" "cannot traverse" "no blocked directory"
    assert_not_contains "$OUT" "change:  set " "no permission change"
    assert_eq "$before" "$(tls_perms)" "modes and groups under live/ and archive/ unchanged"
}

test_tls_fix_removes_quoted_inline_hook() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    tls_make_readable
    # shellcheck disable=SC2016 # literal text as configobj may store it
    printf '%s\n' '[renewalparams]' 'authenticator = dns-google' \
        'renew_hook = "chown -R root:caddy /etc/letsencrypt/live/${RENEWED_DOMAINS%%,*} && (systemctl reload caddy)"' \
        > "${TLS_CONF}"
    run_fix --check
    assert_contains "$OUT" "broken inline renew_hook present" "quoted variant detected"
    run_fix
    assert_eq "" "$(grep '^renew_hook' "${TLS_CONF}")" "quoted variant removed"
    assert_contains "$(cat "${TLS_CONF}")" "authenticator = dns-google" "the rest of the config is kept"
}

test_tls_fix_reload_without_force_unit() {
    EXTRA_ENV=()
    tls_fake_root 60 true
    tls_make_readable
    : > "${STUB_TLS_STATE}/execreload-no-force"
    run_fix --check
    assert_contains "$OUT" "caddy.service reloads without --force" "--check warns about the unit"
    : > "${STUB_LOG}"
    run_fix
    assert_contains "$CALLS" "caddy reload --config /etc/caddy/Caddyfile --force" "falls back to caddy reload --force"
    assert_not_contains "$CALLS" "systemctl reload caddy" "does not use the unforced systemctl reload"
    assert_contains "$OUT" "reloaded Caddy (caddy reload --force)" "says which reload ran"

    tls_fake_root 60 true
    tls_make_readable
    : > "${STUB_TLS_STATE}/execreload-no-force"
    : > "${STUB_TLS_STATE}/caddy-no-force"
    run_fix
    assert_contains "$CALLS" "systemctl reload caddy" "without caddy reload --force, uses systemctl reload"
    assert_not_contains "$CALLS" "caddy reload --config" "does not call an unsupported --force"
    assert_contains "$OUT" "WARNING: caddy.service reloads without --force" "and warns"
}

test_tls_fix_renew_days_and_not_due() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    run_fix --check --renew-days 45
    assert_eq 2 "$RC" "--renew-days above 30 is rejected"
    assert_contains "$OUT" "from 1 to 30" "explains the limit"
    local d
    for d in 0 08 010 31 abc; do
        run_fix --check --renew-days "$d"
        assert_eq 2 "$RC" "--renew-days ${d} is rejected"
    done
    run_fix --check --renew-days 9
    assert_ne 2 "$RC" "--renew-days 9 is accepted"

    tls_fake_root 20 false
    tls_make_readable
    EXTRA_ENV=(STUB_CERTBOT_WINDOW_DAYS=10)
    run_fix
    assert_contains "$CALLS" "certbot renew --cert-name ${TLS_DOMAIN}" "asks certbot to renew"
    assert_contains "$OUT" "certbot did not renew it: not yet due" "reports that certbot did not renew"
    assert_eq 1 "$RC" "exit 1: the certificate still expires within the window"
    EXTRA_ENV=()
}

test_tls_fix_served_unreachable_is_a_problem() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    tls_make_readable
    rm -f "${STUB_TLS_STATE}/served.pem"
    run_fix --check
    assert_eq 1 "$RC" "--check fails"
    assert_contains "$OUT" "PROBLEM: could not fetch the served certificate" "unreachable Caddy is a problem"

    rm -f "${STUB_TLS_STATE}/unit-caddy" "${TLS_ROOT}/etc/caddy/Caddyfile"
    run_fix --check --domain "${TLS_DOMAIN}"
    assert_contains "$OUT" "WARN:    could not fetch the served certificate" "without Caddy set up it is a warning"
    assert_not_contains "$OUT" "PROBLEM: could not fetch" "and not a problem"
}

test_tls_fix_unreadable_live_says_sudo() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    chmod 0000 "${TLS_LE}/live"
    run_fix --check
    chmod 0700 "${TLS_LE}/live"
    assert_eq 1 "$RC" "--check fails"
    assert_contains "$OUT" "cannot read /etc/letsencrypt/live (run with sudo)" "says to run with sudo"
    assert_not_contains "$OUT" "does not exist" "does not claim the file is missing"
}

test_tls_fix_help_hides_test_root() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    run_fix --help
    assert_not_contains "$OUT" "--replace-hook" "--replace-hook is gone"
    assert_not_contains "$OUT" "--root" "--root is not in the usage"
    assert_not_contains "$OUT" "FIX_TLS_ROOT" "FIX_TLS_ROOT is not in the usage"
}

# tls_hook FILE LINE... -- an executable deploy hook with the given lines.
tls_hook() {
    local f="${TLS_HOOKS}/$1"
    shift
    printf '%s\n' '#!/bin/bash' "$@" > "$f"
    chmod 0755 "$f"
}

test_tls_fix_commented_or_echoed_reload_is_not_a_hook() {
    local body
    for body in '# systemctl reload caddy (disabled)' 'true  # systemctl reload caddy' \
        'echo "run: systemctl reload caddy"' 'echo systemctl reload caddy' \
        "printf '%s\\n' 'caddy reload --force'"; do
        EXTRA_ENV=()
        tls_fake_root 60 false
        tls_make_readable
        tls_hook reload-caddy.sh "$body"
        run_fix --check
        assert_eq 1 "$RC" "[${body}] --check fails"
        assert_contains "$OUT" "PROBLEM: the scion deploy hook is not installed" "[${body}] our missing hook is a problem"
        assert_contains "$OUT" "deploy hook reload-caddy.sh does not appear to reload Caddy" "[${body}] reported as not reloading"
        assert_not_contains "$OUT" "also appears to reload Caddy" "[${body}] no double-reload note"
        run_fix
        if [[ -x "${TLS_HOOKS}/scion-reload-caddy.sh" ]]; then pass; else fail "[${body}] our hook is installed"; fi
    done
}

test_tls_fix_caddy_reload_force_only_in_its_segment() {
    local body
    # --force (or -f) of another command does not make `caddy reload` forced.
    for body in 'caddy reload --config /etc/caddy/Caddyfile; rm --force /tmp/stale' \
        'caddy reload --config /etc/caddy/Caddyfile && cp -f /tmp/a /tmp/b' \
        'caddy reload --config /etc/caddy/Caddyfile && rm -f /tmp/x' \
        'caddy reload --config /etc/caddy/Caddyfile; tar -f x' \
        'caddy reload --config /etc/caddy/Caddyfile'; do
        EXTRA_ENV=()
        tls_fake_root 60 false
        tls_make_readable
        tls_hook reload-caddy.sh "$body"
        run_fix --check
        assert_contains "$OUT" "existing hook reload-caddy.sh runs 'caddy reload' without --force" "[${body}] reported as unforced"
        assert_not_contains "$OUT" "also appears to reload Caddy" "[${body}] not reported as a reload"
        assert_contains "$OUT" "PROBLEM: the scion deploy hook is not installed" "[${body}] our missing hook is a problem"
        run_fix
        if [[ -x "${TLS_HOOKS}/scion-reload-caddy.sh" ]]; then pass; else fail "[${body}] our hook is installed"; fi
    done

    tls_fake_root 60 false
    tls_make_readable
    tls_hook reload-caddy.sh 'caddy reload --config /etc/caddy/Caddyfile --force; logger done'
    run_fix --check
    assert_contains "$OUT" "existing hook reload-caddy.sh also appears to reload Caddy" "caddy reload --force is reported as a reload"
    assert_contains "$OUT" "PROBLEM: the scion deploy hook is not installed" "and our hook is still required"
}

test_tls_fix_symlinked_hook_is_detected() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    tls_make_readable
    mkdir -p "${TLS_ROOT}/usr/local/bin"
    printf '%s\n' '#!/bin/bash' 'systemctl reload caddy' > "${TLS_ROOT}/usr/local/bin/reload-caddy"
    chmod 0755 "${TLS_ROOT}/usr/local/bin/reload-caddy"
    ln -s ../../../../usr/local/bin/reload-caddy "${TLS_HOOKS}/reload-caddy"
    run_fix --check
    assert_contains "$OUT" "existing hook reload-caddy also appears to reload Caddy" "a symlinked hook is detected"
}

# tls_run_hook -- runs the installed hook as certbot would; sets OUT and RC.
tls_run_hook() {
    OUT="$(env PATH="${TESTS_DIR}/lib-tls:${PATH}" STUB_REAL_OPENSSL="$(command -v openssl)" \
        FIX_TLS_ROOT="${TLS_ROOT}" STUB_CADDY_USER="${TLS_CADDY_USER}" \
        STUB_CADDY_GROUP="${TLS_CADDY_GROUP}" \
        STUB_TLS_LIVE="${TLS_LE}/live/${TLS_DOMAIN}/fullchain.pem" \
        RENEWED_LINEAGE="${TLS_LE}/live/${TLS_DOMAIN}" "${TLS_HOOKS}/scion-reload-caddy.sh" 2>&1)"
    RC=$?
}

test_tls_hook_failed_reload_exits_non_zero() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    tls_make_readable
    run_fix

    # systemd branch: systemctl reload caddy fails.
    : > "${STUB_TLS_STATE}/reload-fails"
    tls_run_hook
    assert_ne 0 "$RC" "systemd: hook exits non-zero when the reload fails"
    assert_not_contains "$OUT" "reloaded Caddy" "systemd: does not claim a reload"
    assert_contains "$OUT" "scion-reload-caddy: ERROR: reload failed (systemctl reload caddy)" "systemd: prints the error"
    rm -f "${STUB_TLS_STATE}/reload-fails"

    # systemd branch, unit without --force: caddy reload --force fails.
    : > "${STUB_TLS_STATE}/execreload-no-force"
    : > "${STUB_TLS_STATE}/caddy-reload-fails"
    tls_run_hook
    assert_ne 0 "$RC" "systemd, no --force unit: hook exits non-zero"
    assert_not_contains "$OUT" "reloaded Caddy" "systemd, no --force unit: does not claim a reload"
    assert_contains "$OUT" "ERROR: reload failed (caddy reload --force)" "systemd, no --force unit: prints the error"

    # Not under systemd: caddy reload --force fails.
    rm -f "${STUB_TLS_STATE}/active-caddy" "${STUB_TLS_STATE}/unit-caddy"
    tls_run_hook
    assert_ne 0 "$RC" "no systemd: hook exits non-zero when the reload fails"
    assert_not_contains "$OUT" "reloaded Caddy" "no systemd: does not claim a reload"
    assert_not_contains "$OUT" "not running" "no systemd: does not fall through to 'not running'"
    assert_contains "$OUT" "ERROR: reload failed (caddy reload --config /etc/caddy/Caddyfile --force)" "no systemd: prints the error"

    # Not under systemd, and caddy has no reload --force.
    rm -f "${STUB_TLS_STATE}/caddy-reload-fails"
    : > "${STUB_TLS_STATE}/caddy-no-force"
    : > "${STUB_LOG}"
    tls_run_hook
    assert_ne 0 "$RC" "no systemd, no --force: hook exits non-zero"
    assert_not_contains "$OUT" "reloaded Caddy" "no systemd, no --force: does not claim a reload"
    assert_contains "$OUT" "ERROR: caddy has no 'reload --force'; cannot force a reload" "no systemd, no --force: says why"
    assert_not_contains "$(cat "${STUB_LOG}")" "caddy reload --config" "no systemd, no --force: does not run an unforced reload"
    rm -f "${STUB_TLS_STATE}/caddy-no-force"

    # Not under systemd and the reload works.
    tls_run_hook
    assert_eq 0 "$RC" "no systemd: hook exits 0 when the reload works"
    assert_contains "$OUT" "reloaded Caddy (not under systemd)" "no systemd: says it reloaded"
}

test_tls_fix_missing_timer_is_a_problem() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    tls_make_readable
    rm -f "${STUB_TLS_STATE}/unit-certbot.timer"
    run_fix --check
    assert_eq 1 "$RC" "--check fails without a renewal timer"
    assert_contains "$OUT" "PROBLEM: no certbot.timer or snap.certbot.renew.timer" "names the missing timer"

    # A run that has nothing to change but a problem remains.
    : > "${STUB_TLS_STATE}/unit-certbot.timer"
    run_fix
    rm -f "${STUB_TLS_STATE}/unit-certbot.timer"
    run_fix
    assert_eq 1 "$RC" "apply exits 1 with a problem and no changes"
    assert_contains "$OUT" "Result: nothing changed; 1 problem(s) remain." "result line for no changes and a problem"
}

test_tls_hook_unforceable_reload_exits_non_zero() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    tls_make_readable
    run_fix
    : > "${STUB_TLS_STATE}/execreload-no-force"
    : > "${STUB_TLS_STATE}/caddy-no-force"

    # Unit and caddy both without --force: the reload runs but is not forced.
    tls_run_hook
    assert_ne 0 "$RC" "unforceable reload: hook exits non-zero"
    assert_contains "$OUT" "WARNING: caddy.service reloads without --force" "unforceable reload: warns"

    # The same, and systemctl reload fails.
    : > "${STUB_TLS_STATE}/reload-fails"
    tls_run_hook
    assert_ne 0 "$RC" "unforceable and failing reload: hook exits non-zero"
    assert_not_contains "$OUT" "reloaded Caddy" "unforceable and failing reload: does not claim a reload"
    assert_contains "$OUT" "ERROR: reload failed (systemctl reload caddy)" "unforceable and failing reload: prints the error"
}

test_tls_fix_result_line_counts_problems() {
    EXTRA_ENV=()
    tls_fake_root 60 true
    tls_make_readable
    : > "${STUB_TLS_STATE}/reload-fails"
    run_fix
    assert_contains "$OUT" "change(s) applied; " "result line mentions remaining problems"
    assert_contains "$OUT" "problem(s) remain." "result line counts them"
}

test_tls_fix_failed_reload_is_a_problem() {
    EXTRA_ENV=()
    tls_fake_root 60 true
    tls_make_readable
    : > "${STUB_TLS_STATE}/reload-fails"
    run_fix
    assert_eq 1 "$RC" "the run exits 1 when the reload fails"
    assert_contains "$OUT" "PROBLEM: reloading Caddy failed" "reports the failed reload"
    assert_not_contains "$OUT" "reloaded Caddy (" "does not claim a reload"
}

test_tls_hook_reloads_even_when_permissions_fail() {
    EXTRA_ENV=()
    tls_fake_root 60 true
    run_fix
    local hook="${TLS_HOOKS}/scion-reload-caddy.sh" out rc
    cp "${TLS_LE}/archive/${TLS_DOMAIN}/fullchain1.pem" "${STUB_TLS_STATE}/served.pem"
    : > "${STUB_LOG}"
    out="$(env PATH="${TESTS_DIR}/lib-tls:${PATH}" STUB_REAL_OPENSSL="$(command -v openssl)" \
        FIX_TLS_ROOT="${TLS_ROOT}" STUB_CADDY_USER="${TLS_CADDY_USER}" \
        STUB_CADDY_GROUP="no-such-group-4207" \
        STUB_TLS_LIVE="${TLS_LE}/live/${TLS_DOMAIN}/fullchain.pem" \
        RENEWED_LINEAGE="${TLS_LE}/live/${TLS_DOMAIN}" "${hook}" 2>&1)"
    rc=$?
    assert_contains "$out" "WARNING: could not give group no-such-group-4207 access to" "warns about the failed chgrp"
    assert_contains "$out" "scion-reload-caddy: reloaded Caddy (systemctl reload caddy)" "still reloads Caddy"
    assert_contains "$(cat "${STUB_LOG}")" "systemctl reload caddy" "the reload ran"
    assert_eq 0 "$rc" "the hook exits 0"
    assert_eq "$(tls_serial "${TLS_LE}/live/${TLS_DOMAIN}/fullchain.pem")" \
        "$(tls_serial "${STUB_TLS_STATE}/served.pem")" "Caddy serves the renewed certificate"
}

test_tls_fix_acl_readable_skips_permission_changes() {
    # The mode bits say Caddy is blocked, but runuser (as with an ACL) says
    # it can read: nothing is changed.
    EXTRA_ENV=(STUB_RUNUSER_ALLOW=1)
    tls_fake_root 60 false
    local before
    before="$(tls_perms)"
    run_fix
    assert_contains "$OUT" "user ${TLS_CADDY_USER} can read the certificate and key (tested as that user)" "verdict from runuser"
    assert_not_contains "$OUT" "cannot traverse" "no per-path findings"
    assert_eq "$before" "$(tls_perms)" "no chmod or chgrp"
    EXTRA_ENV=()
}

test_tls_fix_without_runuser_rechecks_after_fix() {
    EXTRA_ENV=()
    tls_fake_root 60 false
    local nobin="${STATE_DIR}/bin"
    mkdir -p "${nobin}"
    cp "${TESTS_DIR}/lib-tls/"* "${nobin}/"
    rm -f "${nobin}/runuser"
    if env PATH="${nobin}:/usr/bin:/bin" bash -c 'command -v runuser' >/dev/null 2>&1; then
        pass # runuser is in /usr/bin or /bin here; cannot hide it
        return
    fi
    OUT="$(env PATH="${nobin}:/usr/bin:/bin" STUB_REAL_OPENSSL="$(command -v openssl)" \
        FIX_TLS_ROOT="${TLS_ROOT}" STUB_CADDY_USER="${TLS_CADDY_USER}" \
        STUB_CADDY_GROUP="${TLS_CADDY_GROUP}" \
        STUB_TLS_LIVE="${TLS_LE}/live/${TLS_DOMAIN}/fullchain.pem" \
        bash "${STARTER_DIR}/fix-tls-rotation.sh" --root "${TLS_ROOT}" 2>&1)"
    RC=$?
    assert_contains "$OUT" "change:  set /etc/letsencrypt/live" "fixes the blocked paths"
    assert_contains "$OUT" "can read the certificate and key (runuser not found, so judged from owner, group and mode only)" "verdict recomputed after the fixes"
    assert_not_contains "$OUT" "run with sudo" "does not tell root to use sudo"
    assert_eq 0 "$RC" "run succeeds"
}

test_tls_fix_systemctl_without_value_flag() {
    # systemd older than 230 has no "systemctl show --value" and prints
    # "User=NAME"; the script and the hook must still find the Caddy user.
    EXTRA_ENV=(STUB_SYSTEMCTL_NO_VALUE=1)
    tls_fake_root 60 false
    run_fix --check
    assert_contains "$OUT" "found:   Caddy runs as user ${TLS_CADDY_USER} (group ${TLS_CADDY_GROUP})" "script: user without the User= prefix"
    assert_not_contains "$OUT" "User=" "script: no User= prefix anywhere"
    run_fix
    assert_eq 0 "$RC" "script: the fix succeeds"
    : > "${STUB_LOG}"
    OUT="$(env STUB_SYSTEMCTL_NO_VALUE=1 PATH="${TESTS_DIR}/lib-tls:${PATH}" \
        STUB_CADDY_USER="${TLS_CADDY_USER}" STUB_CADDY_GROUP="${TLS_CADDY_GROUP}" \
        STUB_TLS_LIVE="${TLS_LE}/live/${TLS_DOMAIN}/fullchain.pem" \
        RENEWED_LINEAGE="${TLS_LE}/live/${TLS_DOMAIN}" "${TLS_HOOKS}/scion-reload-caddy.sh" 2>&1)"
    RC=$?
    assert_eq 0 "$RC" "hook: exits 0"
    assert_not_contains "$OUT" "User=" "hook: no User= prefix"
    assert_not_contains "$OUT" "no user" "hook: finds the Caddy user"
    assert_not_contains "$OUT" "WARNING" "hook: permission steps succeed"
    assert_contains "$OUT" "reloaded Caddy" "hook: reloads Caddy"
    EXTRA_ENV=()
}

test_tls_fix_finds_the_certificate_name() {
    # Only real directories under live/ count: a README file and a symlink
    # to a directory are ignored. Several real ones: sorted, and an error.
    # No Caddyfile, so the name comes from live/.
    EXTRA_ENV=()
    tls_fake_root 60 false
    rm -f "${TLS_ROOT}/etc/caddy/Caddyfile"
    chmod 0755 "${TLS_LE}/live"
    : > "${TLS_LE}/live/README"
    ln -s "${TLS_DOMAIN}" "${TLS_LE}/live/alias.example"
    run_fix --check
    assert_contains "$OUT" "found:   certificate name ${TLS_DOMAIN}," "one real directory: its name"
    mkdir "${TLS_LE}/live/b.example" "${TLS_LE}/live/.a.example"
    run_fix --check
    assert_eq 1 "$RC" "several certificates: exits 1"
    assert_contains "$OUT" "several certificates under /etc/letsencrypt/live (.a.example b.example ${TLS_DOMAIN}); pass --domain." "lists them sorted, hidden included, symlink excluded"
}

test_tls_fix_stat_failure_is_reported() {
    # stat fails on a blocked path: reported as a problem, left alone, and
    # the script does not crash on the empty mode.
    tls_fake_root 60 false
    local bin="${STATE_DIR}/statbin" blockpath="${TLS_LE}/live"
    mkdir -p "${bin}"
    # shellcheck disable=SC2016 # the stub's own code, expanded when it runs
    printf '%s\n' '#!/usr/bin/env bash' \
        'for a in "$@"; do [ "$a" != "${STUB_STAT_FAIL}" ] || { echo "stat: cannot statx" >&2; exit 1; }; done' \
        "exec $(command -v stat) \"\$@\"" > "${bin}/stat"
    chmod +x "${bin}/stat"
    EXTRA_ENV=(PATH="${bin}:${TESTS_DIR}/lib-tls:${PATH}" STUB_STAT_FAIL="${blockpath}")
    local mode
    for mode in --check --dry-run ""; do
        run_fix ${mode:+"$mode"}
        assert_not_contains "$OUT" "syntax error" "${mode:-fix}: no arithmetic crash"
        assert_contains "$OUT" "PROBLEM: cannot stat /etc/letsencrypt/live, so cannot tell whether user ${TLS_CADDY_USER} can reach it; left alone" "${mode:-fix}: stat failure reported"
        assert_contains "$OUT" "cannot traverse /etc/letsencrypt/archive" "${mode:-fix}: other blocked paths still found"
        assert_contains "$OUT" "Result:" "${mode:-fix}: runs to the end"
        assert_ne 0 "$RC" "${mode:-fix}: exits non-zero"
    done
    assert_eq "700" "$(stat -c %a "${blockpath}")" "the path stat failed on is left alone"
    EXTRA_ENV=()
}

test_tls_certs_cleans_up_when_scp_fails() {
    fresh_state
    EXTRA_ENV=(CERT_EMAIL=admin@example.com STUB_SCP_FAIL=1)
    run_script gce-certs.sh
    assert_ne 0 "$RC" "gce-certs.sh fails when the copy fails"
    assert_contains "$(cat "${STUB_SSH_LOG}")" "rm -rf /tmp/fix-tls-rotation.stub" "removes the remote temp dir"
    assert_not_contains "$(cat "${STUB_SSH_LOG}")" "sudo bash /tmp/fix-tls-rotation.stub" "does not run the script"
    EXTRA_ENV=()
}

test_tls_certs_installs_rotation_on_existing_and_new_certs() {
    # The gcloud stub's "test -f fullchain.pem" succeeds, so this is the
    # existing-certificate path that #942 skipped.
    fresh_state
    EXTRA_ENV=(CERT_EMAIL=admin@example.com)
    run_script gce-certs.sh
    assert_eq 0 "$RC" "gce-certs.sh succeeds"
    if [[ -f "${STUB_SCP_DIR}/fix-tls-rotation.sh" ]]; then pass; else fail "fix-tls-rotation.sh copied to the VM"; fi
    local ssh
    ssh="$(cat "${STUB_SSH_LOG}")"
    assert_contains "$ssh" "mktemp -d /tmp/fix-tls-rotation.XXXXXX" "makes a private temp dir on the VM"
    assert_contains "$CALLS" ":/tmp/fix-tls-rotation.stub/fix-tls-rotation.sh" "copies into that dir"
    assert_contains "$ssh" "sudo bash /tmp/fix-tls-rotation.stub/fix-tls-rotation.sh --domain" "runs it on the VM"
    assert_contains "$ssh" "rm -rf /tmp/fix-tls-rotation.stub" "removes the dir"
    assert_not_contains "$ssh" "RENEWED_DOMAINS%%" "no inline deploy hook"
}

mapfile -t TESTS < <(declare -F | awk '{print $3}' | grep '^test_' | sort)
for t in "${TESTS[@]}"; do
    CURRENT="$t"
    echo "--- ${t}"
    "$t"
done

echo ""
echo "${PASS} passed, ${FAIL} failed"
[[ "${FAIL}" -eq 0 ]]
