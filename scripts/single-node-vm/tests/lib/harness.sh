# scripts/single-node-vm/tests/lib/harness.sh — shared helpers for the
# deploy.sh gcloud-stub test harness. Sourced by run.sh, never executed
# directly.
#
# Provides: stub info/warn/err/print_gcloud_error/config_get/config_prompt
# (stand-ins for deploy.sh's own helpers, which hybrid-tier.sh and any
# function-level test that sources a deploy.sh helper directly expect
# their caller to have already defined), a fresh-per-test gcloud and
# kubectl stub state dir on PATH, and small assertion helpers. No test
# here ever contacts GCP or a cluster; the only `gcloud` on PATH is
# tests/lib/gcloud and the only `kubectl` is tests/lib/kubectl.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

HARNESS_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PYTHON="${PYTHON:-python3}"

# A handful of tests deliberately replace (not prepend to) $PATH for the
# duration of a single run_expect_fail call, to simulate a missing tool
# (kubectl or the auth plugin -- see
# test_k8s_setup_kubeconfig_missing_*_fails). run_expect_fail's own TMPDIR
# bookkeeping (find/sort/comm, below) must keep working even then, so it
# uses this snapshot -- taken once, here, before any test can touch $PATH
# -- rather than the ambient $PATH.
HARNESS_SAFE_PATH="$PATH"

PASS_COUNT=0
FAIL_COUNT=0

# --- Stub versions of deploy.sh's own helpers, for function-level tests
# that source hybrid-tier.sh (written to use its caller's info/warn/err
# rather than define its own) or a deploy.sh helper directly, without
# driving deploy.sh as a subprocess. ---
info() { :; }
warn() { echo "WARN: $*" >&2; }
err()  { echo "ERR: $*" >&2; }
# Same as deploy.sh's: prints captured gcloud stderr, indented, to stderr.
print_gcloud_error() {
  if [[ -n "$1" ]]; then
    printf '%s\n' "$1" | sed 's/^/  /' >&2
  fi
}

# --- Config stub -------------------------------------------------------
# A function-level test populates the CONFIG associative array (declare
# -A CONFIG=(...)) before calling a helper function; config_get reads
# from it instead of parsing any real JSON file.
declare -A CONFIG
config_get() {
  local key="$1" default="${2:-}"
  if [[ -n "${CONFIG[$key]+set}" ]]; then
    printf '%s' "${CONFIG[$key]}"
  else
    printf '%s' "$default"
  fi
}
config_prompt() {
  # Not expected to be called in any test.
  # If a test hits this, its
  # CONFIG fixture is missing a key it should have set.
  err "config_prompt called unexpectedly with: $2"
  exit 1
}
# shellcheck disable=SC2034 # read by hybrid_read_config in hybrid-tier.sh
CONFIG_FILE="/dev/null/harness-tests-placeholder"

# --- gcloud stub setup ---------------------------------------------------
# fresh_gcloud_state resets CONFIG, the invocation logs, and the gcloud
# and kubectl stubs' on-disk fixture state dirs for the next test.
fresh_gcloud_state() {
  # Each call makes 4 new mktemp entries (a dir and a file per stub).
  # Removing the previous call's entries before making new ones keeps the
  # suite's own footprint bounded to "the current test's state" at any
  # point in time, not "every test's state so far". This still leaves the
  # very last test's entries in place when the run ends, which run.sh's
  # own per-run TMPDIR + EXIT trap is what actually cleans up.
  rm -rf "${GCLOUD_STUB_STATE_DIR:-}" "${KUBECTL_STUB_STATE_DIR:-}"
  rm -f "${GCLOUD_STUB_LOG:-}" "${KUBECTL_STUB_LOG:-}"
  CONFIG=()
  GCLOUD_STUB_STATE_DIR="$(mktemp -d)"
  GCLOUD_STUB_LOG="$(mktemp)"
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/firewall-rules" "${GCLOUD_STUB_STATE_DIR}/clusters" \
    "${GCLOUD_STUB_STATE_DIR}/gke-node-firewall-rules" \
    "${GCLOUD_STUB_STATE_DIR}/instances" "${GCLOUD_STUB_STATE_DIR}/subnets" "${GCLOUD_STUB_STATE_DIR}/run-services" \
    "${GCLOUD_STUB_STATE_DIR}/addresses" "${GCLOUD_STUB_STATE_DIR}/nats" \
    "${GCLOUD_STUB_STATE_DIR}/networks" "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  export GCLOUD_STUB_STATE_DIR GCLOUD_STUB_LOG
  KUBECTL_STUB_STATE_DIR="$(mktemp -d)"
  KUBECTL_STUB_LOG="$(mktemp)"
  mkdir -p "${KUBECTL_STUB_STATE_DIR}/pv" "${KUBECTL_STUB_STATE_DIR}/pvc" "${KUBECTL_STUB_STATE_DIR}/namespace"
  export KUBECTL_STUB_STATE_DIR KUBECTL_STUB_LOG
}

kubectl_log() {
  [[ -f "$KUBECTL_STUB_LOG" ]] && cat "$KUBECTL_STUB_LOG"
}

# seed_k8s_pv NAME HUB_NAME SERVER PATH NAMESPACE PVC_NAME [RECLAIM] —
# simulates a pre-existing, marked PV with the given identity fields.
seed_k8s_pv() {
  local name="$1" hub="$2" server="$3" path="$4" ns="$5" pvc="$6" reclaim="${7:-Retain}"
  "$PYTHON" -c "
import json, sys
name, hub, server, path, ns, pvc, reclaim = sys.argv[1:8]
print(json.dumps({
    'kind': 'PersistentVolume',
    'metadata': {'name': name, 'labels': {'scion-deployment': hub}},
    'spec': {
        'nfs': {'server': server, 'path': path},
        'claimRef': {'namespace': ns, 'name': pvc},
        'persistentVolumeReclaimPolicy': reclaim,
    },
}))
" "$name" "$hub" "$server" "$path" "$ns" "$pvc" "$reclaim" > "${KUBECTL_STUB_STATE_DIR}/pv/${name}.json"
}

