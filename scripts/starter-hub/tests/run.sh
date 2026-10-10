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
# (stub systemctl, certbot, caddy, and an openssl that only fakes s_client)
# first on PATH and run against a fake root in a temp directory. Needs bash
# and openssl.
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
# stub systemctl, certbot, caddy and openssl from tests/lib-tls first on
# PATH. Certificates are throwaway self-signed ones made here.

TLS_DOMAIN="example.com"

# tls_cert DIR N DAYS -- writes archive version N, valid for DAYS days.
tls_cert() {
    openssl req -x509 -newkey rsa:2048 -nodes -days "$3" -subj "/CN=${TLS_DOMAIN}" \
        -keyout "$1/privkey$2.pem" -out "$1/cert$2.pem" 2>/dev/null
    cp "$1/cert$2.pem" "$1/fullchain$2.pem"
    cp "$1/cert$2.pem" "$1/chain$2.pem"
}

# tls_fake_root DAYS STALE -- a hub as an older gce-certs.sh left it: the
# live certificate is valid for DAYS days, the broken inline renew_hook is in
# the renewal config, no deploy hook, certbot.timer disabled, private keys
# not group-readable. STALE=true: Caddy still serves an older certificate.
tls_fake_root() {
    fresh_state
    TLS_ROOT="${STATE_DIR}/root"
    export STUB_TLS_STATE="${STATE_DIR}/tls"
    local le="${TLS_ROOT}/etc/letsencrypt" arch
    arch="${le}/archive/${TLS_DOMAIN}"
    mkdir -p "${arch}" "${le}/live/${TLS_DOMAIN}" "${le}/renewal" \
        "${le}/renewal-hooks/deploy" "${TLS_ROOT}/etc/caddy" "${STUB_TLS_STATE}"
    tls_cert "${arch}" 1 1
    tls_cert "${arch}" 2 "$1"
    for f in cert chain fullchain privkey; do
        ln -s "../../archive/${TLS_DOMAIN}/${f}2.pem" "${le}/live/${TLS_DOMAIN}/${f}.pem"
    done
    chmod 0700 "${le}/live" "${le}/archive"
    # shellcheck disable=SC2016 # literal text as certbot stores it
    printf '%s\n' '[renewalparams]' 'authenticator = dns-google' \
        'renew_hook = chown root:caddy /etc/letsencrypt/live /etc/letsencrypt/archive && chown -R root:caddy /etc/letsencrypt/live/${RENEWED_DOMAINS%%,*} && (systemctl reload caddy)' \
        > "${le}/renewal/${TLS_DOMAIN}.conf"
    printf '%s\n' "hub.${TLS_DOMAIN} {" '    reverse_proxy localhost:8080' \
        "    tls /etc/letsencrypt/live/${TLS_DOMAIN}/fullchain.pem /etc/letsencrypt/live/${TLS_DOMAIN}/privkey.pem" \
        '}' > "${TLS_ROOT}/etc/caddy/Caddyfile"
    : > "${STUB_TLS_STATE}/unit-certbot.timer"
    : > "${STUB_TLS_STATE}/active-caddy"
    if [[ "$2" == "true" ]]; then
        cp "${arch}/fullchain1.pem" "${STUB_TLS_STATE}/served.pem"
    else
        cp "${arch}/fullchain2.pem" "${STUB_TLS_STATE}/served.pem"
    fi
}

# run_fix [ARGS...] -- sets OUT, RC, CALLS.
run_fix() {
    OUT="$(env PATH="${TESTS_DIR}/lib-tls:${PATH}" STUB_REAL_OPENSSL="$(command -v openssl)" \
        FIX_TLS_ROOT="${TLS_ROOT}" CADDY_GROUP="$(id -gn)" \
        STUB_TLS_LIVE="${TLS_ROOT}/etc/letsencrypt/live/${TLS_DOMAIN}/fullchain.pem" \
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
    grep -E '^(systemctl (enable|reload|restart)|certbot renew|caddy reload)' "${STUB_LOG}"
}

tls_serial() {
    openssl x509 -noout -serial -in "$1" | sed 's/^serial=//'
}

test_tls_fix_check_reports_problems_read_only() {
    tls_fake_root 60 true
    local before
    before="$(tls_snapshot)"
    run_fix --check
    assert_eq 1 "$RC" "--check exits 1 on a broken hub"
    assert_contains "$OUT" "on disk: serial $(tls_serial "${TLS_ROOT}/etc/letsencrypt/archive/${TLS_DOMAIN}/cert2.pem")" "reports the disk serial"
    assert_contains "$OUT" "notAfter" "reports notAfter"
    assert_contains "$OUT" "renewal-hooks/deploy/scion-reload-caddy.sh is missing" "reports the missing hook"
    assert_contains "$OUT" "certbot.timer: disabled" "reports the timer state"
    assert_contains "$OUT" "broken inline renew_hook present" "reports the broken inline hook"
    assert_contains "$OUT" "Caddy serves an older certificate" "reports the stale served certificate"
    assert_eq "$before" "$(tls_snapshot)" "--check changes no file"
    assert_eq "" "$(mutating_calls)" "--check makes no mutating call"
}

test_tls_fix_dry_run_changes_nothing() {
    tls_fake_root 60 true
    local before
    before="$(tls_snapshot)"
    run_fix --dry-run
    assert_eq 0 "$RC" "--dry-run exits 0"
    assert_contains "$OUT" "[dry-run] would install the deploy hook" "plans the hook"
    assert_contains "$OUT" "[dry-run] would remove it" "plans removing the inline hook"
    assert_contains "$OUT" "[dry-run] would enable and start certbot.timer" "plans the timer"
    assert_contains "$OUT" "[dry-run] would give group" "plans the permission fix"
    assert_contains "$OUT" "[dry-run] would reload Caddy" "plans the reload"
    assert_not_contains "$OUT" "would renew" "no renewal for a certificate with 60 days left"
    assert_contains "$OUT" "nothing was changed (dry run)" "says nothing changed"
    assert_eq "$before" "$(tls_snapshot)" "--dry-run changes no file"
    assert_eq "" "$(mutating_calls)" "--dry-run makes no mutating call"
}

test_tls_fix_repairs_stale_hub_and_is_idempotent() {
    tls_fake_root 60 true
    local le="${TLS_ROOT}/etc/letsencrypt" hook
    hook="${le}/renewal-hooks/deploy/scion-reload-caddy.sh"
    run_fix
    assert_eq 0 "$RC" "first run succeeds"
    if [[ -x "${hook}" ]]; then pass; else fail "deploy hook installed and executable"; fi
    assert_eq "" "$(grep '^renew_hook' "${le}/renewal/${TLS_DOMAIN}.conf")" "inline renew_hook removed"
    if [[ -f "${le}/renewal/${TLS_DOMAIN}.conf.bak-fix-tls-rotation" ]]; then pass; else fail "renewal config backed up"; fi
    if [[ -f "${STUB_TLS_STATE}/enabled-certbot.timer" ]]; then pass; else fail "timer enabled"; fi
    assert_contains "$CALLS" "systemctl reload caddy" "Caddy reloaded"
    assert_not_contains "$CALLS" "restart" "nothing restarted"
    assert_not_contains "$CALLS" "certbot renew" "no renewal needed"
    assert_eq "$(tls_serial "${le}/live/${TLS_DOMAIN}/fullchain.pem")" \
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
    tls_fake_root 10 false
    local le="${TLS_ROOT}/etc/letsencrypt" old
    old="$(tls_serial "${STUB_TLS_STATE}/served.pem")"
    run_fix
    assert_eq 0 "$RC" "run succeeds"
    assert_contains "$CALLS" "certbot renew --cert-name ${TLS_DOMAIN}" "renews a certificate with 10 days left"
    assert_contains "$OUT" "scion-reload-caddy: reloaded Caddy for ${TLS_DOMAIN}" "the deploy hook ran and reloaded Caddy"
    assert_ne "$old" "$(tls_serial "${STUB_TLS_STATE}/served.pem")" "Caddy serves a new serial"
    assert_eq "$(tls_serial "${le}/live/${TLS_DOMAIN}/fullchain.pem")" \
        "$(tls_serial "${STUB_TLS_STATE}/served.pem")" "served serial matches the renewed certificate"
    if [[ -n "$(find "${le}/archive/${TLS_DOMAIN}/privkey3.pem" -perm -g=r)" ]]; then pass
    else fail "the hook made the renewed key group-readable"; fi

    : > "${STUB_LOG}"
    run_fix
    assert_contains "$OUT" "Result: nothing to change." "second run changes nothing"
    assert_eq "" "$(mutating_calls)" "second run makes no mutating call"
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