# seed_k8s_pv_unmarked NAME — a pre-existing PV with the target name and
# no marker label at all, for the marker-refusal tests.
seed_k8s_pv_unmarked() {
  printf '{"kind": "PersistentVolume", "metadata": {"name": "%s", "labels": {}}, "spec": {}}' "$1" \
    > "${KUBECTL_STUB_STATE_DIR}/pv/$1.json"
}

# seed_k8s_pvc NAME NAMESPACE HUB_NAME VOLUME_NAME — a pre-existing,
# marked PVC bound to VOLUME_NAME.
seed_k8s_pvc() {
  local name="$1" ns="$2" hub="$3" volume_name="$4"
  "$PYTHON" -c "
import json, sys
name, ns, hub, volume_name = sys.argv[1:5]
print(json.dumps({
    'kind': 'PersistentVolumeClaim',
    'metadata': {'name': name, 'namespace': ns, 'labels': {'scion-deployment': hub}},
    'spec': {'volumeName': volume_name},
}))
" "$name" "$ns" "$hub" "$volume_name" > "${KUBECTL_STUB_STATE_DIR}/pvc/${ns}__${name}.json"
}

# seed_k8s_pvc_unmarked NAME NAMESPACE — a pre-existing PVC with the
# target name/namespace and no marker label, for the marker-refusal tests.
seed_k8s_pvc_unmarked() {
  local name="$1" ns="$2"
  printf '{"kind": "PersistentVolumeClaim", "metadata": {"name": "%s", "namespace": "%s", "labels": {}}, "spec": {}}' \
    "$name" "$ns" > "${KUBECTL_STUB_STATE_DIR}/pvc/${ns}__${name}.json"
}

# seed_k8s_namespace NAME HUB_NAME — a pre-existing, marked namespace.
seed_k8s_namespace() {
  local name="$1" hub="$2"
  printf '{"kind": "Namespace", "metadata": {"name": "%s", "labels": {"scion-deployment": "%s"}}, "spec": {}}' \
    "$name" "$hub" > "${KUBECTL_STUB_STATE_DIR}/namespace/${name}.json"
}

# seed_k8s_namespace_unmarked NAME — a pre-existing namespace with no
# marker label, for the "used but not adopted" case.
seed_k8s_namespace_unmarked() {
  printf '{"kind": "Namespace", "metadata": {"name": "%s", "labels": {}}, "spec": {}}' "$1" \
    > "${KUBECTL_STUB_STATE_DIR}/namespace/$1.json"
}

# set_k8s_get_error KIND NAME — every `kubectl get KIND NAME` call
# fails with an error that isn't a "not found" (a permissions or
# connectivity problem, say), so "unknown, not gone/absent" handling can
# be tested directly. KIND is pv, pvc, or namespace; for pvc, NAME must
# be "namespace__pvcname" to match the stub's own key.
set_k8s_get_error() {
  touch "${KUBECTL_STUB_STATE_DIR}/$1/$2.json.get-error"
}

# set_k8s_get_permission_masked KIND NAME — like set_k8s_get_error, but
# with a realistic permission-denied message that also happens to
# contain the words "not found" (some APIs word it that way deliberately
# to avoid confirming a resource's existence to an unauthorized caller).
# Must NOT be treated as "gone".
set_k8s_get_permission_masked() {
  printf 'Error from server: %s "%s" not found or permission denied' "$1" "$2" \
    > "${KUBECTL_STUB_STATE_DIR}/$1/$2.json.get-error"
}

# set_k8s_delete_will_fail KIND NAME — every `kubectl delete KIND
# NAME` call fails instead of succeeding.
set_k8s_delete_will_fail() {
  touch "${KUBECTL_STUB_STATE_DIR}/$1/$2.json.delete-fail"
}

# seed_k8s_pod_using_pvc NAMESPACE POD_NAME PVC_NAME — a pod in NAMESPACE
# whose spec mounts PVC_NAME, for the teardown pod preflight ("stop
# agents first") to find.
seed_k8s_pod_using_pvc() {
  local namespace="$1" pod="$2" pvc="$3"
  mkdir -p "${KUBECTL_STUB_STATE_DIR}/pods"
  printf '{"items": [{"metadata": {"name": "%s"}, "spec": {"volumes": [{"name": "v", "persistentVolumeClaim": {"claimName": "%s"}}]}}]}' \
    "$pod" "$pvc" > "${KUBECTL_STUB_STATE_DIR}/pods/${namespace}.json"
}

# set_k8s_pods_get_error NAMESPACE — every `kubectl get pods -n
# NAMESPACE` call fails, for the "could not check, unknown is never
# treated as gone" path.
set_k8s_pods_get_error() {
  mkdir -p "${KUBECTL_STUB_STATE_DIR}/pods"
  touch "${KUBECTL_STUB_STATE_DIR}/pods/$1.json.get-error"
}

# set_cluster_describe_error NAME — every `container clusters
# describe` call for this cluster fails with an error that isn't
# NOT_FOUND (distinct from the cluster simply never having been seeded,
# which the stub already reports as NOT_FOUND).
set_cluster_describe_error() {
  touch "${GCLOUD_STUB_STATE_DIR}/clusters/$1.json.describe-error"
}

# set_cluster_describe_permission_masked NAME — like
# set_cluster_describe_error, but with a realistic permission-denied
# message that also happens to contain the words "not found". Must NOT
# be treated as "gone".
set_cluster_describe_permission_masked() {
  printf 'gcloud-stub: PERMISSION_DENIED: Cluster %s not found or permission denied' "$1" \
    > "${GCLOUD_STUB_STATE_DIR}/clusters/$1.json.describe-error"
}

# set_get_credentials_will_fail — every `container clusters
# get-credentials` call fails.
set_get_credentials_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/get-credentials-should-fail"
}

# seed_instance NAME ZONE — simulates a pre-existing GCE VM.
seed_instance() {
  printf '%s' "$2" > "${GCLOUD_STUB_STATE_DIR}/instances/$1"
}

# set_instance_delete_will_fail NAME — the next (and every subsequent)
# `instances delete` call for this instance fails instead of succeeding
# (the instance stays "present" in the stub's state, matching a real
# failed delete).
set_instance_delete_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/instances/$1.delete-fail"
}

# set_instance_delete_error_text NAME TEXT — used with
# set_instance_delete_will_fail above: replaces the stub's generic
# failure message with TEXT, so a test can distinguish a confirmed
# not-found error from an ambiguous one (permission, API outage, etc.).
set_instance_delete_error_text() {
  printf '%s' "$2" > "${GCLOUD_STUB_STATE_DIR}/instances/$1.delete-fail-text"
}

# set_instance_internal_ip NAME IP — overrides the internal IP `compute
# instances describe --format=get(networkInterfaces[0].networkIP)`
# reports for this instance. Without this, the stub returns a realistic
# default ("10.128.0.5").
set_instance_internal_ip() {
  printf '%s' "$2" > "${GCLOUD_STUB_STATE_DIR}/instances/$1.internal-ip"
}

# set_instance_add_tags_will_fail NAME — the next (and every subsequent)
# `instances add-tags` call for this instance fails instead of succeeding
# (e.g. a permissions gap), so a caller's "confirmed tagged" logic has
# something real to be gated on.
set_instance_add_tags_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/instances/$1.add-tags-fail"
}

# set_firewall_list_will_fail — the next `firewall-rules list` call fails
# instead of returning a rule list.
set_firewall_list_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/firewall-rules-list-should-fail"
}

# set_gke_node_tag_firewall_list_empty_stdout — the unfiltered
# `firewall-rules list` call node-tag discovery makes succeeds but prints
# nothing at all, rather than an empty JSON array.
set_gke_node_tag_firewall_list_empty_stdout() {
  touch "${GCLOUD_STUB_STATE_DIR}/firewall-rules-list-empty-stdout"
}

# set_instances_list_will_fail — every `instances list` call fails
# (simulating a transient API error), so a VM-gone check can't tell
# whether the VM is still there.
set_instances_list_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/instances-list-should-fail"
}

# set_instances_list_zone_unreachable — every `instances list` call
# simulates a real AggregatedList with one UNREACHABLE zone: exit 0 with
# the VM missing from stdout and a warning on stderr, unless the caller
# set CLOUDSDK_COMPUTE_ALLOW_PARTIAL_ERROR=false, in which case it's a
# hard error (exit 1) instead -- matching real gcloud's own behavior.
set_instances_list_zone_unreachable() {
  touch "${GCLOUD_STUB_STATE_DIR}/instances-list-zone-unreachable"
}

# seed_enabled_apis API... — overrides the stub's default enabled-API
# set (see tests/lib/gcloud) for `services list --enabled` with an
# explicit set, so a test can make some (or all) required APIs come back
# as missing.
seed_enabled_apis() {
  printf '%s\n' "$@" > "${GCLOUD_STUB_STATE_DIR}/enabled-apis.txt"
}

# set_services_list_will_fail — every `services list` call fails
# (simulating a runner without serviceusage.services.list), so the API
# check can't tell what's already enabled.
set_services_list_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/services-list-should-fail"
}

# set_router_exists / set_service_account_exists — simulate a
# pre-existing base router or service account, so its create-only marker
# can be asserted absent on a re-run/adopt path.
set_router_exists() {
  touch "${GCLOUD_STUB_STATE_DIR}/router-exists"
}
set_service_account_exists() {
  touch "${GCLOUD_STUB_STATE_DIR}/service-account-exists"
}

# set_nat_exists NAME — simulates a pre-existing Cloud NAT with this name
# (name-scoped, unlike set_router_exists above, since a project can have
# more than one NAT), so its create-only marker can be asserted absent on
# a re-run/adopt path. `compute routers nats create` for this name sets
# the same marker, so a re-run against a create's own leftover state sees
# it without a test needing to hand-seed it too.
set_nat_exists() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/nats"
  touch "${GCLOUD_STUB_STATE_DIR}/nats/$1.exists"
}

# seed_service_account EMAIL DESCRIPTION — a name-scoped service-account
# fixture: the next `describe` for exactly this email returns this
# description.
seed_service_account() {
  local email="$1" description="${2:-}"
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  "$PYTHON" -c "
import json, sys
email, desc, path = sys.argv[1:4]
json.dump({'email': email, 'description': desc}, open(path, 'w', encoding='utf-8'))
" "$email" "$description" "${GCLOUD_STUB_STATE_DIR}/service-accounts/${email}.json"
}

# set_service_account_delete_will_fail EMAIL — the next `delete` for
# exactly this service account fails.
set_service_account_delete_will_fail() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  touch "${GCLOUD_STUB_STATE_DIR}/service-accounts/$1.json.delete-fail"
}

# set_service_account_delete_error_text EMAIL TEXT — used with
# set_service_account_delete_will_fail above: the failed delete prints
# TEXT instead of the stub's generic failure message.
set_service_account_delete_error_text() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  printf '%s' "$2" > "${GCLOUD_STUB_STATE_DIR}/service-accounts/$1.json.delete-fail-text"
}

# set_service_account_delete_not_found EMAIL — `describe` still finds
# this service account, but the next `delete` reports NOT_FOUND (it was
# deleted in between).
set_service_account_delete_not_found() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  touch "${GCLOUD_STUB_STATE_DIR}/service-accounts/$1.json.delete-not-found"
}

# set_service_account_user_keys EMAIL KEY_NAME... — `keys list
# --managed-by=user` for exactly this service account returns these key
# names.
set_service_account_user_keys() {
  local email="$1"
  shift
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  printf '%s\n' "$@" > "${GCLOUD_STUB_STATE_DIR}/service-accounts/${email}.json.user-keys"
}

# set_service_account_keys_list_error EMAIL — `keys list` for exactly
# this service account fails with a PERMISSION_DENIED error.
set_service_account_keys_list_error() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  touch "${GCLOUD_STUB_STATE_DIR}/service-accounts/$1.json.keys-list-error"
}

# set_service_account_grant_will_fail EMAIL [MESSAGE] — every
# `add-iam-policy-binding` on exactly this service account fails with
# MESSAGE on stderr, or by default a realistic PERMISSION_DENIED error.
set_service_account_grant_will_fail() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  printf '%s' "${2:-}" > "${GCLOUD_STUB_STATE_DIR}/service-accounts/$1.json.grant-fail"
}

# set_iap_client_id CLIENT_ID / set_iap_client_id_empty — every `iap
# settings get` call returns this OAuth client id, or none at all (the
# "IAP configured with no OAuth client" refusal case).
set_iap_client_id() {
  printf '%s' "$1" > "${GCLOUD_STUB_STATE_DIR}/iap-client-id.txt"
}
set_iap_client_id_empty() {
  : > "${GCLOUD_STUB_STATE_DIR}/iap-client-id.txt"
}

# set_iap_settings_get_will_fail — every `iap settings get` call
# fails (a permissions problem, IAP not enabled, etc.).
set_iap_settings_get_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/iap-settings-get-should-fail"
}

# set_project_binding_will_fail ROLE [MESSAGE] — every `projects
# add-iam-policy-binding` call for ROLE fails with MESSAGE on stderr, or
# by default a realistic PERMISSION_DENIED error.
set_project_binding_will_fail() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/project-binding-fail"
  printf '%s' "${2:-}" > "${GCLOUD_STUB_STATE_DIR}/project-binding-fail/${1//\//_}"
}

# set_service_account_describe_error EMAIL [MESSAGE] — `describe` for
# exactly this service account fails with MESSAGE, or by default a
# realistic PERMISSION_DENIED error that says nothing about existence.
set_service_account_describe_error() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  printf '%s' "${2:-}" > "${GCLOUD_STUB_STATE_DIR}/service-accounts/$1.json.describe-error"
}

# set_iap_web_remove_binding_error MESSAGE — `iap web
# remove-iam-policy-binding` fails with MESSAGE on stderr, or by default
# a realistic PERMISSION_DENIED error.
set_iap_web_remove_binding_error() {
  printf '%s' "${1:-}" > "${GCLOUD_STUB_STATE_DIR}/iap-web-remove-binding-error"
}

# set_iap_web_binding_will_fail [MEMBER] [MESSAGE] — every `iap web
# add-iam-policy-binding` call for MEMBER (e.g. serviceAccount:x@y), or
# for every member when MEMBER is empty or omitted, fails with MESSAGE on
# stderr, or by default a realistic PERMISSION_DENIED error.
set_iap_web_binding_will_fail() {
  local member="${1:-}"
  if [[ -z "$member" ]]; then
    printf '%s' "${2:-}" > "${GCLOUD_STUB_STATE_DIR}/iap-web-add-binding-should-fail"
  else
    mkdir -p "${GCLOUD_STUB_STATE_DIR}/iap-web-binding-fail"
    printf '%s' "${2:-}" > "${GCLOUD_STUB_STATE_DIR}/iap-web-binding-fail/${member//[:\/@]/_}"
  fi
}

# set_network_missing NAME — simulates a project with no VPC network named
# NAME (e.g. no "default" network -- the hardened-org
# compute.skipDefaultNetworkCreation scenario). Without this, `compute
# networks describe` reports the network as existing, matching the
# realistic default for most projects.
set_network_missing() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/networks"
  touch "${GCLOUD_STUB_STATE_DIR}/networks/$1.missing"
}

# set_network_describe_error NAME MESSAGE — the next (and every
# subsequent) `compute networks describe` call for this network fails
# with MESSAGE (printed verbatim to stderr), instead of the realistic
# "exists" default or the ".missing" not-found shape. Used to simulate a
# permission or transient API error distinct from a genuinely absent
# network.
set_network_describe_error() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/networks"
  printf '%s' "$2" > "${GCLOUD_STUB_STATE_DIR}/networks/$1.describe-error"
}

# set_network_appears_after NAME N — the network doesn't exist for the
# first N `compute networks describe` calls (a realistic "not found",
# matching the async-propagation window right after enabling
# compute.googleapis.com), then exists from the N+1th call onward. N=0
# behaves like the network already existing on the first call. Used to
# exercise the poll loop itself: N below NETWORK_CHECK_MAX_ATTEMPTS
# proves it retries and then proceeds; N at or above it proves it gives
# up after exactly the configured budget, not sooner or later.
set_network_appears_after() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/networks"
  rm -f "${GCLOUD_STUB_STATE_DIR}/networks/$1.describe-count"
  printf '%s' "$2" > "${GCLOUD_STUB_STATE_DIR}/networks/$1.appears-after"
}

# set_network_appears_after_service_disabled NAME N [SHAPE] — same as
# set_network_appears_after, but the first N calls report a
# SERVICE_DISABLED-related error instead of a plain not-found. SHAPE
# selects which one (default "service-disabled"):
#   service-disabled             the realistic shape when gcloud's
#                                 API-enablement prompt is declined (or
#                                 auto-declined by --quiet): the "has not
#                                 been used in project" message, plus a
#                                 details block with reason:
#                                 SERVICE_DISABLED. Has both match
#                                 substrings.
#   service-disabled-bare-message the realistic shape when the
#                                 API-enablement prompt is disabled
#                                 entirely: a "Could not fetch resource:"
#                                 header plus the message, no
#                                 PERMISSION_DENIED prefix and no details
#                                 block. Has only the message-fragment
#                                 substring, proving that match matters on
#                                 its own.
#   service-disabled-reason-only  synthetic, not a shape real gcloud is
#                                 known to produce for this call: a
#                                 details block with reason:
#                                 SERVICE_DISABLED and none of the message
#                                 text. Exists only so the SERVICE_DISABLED
#                                 match is exercised independently of the
#                                 message fragment too.
# Used to prove the retry loop treats each of these the same as a genuine
# not-found, per N1.
set_network_appears_after_service_disabled() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/networks"
  rm -f "${GCLOUD_STUB_STATE_DIR}/networks/$1.describe-count"
  printf '%s' "$2" > "${GCLOUD_STUB_STATE_DIR}/networks/$1.appears-after"
  printf '%s' "${3:-service-disabled}" > "${GCLOUD_STUB_STATE_DIR}/networks/$1.appears-after-error-shape"
}

# set_subnet_cidr NAME CIDR — overrides the CIDR `compute networks subnets
# describe` reports for this subnet. Without this, the stub returns a
# realistic default ("10.128.0.0/20").
set_subnet_cidr() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/subnets"
  printf '%s' "$2" > "${GCLOUD_STUB_STATE_DIR}/subnets/$1.cidr"
}

# set_subnet_missing NAME — simulates a region with no subnet named NAME
# (e.g. a "default" network that was created custom-mode instead of
# auto-mode, so it has no per-region "default" subnet).
set_subnet_missing() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/subnets"
  touch "${GCLOUD_STUB_STATE_DIR}/subnets/$1.missing"
}

# set_run_service_add_binding_will_fail NAME — the next `run services
# add-iam-policy-binding` call for this service fails.
set_run_service_add_binding_will_fail() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/run-services"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/$1.add-binding-fail"
}

# set_run_service_remove_binding_will_fail NAME — the next `run services
# remove-iam-policy-binding` call for this service fails, with a
# simulated error distinct from the stub's default "not found" response
# (see seed_run_service_allusers_invoker below for modeling absence).
set_run_service_remove_binding_will_fail() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/run-services"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/$1.remove-binding-fail"
}

# seed_run_service_allusers_invoker NAME — simulates this service
# currently having an allUsers roles/run.invoker binding (e.g. left by a
# prior --allow-unauthenticated deploy). `run services get-iam-policy`
# reports it present; `run services remove-iam-policy-binding` clears it
# (unless set_run_service_remove_binding_will_fail is also set). Without
# this, get-iam-policy reports no allUsers binding, matching a fresh
# deploy or one where gcloud's own removal already ran.
seed_run_service_allusers_invoker() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/run-services"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/$1.allusers-invoker"
}

# set_run_service_get_policy_will_fail NAME — the next `run services
# get-iam-policy` call for this service fails.
set_run_service_get_policy_will_fail() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/run-services"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/$1.get-policy-fail"
}

# seed_run_service_exists NAME — the next `run services describe` call
# for this service succeeds (simulating a redeploy of an existing
# service). Without this, the stub reports NOT_FOUND.
seed_run_service_exists() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/run-services"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/$1.exists"
}

# set_run_service_describe_error NAME — the next `run services describe`
# call for this service fails (a permissions problem, for example). On
# its own this tells hybrid_cloud_run_label_args nothing, since it never
# trusts describe's error text -- so pair this with either
# seed_run_service_exists (the follow-up `list` still finds it: no
# label) or set_run_service_list_error (list also fails: no label,
# fail-safe) to exercise the two ways a describe failure can resolve.
set_run_service_describe_error() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/run-services"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/$1.describe-error"
}

# set_run_service_delete_error NAME [TEXT] — `run services delete` for
# this service fails. With TEXT, the stub prints TEXT as the error;
# without it, a PERMISSION_DENIED error that is not a not-found.
set_run_service_delete_error() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/run-services"
  printf '%s' "${2:-}" > "${GCLOUD_STUB_STATE_DIR}/run-services/$1.delete-error"
}

# set_nat_delete_error NAME [TEXT] — `compute routers nats delete` for
# this NAT fails, printing TEXT (or a generic failure) as the error.
set_nat_delete_error() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/nats"
  printf '%s' "${2:-}" > "${GCLOUD_STUB_STATE_DIR}/nats/$1.delete-error"
}

# set_router_delete_error [TEXT] — `compute routers delete` fails,
# printing TEXT (or a generic failure) as the error.
set_router_delete_error() {
  printf '%s' "${1:-}" > "${GCLOUD_STUB_STATE_DIR}/router-delete-error"
}

# set_run_service_list_error — every `run services list` call fails
# outright, simulating "truly cannot tell whether it exists". The
# positive-absence check must fail safe (no label) here, never read a
# failed list as confirmation of absence.
set_run_service_list_error() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/run-services"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/list-error"
}

# set_firewall_delete_will_fail NAME — the next (and every subsequent)
# `firewall-rules delete` call for this rule fails instead of succeeding
# (the rule's JSON stays present in stub state, matching a real failed
# delete rather than one that succeeded or found nothing).
set_firewall_delete_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/firewall-rules/$1.json.delete-fail"
}

# set_firewall_delete_error_text NAME TEXT — used with
# set_firewall_delete_will_fail above: the failed delete prints TEXT
# instead of the stub's generic failure message.
set_firewall_delete_error_text() {
  printf '%s' "$2" > "${GCLOUD_STUB_STATE_DIR}/firewall-rules/$1.json.delete-fail-text"
}

# set_firewall_describe_target_tags_will_fail NAME — the next (and every
# subsequent) `firewall-rules describe --format=value(targetTags)` call
# for this rule fails, while a plain `describe` for the same rule still
# succeeds -- simulating a transient error on the second of the two
# describe calls deploy.sh makes for an existing rule, distinct from the
# rule not existing at all.
set_firewall_describe_target_tags_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/firewall-rules/$1.json.describe-target-tags-fail"
}

# seed_firewall_rule_desc_only NAME DESCRIPTION — simulates a pre-existing
# rule that carries an arbitrary (possibly non-marker) description, with no
# other fields set.
seed_firewall_rule_desc_only() {
  "$PYTHON" "${HARNESS_LIB_DIR}/firewall-rule-json.py" \
    "$2" "default" "INGRESS" "ALLOW" "tcp" "22" "" "" "" "900" \
    > "${GCLOUD_STUB_STATE_DIR}/firewall-rules/$1.json"
}

# seed_address NAME ADDRESS DESC [ADDRESS_TYPE [SUBNET [REGION [PROJECT]]]]
# — a pre-existing static internal address reservation with the given
# address and description. REGION defaults to us-central1, matching every
# existing fixture; only tests that care about cross-region name
# collisions need to pass a different one. PROJECT (default demo-project)
# is the project in the stored URLs. `region` is stored as the full
# region URL, as the real list returns it.
seed_address() {
  local name="$1" address="$2" desc="$3" address_type="${4:-INTERNAL}" subnet="${5:-default}" region="${6:-us-central1}" project="${7:-demo-project}"
  _address_json "$address" "$desc" "$address_type" "$subnet" "$region" "$project" \
    > "${GCLOUD_STUB_STATE_DIR}/addresses/${name}.json"
}

# seed_address_extra NAME ADDRESS DESC REGION PROJECT [ADDRESS_TYPE [SUBNET]]
# — one more address with the same NAME, in REGION of PROJECT's URLs,
# for the cross-region name-collision tests. ADDRESS_TYPE defaults to
# INTERNAL and SUBNET to default. It is returned only by `compute
# addresses list` (listed before the NAME fixture from seed_address, if
# any), subject to the same region: filter as every other entry;
# describe, create and delete never see it.
seed_address_extra() {
  local name="$1" address="$2" desc="$3" region="$4" project="$5" address_type="${6:-INTERNAL}" subnet="${7:-default}"
  _address_json "$address" "$desc" "$address_type" "$subnet" "$region" "$project" \
    > "${GCLOUD_STUB_STATE_DIR}/addresses/${name}.json.extra-${project}-${region}"
}

# _address_json ADDRESS DESC ADDRESS_TYPE SUBNET REGION PROJECT — one
# address resource, as JSON, in the shape the real list returns.
_address_json() {
  "$PYTHON" -c "
import json, sys
addr, desc, address_type, subnet, region, project = sys.argv[1:7]
region_url = 'https://www.googleapis.com/compute/v1/projects/' + project + '/regions/' + region
print(json.dumps({
    'address': addr,
    'description': desc,
    'addressType': address_type,
    'subnetwork': region_url + '/subnetworks/' + subnet,
    'region': region_url,
}))
" "$@"
}

# seed_address_unmarked NAME ADDRESS — a pre-existing reservation with no
# marker description, for the "refuse to adopt" test.
seed_address_unmarked() {
  seed_address "$1" "$2" "some other unrelated reservation"
}

# set_address_delete_will_fail NAME — every `compute addresses delete`
# call for this reservation fails instead of succeeding.
set_address_delete_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/addresses/$1.json.delete-fail"
}

# set_address_create_will_fail NAME — every `compute addresses create`
# call for this reservation fails instead of succeeding.
set_address_create_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/addresses/$1.json.create-fail"
}

# set_address_list_will_fail NAME — every `compute addresses list`
# call filtered to this name fails instead of returning a result.
set_address_list_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/addresses/$1.json.list-error"
}

# set_address_list_invalid_json NAME — every `compute addresses list`
# call filtered to this name succeeds (exit 0) but prints text that is
# not JSON.
set_address_list_invalid_json() {
  touch "${GCLOUD_STUB_STATE_DIR}/addresses/$1.json.list-invalid-json"
}

# set_address_list_will_fail_after_create NAME — the address doesn't
# exist yet, so the next `compute addresses list` call (checking
# absence) still succeeds as empty; but once a `compute addresses
# create` call for NAME has actually run, every list call after that
# fails, simulating list eventual-consistency lag right after create.
set_address_list_will_fail_after_create() {
  touch "${GCLOUD_STUB_STATE_DIR}/addresses/$1.json.list-error-after-create"
}

# seed_firewall_rule_json NAME DESC NETWORK DIRECTION ACTION PROTO PORTS \
#   SOURCE_TAGS SOURCE_RANGES TARGET_TAGS PRIORITY \
#   [SOURCE_SAS [TARGET_SAS [DEST_RANGES [DISABLED [EXTRA_PROTO [EXTRA_PORTS]]]]]]
#
# Simulates a pre-existing rule with a fully specified spec. The trailing
# fields are optional and default to empty/false; see firewall-rule-json.py
# for what each one builds.
seed_firewall_rule_json() {
  local name="$1"
  shift
  "$PYTHON" "${HARNESS_LIB_DIR}/firewall-rule-json.py" "$@" \
    > "${GCLOUD_STUB_STATE_DIR}/firewall-rules/${name}.json"
}

# _seed_default_gke_node_tag_rules NAME NETWORK POD_CIDR — (re)writes the
# GKE-managed-style -all/-vms firewall rule pair node-tag discovery reads,
# keyed to this cluster's own name so multiple clusters on one network
# don't collide, and to whatever pod CIDR it currently has. Called from
# seed_cluster (first write) and seed_pod_cidr (to keep the two in sync
# when a test changes the CIDR after the fact). Tests that want a
# specific failure shape instead call seed_gke_node_tag_rules directly,
# or clear_gke_node_tag_rules to remove this default entirely.
_seed_default_gke_node_tag_rules() {
  local name="$1" network="$2" pod_cidr="$3"
  seed_gke_node_tag_rules "$name" "$network" "$pod_cidr" "gke-${name}-x-node"
}

# seed_gke_node_tag_rules NAME NETWORK POD_CIDR TAG [VMS_TAG] [PREFIX] —
# writes a matching -all/-vms rule pair (both named gke-<name>-<PREFIX>-*,
# both INGRESS, -all sourced from POD_CIDR) with the given target tag.
# PREFIX defaults to "x" (matching _seed_default_gke_node_tag_rules and
# every test that doesn't care about the exact rule name); pass the real
# per-cluster hash-shaped suffix to mirror an actual GKE-managed rule
# name exactly. VMS_TAG defaults to TAG (the two rules agreeing, the
# normal case); pass a different value to build the "-all and -vms
# disagree" fixture, or a comma-separated list to build the "-vms rule
# itself carries more than one target tag" fixture. Direction defaults to
# INGRESS; pass a 7th argument to override the -all rule's direction,
# for the "candidate not INGRESS" fixture. NETWORK is a
# short name (e.g. "default"); the fixture's own 'network' field is
# written as the full self-link URL real `firewall-rules list --format=
# json` actually returns, so discovery's own network-name match is
# what a test exercises: its list call is unfiltered and returns every
# seeded rule on every network. Pass "relative" as an 8th argument to
# write the 'network' field in the relative form
# projects/<project>/global/networks/<network> instead.
seed_gke_node_tag_rules() {
  local name="$1" network="$2" pod_cidr="$3" tag="$4" vms_tag="${5:-$4}" prefix="${6:-x}" direction="${7:-INGRESS}" network_form="${8:-url}"
  "$PYTHON" -c "
import json, sys
name, network, pod_cidr, tag, vms_tag, prefix, direction, project, network_form = sys.argv[1:10]
network_url = 'projects/%s/global/networks/%s' % (project, network)
if network_form != 'relative':
    network_url = 'https://www.googleapis.com/compute/v1/' + network_url
# TAG and VMS_TAG may each be a comma-separated list, to build the 'more
# than one target tag' fixture on either rule; an empty string means zero
# target tags.
rules = {
    'gke-%s-%s-all' % (name, prefix): {
        'name': 'gke-%s-%s-all' % (name, prefix), 'network': network_url, 'direction': direction,
        'sourceRanges': [pod_cidr], 'targetTags': tag.split(',') if tag else [],
    },
    'gke-%s-%s-vms' % (name, prefix): {
        'name': 'gke-%s-%s-vms' % (name, prefix), 'network': network_url, 'direction': 'INGRESS',
        'sourceRanges': ['10.128.0.0/9'], 'targetTags': vms_tag.split(',') if vms_tag else [],
    },
}
for rule_name, body in rules.items():
    with open(sys.argv[10] + '/' + rule_name + '.json', 'w') as f:
        json.dump(body, f)
" "$name" "$network" "$pod_cidr" "$tag" "$vms_tag" "$prefix" "$direction" "${GCLOUD_STUB_PROJECT:-demo-project}" \
    "$network_form" "${GCLOUD_STUB_STATE_DIR}/gke-node-firewall-rules"
}

# clear_gke_node_tag_rules NAME — removes the auto-seeded (or explicitly
# seeded) -all/-vms pair for this cluster name, for tests that want
# discovery to find nothing at all.
clear_gke_node_tag_rules() {
  rm -f "${GCLOUD_STUB_STATE_DIR}/gke-node-firewall-rules/gke-$1-"*"-all.json" \
    "${GCLOUD_STUB_STATE_DIR}/gke-node-firewall-rules/gke-$1-"*"-vms.json"
}

# seed_cluster NAME NETWORK — the fixture cluster's reported network.
# Also writes this cluster's default node-tag firewall rule pair (see
# _seed_default_gke_node_tag_rules) so discovery succeeds out of the box;
# tests that care about a specific tag or failure shape override it with
# seed_gke_node_tag_rules or clear_gke_node_tag_rules.
seed_cluster() {
  local name="$1" network="$2"
  "$PYTHON" -c "
import json, sys
network = sys.argv[1]
body = {
    'network': network,
    'subnetwork': 'default-subnet',
    # A default pod CIDR, agreeing between both fields hybrid_discover
    # cross-checks, so existing fixtures that don't care about the
    # actual pod CIDR value don't all need updating -- same rationale as
    # seed_subnet's own implicit default below.
    'clusterIpv4Cidr': '10.52.0.0/14',
    'ipAllocationPolicy': {'clusterIpv4CidrBlock': '10.52.0.0/14'},
}
print(json.dumps(body))
" "$network" > "${GCLOUD_STUB_STATE_DIR}/clusters/${name}.json"
  _seed_default_gke_node_tag_rules "$name" "$network" "10.52.0.0/14"
}

# seed_pod_cidr NAME CIDR — overrides both clusterIpv4Cidr and
# ipAllocationPolicy.clusterIpv4CidrBlock on an already-seeded cluster
# fixture to the same explicit value, for tests that care about the
# actual pod CIDR hybrid_discover reads. Also rewrites the default
# node-tag rule pair for the new CIDR, so
# a test that changes the pod CIDR and still expects discovery to
# succeed doesn't have to know about the firewall-rule fixture at all.
seed_pod_cidr() {
  local name="$1" cidr="$2" services_cidr="${3:-}"
  "$PYTHON" -c "
import json, sys
p = sys.argv[1]
cidr = sys.argv[2]
services_cidr = sys.argv[3]
d = json.load(open(p))
d['clusterIpv4Cidr'] = cidr
d['ipAllocationPolicy'] = {'clusterIpv4CidrBlock': cidr}
if services_cidr:
    d['servicesIpv4Cidr'] = services_cidr
json.dump(d, open(p, 'w'))
" "${GCLOUD_STUB_STATE_DIR}/clusters/${name}.json" "$cidr" "$services_cidr"
  local network
  network="$("$PYTHON" -c "import json,sys; print(json.load(open(sys.argv[1])).get('network') or '')" \
    "${GCLOUD_STUB_STATE_DIR}/clusters/${name}.json")"
  if [[ -f "${GCLOUD_STUB_STATE_DIR}/gke-node-firewall-rules/gke-${name}-x-all.json" ]]; then
    _seed_default_gke_node_tag_rules "$name" "$network" "$cidr"
  fi
}

# seed_pod_cidr_mismatch NAME CIDR ALT_CIDR — sets clusterIpv4Cidr and
# ipAllocationPolicy.clusterIpv4CidrBlock to two DIFFERENT values, for
# the "the two disagree" refusal test.
seed_pod_cidr_mismatch() {
  local name="$1" cidr="$2" alt_cidr="$3"
  "$PYTHON" -c "
import json, sys
p, cidr, alt = sys.argv[1:4]
d = json.load(open(p))
d['clusterIpv4Cidr'] = cidr
d['ipAllocationPolicy'] = {'clusterIpv4CidrBlock': alt}
json.dump(d, open(p, 'w'))
" "${GCLOUD_STUB_STATE_DIR}/clusters/${name}.json" "$cidr" "$alt_cidr"
}

# seed_pod_cidr_missing NAME — removes both pod-CIDR fields entirely,
# for the "absent" refusal test.
seed_pod_cidr_missing() {
  local name="$1"
  "$PYTHON" -c "
import json, sys
p = sys.argv[1]
d = json.load(open(p))
d.pop('clusterIpv4Cidr', None)
d.pop('ipAllocationPolicy', None)
json.dump(d, open(p, 'w'))
" "${GCLOUD_STUB_STATE_DIR}/clusters/${name}.json"
}

# seed_subnet NAME CIDR — overrides the stub's default node-subnet
# fixture ("default-subnet" / 10.128.0.0/20, seeded implicitly so
# existing cluster fixtures don't all need updating) with an explicit
# name and primary IP range, for tests that care about the actual CIDR
# value.
seed_subnet() {
  local name="$1" cidr="$2"
  printf '{"ipCidrRange": "%s"}' "$cidr" > "${GCLOUD_STUB_STATE_DIR}/subnets/${name}.json"
}

# set_subnet_describe_will_fail NAME — every `networks subnets
# describe` call for this subnet fails instead of returning a range.
set_subnet_describe_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/subnets/$1.json.describe-fail"
}

# seed_routers_list JSON — writes JSON (a full JSON array, in the shape
# `gcloud compute routers list --format=json` returns) as the fixture the
# stub's `compute routers list` case serves. Without this, the stub
# defaults to an empty list ("[]").
seed_routers_list() {
  printf '%s' "$1" > "${GCLOUD_STUB_STATE_DIR}/routers-list.json"
}

# set_routers_list_will_fail — the next (and every subsequent) `compute
# routers list` call fails instead of returning a list.
set_routers_list_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/routers-list-should-fail"
}

# set_routers_list_returns_empty — the next (and every subsequent)
# `compute routers list` call exits 0 but prints nothing, distinct from
# set_routers_list_will_fail's exit 1: a real `list` that succeeds always
# prints at least "[]", so empty stdout on a zero exit is its own failure
# mode (a caller parsing it as JSON gets nothing to parse), not the "no
# routers" empty-array case.
set_routers_list_returns_empty() {
  touch "${GCLOUD_STUB_STATE_DIR}/routers-list-should-return-empty"
}

gcloud_log() {
  [[ -f "$GCLOUD_STUB_LOG" ]] && cat "$GCLOUD_STUB_LOG"
}

gcloud_call_count() {
  gcloud_log | grep -c . || true
}

# line_number PATTERN LOG — the 1-based line number of the first log line
# containing PATTERN, or empty if none matches. Shared here (rather than
# left as a private helper in whichever test file happens to need it
# first) because more than one tests/test_*.sh file wants ordering
# assertions against gcloud_log, and a test file may only use
# lib/harness.sh plus its own definitions -- see README.md "How it works".
line_number() {
  echo "$2" | grep -n -F -- "$1" | head -1 | cut -d: -f1
}

# --- Assertions ----------------------------------------------------------
# Each prints PASS/FAIL with the current test name and bumps the counters.
# CURRENT_TEST is set by run.sh before invoking each test_* function.

assert_eq() {
  local expected="$1" actual="$2" msg="${3:-}"
  if [[ "$expected" == "$actual" ]]; then
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "FAIL [${CURRENT_TEST}]: ${msg} (expected '${expected}', got '${actual}')"
  fi
}

assert_contains() {
  local haystack="$1" needle="$2" msg="${3:-}"
  if [[ "$haystack" == *"$needle"* ]]; then
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "FAIL [${CURRENT_TEST}]: ${msg} (expected to find '${needle}')"
    echo "  --- haystack ---"
    local line
    while IFS= read -r line; do
      echo "  ${line}"
    done <<< "$haystack"
  fi
}

assert_not_contains() {
  local haystack="$1" needle="$2" msg="${3:-}"
  if [[ "$haystack" != *"$needle"* ]]; then
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "FAIL [${CURRENT_TEST}]: ${msg} (did not expect to find '${needle}')"
  fi
}

assert_true() {
  local cond="$1" msg="${2:-}"
  if [[ "$cond" == "true" ]]; then
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "FAIL [${CURRENT_TEST}]: ${msg} (expected true, got '${cond}')"
  fi
}

# assert_false COND MSG — COND must be "" or "false" (the two shapes
# `$([[ x ]] && echo true)` and `$([[ x ]] && echo true || echo false)`
# both produce when x is false).
assert_false() {
  local cond="$1" msg="${2:-}"
  if [[ -z "$cond" || "$cond" == "false" ]]; then
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "FAIL [${CURRENT_TEST}]: ${msg} (expected false/empty, got '${cond}')"
  fi
}

# run_expect_fail CMD... — runs a command that is expected to call `exit`
# with a non-zero status. The command substitution below already runs it
# in a subshell, so its `exit` can never end the calling test (each test
# additionally runs in its own subshell at the run.sh level, so this is
# defense in depth, not the only thing preventing that). Restores
# errexit to whatever it was before the call, rather than unconditionally
# turning it on, so this never changes the caller's shell options as a
# side effect. Sets RUN_EXIT_CODE and RUN_OUTPUT, both read back by the
# caller.
run_expect_fail() {
  local out had_errexit=false before_tmp after_tmp
  case "$-" in *e*) had_errexit=true ;; esac
  set +e
  before_tmp="$(PATH="$HARNESS_SAFE_PATH" find "${TMPDIR:-/tmp}" -mindepth 1 2>/dev/null | PATH="$HARNESS_SAFE_PATH" sort)"
  out="$("$@" 2>&1)"
  # shellcheck disable=SC2034 # read by callers
  RUN_EXIT_CODE=$?
  [[ "$had_errexit" == "true" ]] && set -e
  # shellcheck disable=SC2034 # read by callers
  RUN_OUTPUT="$out"
  # "$@" above runs inside a command-substitution subshell, so a mktemp
  # file/dir it creates is invisible here once the subshell exits: there
  # is no variable left to rm -f, only the file on disk. Diffing TMPDIR
  # before/after and removing whatever appeared catches this generically,
  # for any function this wraps, not just the ones a test author
  # remembered to audit.
  after_tmp="$(PATH="$HARNESS_SAFE_PATH" find "${TMPDIR:-/tmp}" -mindepth 1 2>/dev/null | PATH="$HARNESS_SAFE_PATH" sort)"
  if [[ "$after_tmp" != "$before_tmp" ]]; then
    PATH="$HARNESS_SAFE_PATH" comm -13 <(printf '%s\n' "$before_tmp") <(printf '%s\n' "$after_tmp") | while IFS= read -r _new_tmp_entry; do
      [[ -n "$_new_tmp_entry" ]] && rm -rf "$_new_tmp_entry"
    done
  fi
}
