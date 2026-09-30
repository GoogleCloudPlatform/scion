# scripts/single-node-vm/tests/test_hybrid_tier.sh — tests for
# hybrid-tier.sh. Sourced by run.sh, which also sources lib/harness.sh
# first and puts lib/ (containing the stub `gcloud`) at the front of PATH.
# No test here contacts GCP.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

HUB="demohub"
PROJECT="demo-project"
NETWORK="default"
REGION="us-central1"
ALLOW_NAME="scion-hub-${HUB}-nfs-allow"
DENY_NAME="scion-hub-${HUB}-nfs-deny"
HUB_DENY_NAME="scion-hub-${HUB}-hub-deny"
TARGET_TAG="scion-hub-${HUB}-nfs"
MARKER="scion-deployment=${HUB}"
INSTANCE_NAME_TEST="scion-hub-${HUB}"

# --- tier-off: reading an absent config makes zero gcloud calls -----------
test_tier_off_no_gcloud_calls() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]=""
  hybrid_read_config "$PROJECT" "$HUB"
  assert_eq "false" "$HYBRID_ENABLED" "tier should be off when gke_target.name is absent"
  assert_eq "0" "$(gcloud_call_count)" "no gcloud call should happen reading config alone"
}

# --- config: project mismatch is refused actionably ------------------------
test_config_project_mismatch_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  CONFIG["gke_target.location"]="us-central1"
  CONFIG["gke_target.project"]="other-project"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "mismatched gke_target.project must fail the run"
  assert_contains "$RUN_OUTPUT" "other-project" "error should name the offending project"
}

# --- config: missing location is refused actionably -------------------------
test_config_missing_location_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "missing gke_target.location must fail the run"
  # Specifically the "is required" check, not merely that the later regex
  # validation also happens to reject an empty string with a message that
  # happens to also contain the word "location" (which would still pass a
  # bare substring check without actually pinning this specific check).
  assert_contains "$RUN_OUTPUT" "is required when gke_target.name is set" \
    "error should be the missing-field message, not some other check that also mentions location"
}

# --- config: same-project cluster enables the tier --------------------------
test_config_enables_tier() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  # shellcheck disable=SC2034 # read by config_get (harness.sh) via hybrid_read_config
  CONFIG["gke_target.location"]="us-central1"
  hybrid_read_config "$PROJECT" "$HUB"
  assert_eq "true" "$HYBRID_ENABLED" "tier should be on when name+location are set"
  assert_eq "mycluster" "$GKE_NAME" "GKE_NAME should come from config"
  assert_eq "$PROJECT" "$GKE_PROJECT" "GKE_PROJECT should default to the hub's project"
  assert_eq "scion-hub-${HUB}" "$GKE_NAMESPACE" "GKE_NAMESPACE should default to scion-hub-<hub> when not set in config"
  assert_eq "scion-hub-${HUB}-shared" "$GKE_PVC_NAME" "GKE_PVC_NAME should default to scion-hub-<hub>-shared when not set in config"
}

test_config_namespace_and_pvc_name_from_config() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  # shellcheck disable=SC2034 # read by config_get (harness.sh) via hybrid_read_config
  CONFIG["gke_target.location"]="us-central1"
  CONFIG["gke_target.namespace"]="custom-ns"
  CONFIG["gke_target.pvc_name"]="custom-pvc"
  hybrid_read_config "$PROJECT" "$HUB"
  assert_eq "custom-ns" "$GKE_NAMESPACE" "an explicit gke_target.namespace must override the default"
  assert_eq "custom-pvc" "$GKE_PVC_NAME" "an explicit gke_target.pvc_name must override the default"
}

test_config_namespace_invalid_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  # shellcheck disable=SC2034 # read by config_get (harness.sh) via hybrid_read_config
  CONFIG["gke_target.location"]="us-central1"
  CONFIG["gke_target.namespace"]="Not_Valid"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an invalid namespace name must be refused"
  assert_contains "$RUN_OUTPUT" "gke_target.namespace" "error should name the offending field"
}

test_config_pvc_name_invalid_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  # shellcheck disable=SC2034 # read by config_get (harness.sh) via hybrid_read_config
  CONFIG["gke_target.location"]="us-central1"
  CONFIG["gke_target.pvc_name"]="Not_Valid"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an invalid PVC name must be refused"
  assert_contains "$RUN_OUTPUT" "gke_target.pvc_name" "error should name the offending field"
}

test_k8s_ensure_objects_uses_hybrid_read_config_override() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  # shellcheck disable=SC2034 # read by config_get (harness.sh) via hybrid_read_config
  CONFIG["gke_target.location"]="us-central1"
  CONFIG["gke_target.namespace"]="custom-ns"
  CONFIG["gke_target.pvc_name"]="custom-pvc"
  hybrid_read_config "$PROJECT" "$HUB"
  hybrid_k8s_setup_kubeconfig
  hybrid_k8s_ensure_objects "$HUB" "$K8S_VM_IP"
  assert_true "$([[ -f "${KUBECTL_STUB_STATE_DIR}/namespace/custom-ns.json" ]] && echo true || echo false)" \
    "the namespace set via hybrid_read_config (interactive-prompt-capable) must be the one actually used, not just the default"
  assert_true "$([[ -f "${KUBECTL_STUB_STATE_DIR}/pvc/custom-ns__custom-pvc.json" ]] && echo true || echo false)" \
    "the PVC name set via hybrid_read_config must be the one actually used"
  rm -f "$HYBRID_KUBECONFIG"
}

# Drives the REAL interactive prompt path (no config file), with a real
# config_prompt (read -rp against stdin) standing in for the harness's
# own stub, which errors on any call it doesn't expect -- exactly what
# the interactive branch needs to actually exercise. Restores the
# stub afterward so no other test's use of config_prompt is affected.
test_read_config_interactive_prompts_for_namespace_and_pvc_name() {
  fresh_gcloud_state
  # shellcheck disable=SC2034 # read by hybrid_read_config's own [[ -z "${CONFIG_FILE:-}" ]] check, via this local shadow
  local CONFIG_FILE=""
  local saved_config_prompt
  saved_config_prompt="$(declare -f config_prompt)"
  # shellcheck disable=SC2317,SC2329 # invoked indirectly, by hybrid_read_config below
  config_prompt() {
    local varname="$1" prompt="$2" default="$3" input
    read -rp "$prompt" input
    printf -v "$varname" '%s' "${input:-$default}"
  }
  hybrid_read_config "$PROJECT" "$HUB" <<'STDIN'
y
mycluster
us-central1

custom-ns
custom-pvc
STDIN
  eval "$saved_config_prompt"
  assert_eq "true" "$HYBRID_ENABLED" "answering 'y' to the hybrid-tier prompt must enable the tier"
  assert_eq "custom-ns" "$GKE_NAMESPACE" \
    "the interactive namespace prompt's answer must be used, not silently skipped or defaulted"
  assert_eq "custom-pvc" "$GKE_PVC_NAME" \
    "the interactive PVC-name prompt's answer must be used, not silently skipped or defaulted"
}

# =====================================================================
# Discovery: the node tag is read from the cluster's own GKE-managed
# firewall rules, the same mechanism for Standard and Autopilot.
# =====================================================================

test_discover_standard_shape() {
  fresh_gcloud_state
  GKE_NAME="demo-cluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "demo-cluster" "$NETWORK"
  seed_pod_cidr "demo-cluster" "10.44.0.0/17"
  clear_gke_node_tag_rules "demo-cluster"
  seed_gke_node_tag_rules "demo-cluster" "$NETWORK" "10.44.0.0/17" \
    "gke-demo-cluster-0123abcd-node" "gke-demo-cluster-0123abcd-node" "0123abcd"

  hybrid_discover "$NETWORK"
  assert_eq "gke-demo-cluster-0123abcd-node" "$GKE_NODE_TAG" \
    "should discover the node tag via the cluster's GKE-managed firewall rules"
  assert_eq "10.128.0.0/20" "$GKE_NODE_SUBNET_CIDR" "should discover the node subnet's primary CIDR"
}

# Autopilot node instance groups, templates and instances are not visible
# as Compute resources in the project (a 404 on every instance-group
# describe, even for a cluster with running nodes) -- discovery must
# never depend on reading them, for either cluster type.
test_discover_autopilot_shape() {
  fresh_gcloud_state
  GKE_NAME="aplcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "aplcluster" "$NETWORK"

  hybrid_discover "$NETWORK"
  assert_eq "gke-aplcluster-x-node" "$GKE_NODE_TAG" \
    "should discover the Autopilot node tag via firewall rules, the same path as a Standard cluster"
  assert_not_contains "$(gcloud_log)" "instance-groups managed describe" \
    "discovery must never call instance-groups managed describe -- it 404s on Autopilot even when nodes are running"
}

test_discover_no_all_rule_refused_with_evidence() {
  fresh_gcloud_state
  GKE_NAME="notagcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "notagcluster" "$NETWORK"
  clear_gke_node_tag_rules "notagcluster"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "no -all rule must fail the run"
  assert_contains "$RUN_OUTPUT" "notagcluster" "error should name the cluster"
  assert_contains "$RUN_OUTPUT" "missing or ambiguous" "error should say the cluster's firewall rules are missing or ambiguous"
  assert_contains "$RUN_OUTPUT" "cluster's own firewall rules" "error should say this is fixed on the cluster, not in this script"
}

test_discover_pod_cidr_matches_no_rule_refused_with_evidence() {
  fresh_gcloud_state
  GKE_NAME="nomatchcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "nomatchcluster" "$NETWORK"
  seed_pod_cidr "nomatchcluster" "10.44.0.0/17"
  # The -all rule exists, but its source range doesn't cover this
  # cluster's pod CIDR -- distinct from no -all rule existing at all.
  seed_gke_node_tag_rules "nomatchcluster" "$NETWORK" "10.60.0.0/17" "gke-nomatchcluster-x-node"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an -all rule whose source range doesn't cover the pod CIDR must fail the run"
  assert_contains "$RUN_OUTPUT" "gke-nomatchcluster-x-all" "error should list the rule that was found"
  assert_contains "$RUN_OUTPUT" "10.60.0.0/17" "error should list the source range that was found"
}

test_discover_candidate_not_ingress_refused_with_evidence() {
  fresh_gcloud_state
  GKE_NAME="egresscluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "egresscluster" "$NETWORK"
  seed_pod_cidr "egresscluster" "10.44.0.0/17"
  seed_gke_node_tag_rules "egresscluster" "$NETWORK" "10.44.0.0/17" "gke-egresscluster-x-node" \
    "gke-egresscluster-x-node" "x" "EGRESS"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a matching rule whose direction isn't INGRESS must fail the run"
  assert_contains "$RUN_OUTPUT" "gke-egresscluster-x-all" "error should list the rule that was found"
  assert_contains "$RUN_OUTPUT" "direction=EGRESS" "error should list the direction that was found"
}

test_discover_ambiguous_candidates_refused_with_evidence() {
  fresh_gcloud_state
  GKE_NAME="ambigcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "ambigcluster" "$NETWORK"
  seed_pod_cidr "ambigcluster" "10.44.0.0/17"
  clear_gke_node_tag_rules "ambigcluster"
  seed_gke_node_tag_rules "ambigcluster" "$NETWORK" "10.44.0.0/17" "gke-ambigcluster-aaa-node" \
    "gke-ambigcluster-aaa-node" "aaa"
  seed_gke_node_tag_rules "ambigcluster" "$NETWORK" "10.44.0.0/17" "gke-ambigcluster-bbb-node" \
    "gke-ambigcluster-bbb-node" "bbb"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "more than one candidate must fail the run, never guess"
  assert_contains "$RUN_OUTPUT" "ambigcluster" "error should name the cluster"
  assert_contains "$RUN_OUTPUT" "gke-ambigcluster-aaa-all" "error should list the first candidate rule"
  assert_contains "$RUN_OUTPUT" "gke-ambigcluster-bbb-all" "error should list the second candidate rule"
}

test_discover_all_rule_two_node_tags_refused_with_evidence() {
  fresh_gcloud_state
  GKE_NAME="twotagcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "twotagcluster" "$NETWORK"
  seed_pod_cidr "twotagcluster" "10.44.0.0/17"
  # VMS_TAG is pinned to exactly the first of the -all rule's two tags:
  # if the exactly-one-tag check were bypassed, tags[0] is what the rest
  # of the function would go on to use, and it must agree with -vms here
  # so a bypassed check shows up as an unexpected SUCCESS, not a second,
  # different failure (the -all/-vms disagreement check) that would make
  # this test pass for the wrong reason either way.
  seed_gke_node_tag_rules "twotagcluster" "$NETWORK" "10.44.0.0/17" \
    "gke-twotagcluster-x-node,gke-twotagcluster-y-node" "gke-twotagcluster-x-node"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an -all rule with more than one target tag must fail the run"
  assert_contains "$RUN_OUTPUT" "gke-twotagcluster-x-all has 2 target tag(s)" "error should name the rule and say how many tags it has"
  assert_contains "$RUN_OUTPUT" "gke-twotagcluster-x-node" "error should list the first tag found"
  assert_contains "$RUN_OUTPUT" "gke-twotagcluster-y-node" "error should list the second tag found"
}

test_discover_all_vms_tags_disagree_refused_with_evidence() {
  fresh_gcloud_state
  GKE_NAME="disagreecluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "disagreecluster" "$NETWORK"
  seed_pod_cidr "disagreecluster" "10.44.0.0/17"
  seed_gke_node_tag_rules "disagreecluster" "$NETWORK" "10.44.0.0/17" \
    "gke-disagreecluster-x-node" "gke-disagreecluster-y-node"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "the -all and -vms rules' target tags disagreeing must fail the run"
  assert_contains "$RUN_OUTPUT" "gke-disagreecluster-x-vms" "error should name the -vms rule"
  assert_contains "$RUN_OUTPUT" "gke-disagreecluster-x-node" "error should list the -all rule's tag"
  assert_contains "$RUN_OUTPUT" "gke-disagreecluster-y-node" "error should list the -vms rule's disagreeing tag"
}

test_discover_missing_vms_rule_refused_with_evidence() {
  fresh_gcloud_state
  GKE_NAME="novmscluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "novmscluster" "$NETWORK"
  seed_pod_cidr "novmscluster" "10.44.0.0/17"
  seed_gke_node_tag_rules "novmscluster" "$NETWORK" "10.44.0.0/17" "gke-novmscluster-x-node"
  rm -f "${GCLOUD_STUB_STATE_DIR}/gke-node-firewall-rules/gke-novmscluster-x-vms.json"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a missing -vms rule must fail the run"
  assert_contains "$RUN_OUTPUT" "gke-novmscluster-x-vms" "error should name the missing rule"
}

test_discover_vms_rule_two_tags_refused_with_evidence() {
  fresh_gcloud_state
  GKE_NAME="vmstwotagcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "vmstwotagcluster" "$NETWORK"
  seed_pod_cidr "vmstwotagcluster" "10.44.0.0/17"
  # The -all rule carries exactly one tag, so this exercises the -vms
  # side's own equality check specifically: comparing only the first
  # element of a two-element list would wrongly accept this, since the
  # first element does agree with the -all rule's tag.
  seed_gke_node_tag_rules "vmstwotagcluster" "$NETWORK" "10.44.0.0/17" \
    "gke-vmstwotagcluster-x-node" "gke-vmstwotagcluster-x-node,extra-tag"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a -vms rule carrying an extra target tag beyond the agreeing one must fail the run"
  assert_contains "$RUN_OUTPUT" "gke-vmstwotagcluster-x-vms" "error should name the -vms rule"
  assert_contains "$RUN_OUTPUT" "extra-tag" "error should list the -vms rule's extra tag"
}

test_discover_pod_cidr_substring_decoy_refused_with_evidence() {
  fresh_gcloud_state
  GKE_NAME="substringcidrcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "substringcidrcluster" "$NETWORK"
  seed_pod_cidr "substringcidrcluster" "10.44.0.0/17"
  clear_gke_node_tag_rules "substringcidrcluster"
  # sourceRanges contains the pod CIDR as a substring of a longer string,
  # never as an element in its own right -- pod-CIDR membership must be
  # exact-element equality, not a substring test.
  seed_gke_node_tag_rules "substringcidrcluster" "$NETWORK" "110.44.0.0/17x" "gke-substringcidrcluster-x-node"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a sourceRanges entry containing the pod CIDR only as a substring must not be accepted"
  assert_contains "$RUN_OUTPUT" "110.44.0.0/17x" "error should list the decoy range as seen"
}

test_discover_pod_cidr_broader_range_decoy_refused_with_evidence() {
  fresh_gcloud_state
  GKE_NAME="broadrangecluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "broadrangecluster" "$NETWORK"
  seed_pod_cidr "broadrangecluster" "10.44.0.0/17"
  clear_gke_node_tag_rules "broadrangecluster"
  # A rule whose sourceRanges is a broader range that happens to contain
  # the pod CIDR (10.0.0.0/8 covers 10.44.0.0/17) must not be accepted:
  # membership is exact-element equality, never CIDR containment, so a
  # user-created broad rule can never steer or ambiguate discovery.
  seed_gke_node_tag_rules "broadrangecluster" "$NETWORK" "10.0.0.0/8" "gke-broadrangecluster-x-node"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a broader range that merely contains the pod CIDR must not be accepted as a candidate"
  assert_contains "$RUN_OUTPUT" "10.0.0.0/8" "error should list the broader range as seen"
}

test_discover_same_pod_cidr_pair_on_other_network_ignored() {
  fresh_gcloud_state
  GKE_NAME="ownnetcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "ownnetcluster" "$NETWORK"
  seed_pod_cidr "ownnetcluster" "10.44.0.0/17"
  # A second, fully valid -all/-vms pair for the same pod CIDR, but on a
  # different network -- discovery's list call is unfiltered, so it
  # returns every seeded rule on every network, and this exercises
  # discovery's own network-URL check: without it, this decoy pair
  # would either be picked directly or make the run ambiguous.
  seed_gke_node_tag_rules "otherclusteronothernet" "other-vpc" "10.44.0.0/17" "gke-otherclusteronothernet-x-node"
  hybrid_discover "$NETWORK"
  assert_eq "gke-ownnetcluster-x-node" "$GKE_NODE_TAG" "the correct network's own tag must still be discovered"
}

# The network check matches the network URL's last path segment exactly:
# a pair on a network whose name ends with the cluster's network name, or
# starts with it, is on a different network and must be ignored. A
# suffix-only or substring check would pick one of these pairs up and
# make the run ambiguous.
test_discover_network_name_prefix_and_suffix_decoys_ignored() {
  fresh_gcloud_state
  GKE_NAME="exactnetcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "exactnetcluster" "$NETWORK"
  seed_pod_cidr "exactnetcluster" "10.44.0.0/17"
  seed_gke_node_tag_rules "suffixdecoy" "not-${NETWORK}" "10.44.0.0/17" "gke-suffixdecoy-x-node"
  seed_gke_node_tag_rules "prefixdecoy" "${NETWORK}-2" "10.44.0.0/17" "gke-prefixdecoy-x-node"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_eq "0" "$RUN_EXIT_CODE" \
    "pairs on networks named not-${NETWORK} and ${NETWORK}-2 must not count as on ${NETWORK}: ${RUN_OUTPUT}"
  hybrid_discover "$NETWORK"
  assert_eq "gke-exactnetcluster-x-node" "$GKE_NODE_TAG" \
    "only the pair on the cluster's own network may supply the tag"
}

# A rule's network may also be given in the relative form
# projects/<project>/global/networks/<network>. Discovery matches it on
# the same exact /networks/<network> suffix as the full URL form.
test_discover_relative_network_form() {
  fresh_gcloud_state
  GKE_NAME="relnetcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "relnetcluster" "$NETWORK"
  seed_pod_cidr "relnetcluster" "10.44.0.0/17"
  clear_gke_node_tag_rules "relnetcluster"
  seed_gke_node_tag_rules "relnetcluster" "$NETWORK" "10.44.0.0/17" "gke-relnetcluster-x-node" \
    "" "x" "INGRESS" "relative"
  local seeded_network
  seeded_network="$("$PYTHON" -c "import json,sys; print(json.load(open(sys.argv[1]))['network'])" \
    "${GCLOUD_STUB_STATE_DIR}/gke-node-firewall-rules/gke-relnetcluster-x-all.json")"
  assert_eq "projects/${PROJECT}/global/networks/${NETWORK}" "$seeded_network" \
    "the fixture carries the relative network form"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_eq "0" "$RUN_EXIT_CODE" \
    "a relative-form rule on the cluster's own network must be accepted: ${RUN_OUTPUT}"
  hybrid_discover "$NETWORK"
  assert_eq "gke-relnetcluster-x-node" "$GKE_NODE_TAG" \
    "a rule whose network is the relative form of the cluster's network supplies the tag"

  # The same pair, in the relative form, on another network only.
  clear_gke_node_tag_rules "relnetcluster"
  seed_gke_node_tag_rules "relnetcluster" "other-${NETWORK}" "10.44.0.0/17" "gke-relnetcluster-x-node" \
    "" "x" "INGRESS" "relative"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a relative-form rule on network other-${NETWORK} must not count as on ${NETWORK}"
  assert_contains "$RUN_OUTPUT" "found on network ${NETWORK})" \
    "the error says no candidate rule is on the cluster's network"
}

# Discovery lists every firewall rule in the project with no --filter and
# keeps the rules on its network itself. A --filter on the network field
# is easy to get wrong in a way no stub that ignores it would show: in
# gcloud's filter language ':' is a word match in which only a trailing
# '*' is a wildcard, so a pattern such as '*/NETWORK' matches no rule at
# all. The stub applies such a filter the way real gcloud does (see
# test_stub_firewall_list_network_filter_semantics), and this pins the
# exact call.
test_discover_firewall_list_call_is_unfiltered() {
  fresh_gcloud_state
  GKE_NAME="unfilteredcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "unfilteredcluster" "$NETWORK"
  hybrid_discover "$NETWORK"
  assert_eq "gke-unfilteredcluster-x-node" "$GKE_NODE_TAG" "discovery must find the tag"
  assert_eq "compute firewall-rules list --project=${PROJECT} --format=json" \
    "$(gcloud_log | grep '^compute firewall-rules list' || true)" \
    "discovery must make exactly one firewall-rules list call, in exactly this form, with no --filter"
}

# The stub prints a WARNING line on stderr for every list call. Discovery
# parses only the list's stdout, so the line does not reach the JSON.
test_discover_parses_firewall_list_with_stderr_warning() {
  fresh_gcloud_state
  GKE_NAME="stderrcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "stderrcluster" "$NETWORK"
  local stderr_file
  stderr_file="$(mktemp)"
  gcloud compute firewall-rules list --project="$PROJECT" --format=json >/dev/null 2>"$stderr_file"
  assert_contains "$(cat "$stderr_file")" "WARNING: " \
    "the stub's firewall-rules list prints a WARNING line on stderr"
  rm -f "$stderr_file"
  hybrid_discover "$NETWORK"
  assert_eq "gke-stderrcluster-x-node" "$GKE_NODE_TAG" \
    "discovery must find the tag with a WARNING line on the list's stderr"
}

# The hybrid tier's own rules are in the unfiltered list too, next to the
# GKE-managed ones; they must not disturb discovery.
test_discover_ignores_hybrid_tier_own_rules_in_unfiltered_list() {
  fresh_gcloud_state
  GKE_NAME="ownrulescluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "ownrulescluster" "$NETWORK"
  seed_firewall_rule_json "scion-hub-${HUB}-hub-deny" "scion-deployment=${HUB}" \
    "$NETWORK" "INGRESS" "DENY" "all" "" "" "10.52.0.0/14" "scion-hub-${HUB}" "950"
  hybrid_discover "$NETWORK"
  assert_eq "gke-ownrulescluster-x-node" "$GKE_NODE_TAG" \
    "a non-GKE rule on the same network and pod range must not affect discovery"
}

# The stub's network --filter emulation reproduces what real gcloud
# returns for these forms: the leading-glob ':' form matches nothing,
# while the plain word form and the anchored regex form match exactly the
# rules on that network.
test_stub_firewall_list_network_filter_semantics() {
  fresh_gcloud_state
  seed_gke_node_tag_rules "filtcluster" "$NETWORK" "10.44.0.0/17" "gke-filtcluster-x-node"
  seed_gke_node_tag_rules "filtother" "other-vpc" "10.45.0.0/17" "gke-filtother-x-node"
  _fw_list_count() {
    gcloud compute firewall-rules list --project="$PROJECT" "$@" --format=json \
      | "$PYTHON" -c 'import json, sys; print(len(json.load(sys.stdin)))'
  }
  assert_eq "4" "$(_fw_list_count)" "the unfiltered list returns every rule, on every network"
  assert_eq "0" "$(_fw_list_count --filter="network:*/${NETWORK}")" \
    "a leading-glob ':' pattern matches nothing"
  assert_eq "2" "$(_fw_list_count --filter="network:${NETWORK}")" \
    "the plain ':' word form matches the rules on that network"
  assert_eq "2" "$(_fw_list_count --filter="network~/networks/${NETWORK}\$")" \
    "the anchored regex form matches the rules on that network"
}

test_discover_non_default_network() {
  fresh_gcloud_state
  GKE_NAME="othernetcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "othernetcluster" "other-vpc"
  seed_pod_cidr "othernetcluster" "10.44.0.0/17"
  # Discovery must work on a network other than "default": nothing in the
  # Python selector may hard-code that name.
  hybrid_discover "other-vpc"
  assert_eq "gke-othernetcluster-x-node" "$GKE_NODE_TAG" "the tag should still be discovered correctly"
}

test_discover_firewall_list_failure_refused() {
  fresh_gcloud_state
  GKE_NAME="listfailcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "listfailcluster" "$NETWORK"
  set_firewall_list_will_fail
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a firewall-rules list failure must fail the run, not be treated as no rules found"
  assert_contains "$RUN_OUTPUT" "Could not list firewall rules" "error should say the list call itself failed"
}

test_discover_firewall_list_empty_stdout_treated_as_no_rules() {
  fresh_gcloud_state
  GKE_NAME="emptystdoutcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "emptystdoutcluster" "$NETWORK"
  clear_gke_node_tag_rules "emptystdoutcluster"
  # A successful call that prints nothing at all, distinct from the
  # stub's normal empty-glob output of a genuine "[]": json.load on zero
  # bytes raises, so this pins the code's own empty-stdout-as-[] handling
  # rather than a crash surfacing as an uncaught traceback.
  set_gke_node_tag_firewall_list_empty_stdout
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "empty stdout from the list call must fail cleanly, not crash on an empty rule list"
  assert_contains "$RUN_OUTPUT" "no firewall rule matching" "error should give the no-candidate reason, not a traceback"
}

test_discover_network_mismatch_refused() {
  fresh_gcloud_state
  GKE_NAME="netmismatch"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "netmismatch" "other-vpc"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "network mismatch must fail the run"
  assert_contains "$RUN_OUTPUT" "other-vpc" "error should name the cluster's actual network"
}

test_discover_cluster_not_found_refused() {
  fresh_gcloud_state
  GKE_NAME="ghostcluster"
  GKE_PROJECT="$PROJECT"
  # shellcheck disable=SC2034 # read by hybrid_discover (hybrid-tier.sh)
  GKE_LOCATION="us-central1"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unfound cluster must fail the run"
  assert_eq "0" "$(gcloud_log | grep -c 'firewall-rules create' || true)" "nothing should be created after a failed discovery"
}

# --- Node subnet discovery (the NFS export's client CIDR) -----------------

test_discover_node_subnet_cidr_from_explicit_fixture() {
  fresh_gcloud_state
  GKE_NAME="subnetcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "subnetcluster" "$NETWORK"
  seed_subnet "default-subnet" "10.4.0.0/22"
  hybrid_discover "$NETWORK"
  assert_eq "10.4.0.0/22" "$GKE_NODE_SUBNET_CIDR" "should discover the subnet's actual primary CIDR, not a hardcoded default"
}

test_discover_node_subnet_region_from_zonal_location() {
  fresh_gcloud_state
  GKE_NAME="zonalcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1-a"
  seed_cluster "zonalcluster" "$NETWORK"
  hybrid_discover "$NETWORK"
  assert_eq "1" "$(gcloud_log | grep -c 'networks subnets describe default-subnet --region=us-central1 ' || true)" \
    "a zonal location (us-central1-a) must resolve to its region (us-central1) for the subnet lookup, not be passed through as-is"
}

test_discover_node_subnet_describe_failure_refused() {
  fresh_gcloud_state
  GKE_NAME="subnetfailcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "subnetfailcluster" "$NETWORK"
  set_subnet_describe_will_fail "default-subnet"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unreadable node subnet must fail discovery"
  assert_contains "$RUN_OUTPUT" "default-subnet" "error should name the subnet"
}

test_discover_node_subnet_refuses_zero_slash_zero() {
  fresh_gcloud_state
  GKE_NAME="wideopencluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "wideopencluster" "$NETWORK"
  seed_subnet "default-subnet" "0.0.0.0/0"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "0.0.0.0/0 must never be accepted as an NFS export client range"
}

test_validate_node_subnet_cidr_refuses_host_bits_set() {
  # ipaddress.ip_network(..., strict=True) is what actually rejects this;
  # a host address masquerading as a network (10.0.0.1/8 instead of
  # 10.0.0.0/8) must never be silently normalized and accepted -- the
  # export client list must be exactly the network the operator/GKE
  # reported, not whatever it happens to round down to.
  run_expect_fail _hybrid_validate_node_subnet_cidr "10.0.0.1/8"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a CIDR with host bits set must be refused, not silently accepted as strict"
}

test_validate_node_subnet_cidr_accepts_clean_network() {
  assert_true "$(_hybrid_validate_node_subnet_cidr "10.0.0.0/8" && echo true || echo false)" \
    "a syntactically clean /8 network must be accepted"
}

test_discover_node_subnet_refuses_broader_than_slash_8() {
  fresh_gcloud_state
  GKE_NAME="toobroadcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "toobroadcluster" "$NETWORK"
  seed_subnet "default-subnet" "10.0.0.0/7"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "anything broader than /8 must be refused"
}

test_discover_node_subnet_accepts_slash_8_boundary() {
  fresh_gcloud_state
  GKE_NAME="boundarycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "boundarycluster" "$NETWORK"
  seed_subnet "default-subnet" "10.0.0.0/8"
  hybrid_discover "$NETWORK"
  assert_eq "10.0.0.0/8" "$GKE_NODE_SUBNET_CIDR" "/8 itself is the narrowest allowed refusal boundary, so it must be accepted"
}

# =====================================================================
# NFS export: the fsid and export-line renderers are pure functions
# (no gcloud or SSH calls), directly unit-testable.
# =====================================================================

test_nfs_export_line_renders_expected_options() {
  local line
  line="$(hybrid_nfs_export_line "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123")"
  assert_eq "/srv/scion-shared 10.128.0.0/20(rw,sync,no_subtree_check,all_squash,anonuid=6001,anongid=6000,sec=sys,mp,fsid=abc123)" \
    "$line" "the export line must render every required option in the expected shape"
}

test_nfs_export_line_uses_given_cidr_not_hardcoded() {
  local line
  line="$(hybrid_nfs_export_line "/srv/scion-shared" "10.4.0.0/22" "6001" "6000" "abc123")"
  assert_contains "$line" "10.4.0.0/22" "the export line must use the actual discovered CIDR"
}

test_nfs_export_line_anonuid_and_anongid_are_distinct_values() {
  local line
  line="$(hybrid_nfs_export_line "/srv/scion-shared" "10.128.0.0/20" "6001" "1000" "abc123")"
  assert_contains "$line" "anonuid=6001" "anonuid must be the squash uid, not the scion gid"
  assert_contains "$line" "anongid=1000" "anongid must be the scion group's gid -- there is no separate squash group"
}

test_nfs_export_line_has_mp_option() {
  local line
  line="$(hybrid_nfs_export_line "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123")"
  assert_contains "$line" ",mp," \
    "the mp option is the export-side fail-closed guarantee: knfsd itself refuses to serve the path unless it's a mountpoint"
}

test_nfs_fsid_deterministic_for_same_hub() {
  local first second
  first="$(hybrid_nfs_fsid "demohub")"
  second="$(hybrid_nfs_fsid "demohub")"
  assert_eq "$first" "$second" "the fsid must be stable across calls (and so across re-runs) for the same hub"
}

test_nfs_fsid_differs_per_hub() {
  local hub1 hub2
  hub1="$(hybrid_nfs_fsid "hub-one")"
  hub2="$(hybrid_nfs_fsid "hub-two")"
  assert_true "$([[ "$hub1" != "$hub2" ]] && echo true || echo false)" \
    "different hubs must never collide on the same fsid"
}

test_nfs_fsid_is_a_well_formed_uuid() {
  local fsid
  fsid="$(hybrid_nfs_fsid "demohub")"
  assert_true "$([[ "$fsid" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] && echo true || echo false)" \
    "the fsid must be a well-formed UUID, which nfs-utils accepts for fsid="
}

# =====================================================================
# NFS squash identity and export remote scripts, and the Cloud Run
# label decision: the parts of the tier-gated remote step that would
# otherwise be unreachable from the wiring tests (they run after the
# point the create-mode sentinel stops). Rendering them as pure
# functions (identity/export scripts) or gcloud-stub-testable functions
# (the Cloud Run decision) closes that gap.
# =====================================================================

test_nfs_squash_identity_script_has_set_euo_pipefail() {
  local script
  script="$(hybrid_nfs_squash_identity_script "scion-nfs")"
  assert_eq "set -euo pipefail" "$(echo "$script" | head -1)" \
    "the remote script must fail closed on any unexpected error, not silently continue"
}

test_nfs_squash_identity_script_useradd_guarded_by_exists() {
  local script
  script="$(hybrid_nfs_squash_identity_script "scion-nfs")"
  assert_contains "$script" "id scion-nfs >/dev/null 2>&1 ||" \
    "useradd must only run when the identity doesn't already exist"
  assert_contains "$script" "useradd -r -M -N -g scion -s /usr/sbin/nologin scion-nfs" \
    "useradd must create a system account, no home, no user-private group, primary group scion, no login shell"
}

test_nfs_squash_identity_script_reads_ids() {
  local script
  script="$(hybrid_nfs_squash_identity_script "scion-nfs")"
  # shellcheck disable=SC2016 # asserting the literal remote-script text, not expanding locally
  assert_contains "$script" 'SQUASH_UID=$(id -u scion-nfs)' "must read the squash uid back numerically"
  # shellcheck disable=SC2016 # asserting the literal remote-script text, not expanding locally
  assert_contains "$script" 'SCION_UID=$(id -u scion)' "must read the broker's own uid for the comparison"
  # shellcheck disable=SC2016 # asserting the literal remote-script text, not expanding locally
  assert_contains "$script" 'SCION_GID=$(getent group scion | cut -d: -f3)' \
    "must read the scion group's gid for anongid, not assume it matches the user's own gid"
}

test_nfs_squash_identity_script_asserts_uid_mismatch() {
  local script
  script="$(hybrid_nfs_squash_identity_script "scion-nfs")"
  # shellcheck disable=SC2016 # asserting the literal remote-script text, not expanding locally
  assert_contains "$script" 'if [ "$SQUASH_UID" = "$SCION_UID" ]; then' \
    "the mismatch check must run unconditionally, not just log a warning"
  assert_contains "$script" "The NFS squash uid must not equal the scion (broker) uid." \
    "the failure message must explain why"
  assert_contains "$script" $'  exit 1\nfi' "an equal uid must exit non-zero, not just warn and continue"
}

# Executes the rendered squash-identity script end to end against fake
# id/getent/useradd/awk/sudo binaries, proving the validation logic
# actually holds at runtime -- rendered-text assertions alone can't prove
# a fresh useradd call, or a pre-existing account with a bad property,
# actually produces the exit code and stdout the caller depends on.
# SYS_UID_MAX is faked too (via awk, which the script uses only to read
# it from /etc/login.defs), since the real file's contents in whatever
# environment runs this suite are not something a test should depend on.
_setup_squash_script_fakebins() {
  local dir="$1" squash_uid="$2" squash_group="${3:-scion}" squash_shell="${4:-/usr/sbin/nologin}" \
    scion_uid="${5:-1000}" scion_gid="${6:-1001}" sys_uid_max="${7:-999}" account_exists="${8:-false}"
  mkdir -p "$dir"
  cat > "$dir/id" <<IDEOF
#!/bin/bash
echo "\$*" >> "${dir}/id.log"
case "\$1" in
  -u) [ "\$2" = "scion" ] && echo "$scion_uid" || echo "$squash_uid"; exit 0 ;;
  -gn) echo "$squash_group"; exit 0 ;;
  *) [ "$account_exists" = "true" ] && exit 0 || exit 1 ;;
esac
IDEOF
  cat > "$dir/getent" <<GETENTEOF
#!/bin/bash
if [ "\$1" = "passwd" ]; then
  echo "\$2:x:$squash_uid:$scion_gid::/nonexistent:$squash_shell"
elif [ "\$1" = "group" ]; then
  echo "\$2:x:$scion_gid:"
fi
GETENTEOF
  cat > "$dir/useradd" <<'USERADDEOF'
#!/bin/bash
echo "$*" >> "$(dirname "$0")/useradd.log"
exit 0
USERADDEOF
  cat > "$dir/awk" <<AWKEOF
#!/bin/bash
echo "$sys_uid_max"
AWKEOF
  cat > "$dir/sudo" <<'SUDOEOF'
#!/bin/bash
"$@"
SUDOEOF
  chmod +x "$dir"/id "$dir"/getent "$dir"/useradd "$dir"/awk "$dir"/sudo
}

test_probe_squash_script_executed_fresh_account_succeeds() {
  local d script out rc
  d="$(mktemp -d)"
  _setup_squash_script_fakebins "$d" "900" "scion" "/usr/sbin/nologin" "1000" "1001" "999" "false"
  script="$(hybrid_nfs_squash_identity_script "scion-nfs")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_eq "0" "$rc" "a fresh, valid identity must succeed"
  assert_eq "900:1001" "$out" "must print SQUASH_UID:SCION_GID"
  assert_eq "1" "$(wc -l < "${d}/useradd.log" 2>/dev/null || echo 0)" \
    "useradd must actually run when the account doesn't exist yet"
  rm -rf "$d"
}

test_probe_squash_script_executed_preexisting_account_not_recreated() {
  local d script out rc
  d="$(mktemp -d)"
  _setup_squash_script_fakebins "$d" "900" "scion" "/usr/sbin/nologin" "1000" "1001" "999" "true"
  script="$(hybrid_nfs_squash_identity_script "scion-nfs")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_eq "0" "$rc" "a valid pre-existing identity must succeed"
  assert_eq "900:1001" "$out" "must print SQUASH_UID:SCION_GID"
  assert_false "$([[ -f "${d}/useradd.log" ]] && echo true)" \
    "useradd must never run when the account already exists"
  rm -rf "$d"
}

test_probe_squash_script_executed_refuses_uid_zero() {
  local d script out rc
  d="$(mktemp -d)"
  _setup_squash_script_fakebins "$d" "0" "scion" "/usr/sbin/nologin" "1000" "1001" "999" "true"
  script="$(hybrid_nfs_squash_identity_script "scion-nfs")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" "uid 0 must be refused"
  assert_contains "$out" "must not be uid 0" "error should explain why"
  rm -rf "$d"
}

test_probe_squash_script_executed_refuses_uid_above_sys_uid_max() {
  local d script out rc
  d="$(mktemp -d)"
  _setup_squash_script_fakebins "$d" "1500" "scion" "/usr/sbin/nologin" "1000" "1001" "999" "true"
  script="$(hybrid_nfs_squash_identity_script "scion-nfs")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" "a uid above SYS_UID_MAX must be refused"
  assert_contains "$out" "must be a system uid" "error should explain why"
  rm -rf "$d"
}

test_probe_squash_script_executed_refuses_wrong_group() {
  local d script out rc
  d="$(mktemp -d)"
  _setup_squash_script_fakebins "$d" "900" "nogroup" "/usr/sbin/nologin" "1000" "1001" "999" "true"
  script="$(hybrid_nfs_squash_identity_script "scion-nfs")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" "a non-scion primary group must be refused"
  assert_contains "$out" "primary group must be scion" "error should explain why"
  rm -rf "$d"
}

test_probe_squash_script_executed_refuses_wrong_shell() {
  local d script out rc
  d="$(mktemp -d)"
  _setup_squash_script_fakebins "$d" "900" "scion" "/bin/bash" "1000" "1001" "999" "true"
  script="$(hybrid_nfs_squash_identity_script "scion-nfs")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" "a non-nologin shell must be refused"
  assert_contains "$out" "login shell must be" "error should explain why"
  rm -rf "$d"
}

test_probe_squash_script_executed_refuses_uid_equal_to_scion_uid() {
  local d script out rc
  d="$(mktemp -d)"
  _setup_squash_script_fakebins "$d" "900" "scion" "/usr/sbin/nologin" "900" "1001" "999" "true"
  script="$(hybrid_nfs_squash_identity_script "scion-nfs")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" "a squash uid equal to the broker's own uid must be refused"
  assert_contains "$out" "must not equal the scion (broker) uid" "error should explain why"
  rm -rf "$d"
}

test_nfs_export_script_has_set_euo_pipefail() {
  local script
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_eq "set -euo pipefail" "$(echo "$script" | head -1)" \
    "the remote script must fail closed on any unexpected error"
}

test_nfs_export_script_root_owner_and_mode() {
  local script
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_contains "$script" "sudo mkdir -p /srv/scion-shared" "must create the export root"
  assert_contains "$script" "sudo chown scion:scion /srv/scion-shared" "the export root must be owned scion:scion"
  assert_contains "$script" "sudo chmod 2755 /srv/scion-shared" \
    "mode 2755 is what keeps the squash uid from writing the export root"
}

test_nfs_export_script_writes_exports_file_using_the_render_function() {
  local script expected_line
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  expected_line="$(hybrid_nfs_export_line "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123")"
  assert_contains "$script" "echo '${expected_line}' | sudo tee /etc/exports.d/scion-hub-demohub.exports" \
    "the exports file must be this hub's own file, containing exactly hybrid_nfs_export_line's output"
}

test_nfs_export_script_exportfs_and_enable_service() {
  local script
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_contains "$script" "sudo exportfs -ra" "must re-export after writing the file"
  assert_contains "$script" "sudo systemctl enable nfs-server" \
    "must enable the service under its canonical unit name, not the Debian package name"
  assert_contains "$script" "sudo systemctl restart nfs-server" \
    "must restart (not just enable --now), since apt's postinst already started the server with the stock config before this script's own /etc/nfs.conf.d write; enable --now alone is a no-op on an already-running unit"
}

test_nfs_export_script_installs_server_package_if_needed() {
  local script
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_contains "$script" "if ! dpkg -s nfs-kernel-server >/dev/null 2>&1; then" \
    "the install must be guarded, not run unconditionally on every re-run"
  assert_contains "$script" "apt-get install -y nfs-kernel-server" "must actually install the package"
}

test_nfs_export_script_creates_exports_d_directory() {
  local script
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_contains "$script" "sudo install -d -m 0755 /etc/exports.d" \
    "/etc/exports.d does not exist on a stock jammy install; it must be created before the exports file is written into it"
}

test_nfs_export_script_v4_only_hardening() {
  local script
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_contains "$script" "/etc/nfs.conf.d/scion-hub.conf" "must write an NFS server config drop-in"
  assert_contains "$script" "vers2=n" "NFSv2 must be disabled"
  assert_contains "$script" "vers3=n" "NFSv3 must be disabled"
  assert_contains "$script" "vers4.0=n" "NFSv4.0 must be disabled -- the PV pins nfsvers=4.1"
  assert_contains "$script" "udp=n" "UDP must be disabled"
  assert_contains "$script" "sudo systemctl mask --now rpcbind.service rpcbind.socket" \
    "rpcbind is unneeded once v2/v3 are off and must be masked"
  # shellcheck disable=SC2016 # asserting the literal remote-script text, not expanding locally
  assert_contains "$script" '[ "$(systemctl is-enabled rpcbind.socket 2>/dev/null || true)" = masked ]' \
    "the mask must be verified without a cmd-under-pipefail | grep -q that fails even on a genuine match, since \`systemctl is-enabled\` itself exits non-zero for a masked unit"
  assert_not_contains "$script" 'mask --now rpcbind.service rpcbind.socket || true' \
    "the mask's own exit status must not be discarded"
}

test_nfs_export_script_image_created_once_never_remkfs() {
  local script
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_contains "$script" "if [ ! -e ${HYBRID_NFS_IMAGE_PATH} ]; then" \
    "the image must only be created (and mkfs'd) the first time -- an existing image is never re-created"
  assert_contains "$script" "sudo fallocate -l 20G ${HYBRID_NFS_IMAGE_PATH}" "must size the image from the IMAGE_SIZE_GB argument"
  assert_contains "$script" "sudo mkfs.ext4 -F -q ${HYBRID_NFS_IMAGE_PATH}" "must format the image ext4"
}

test_nfs_export_script_fallocate_failure_removes_partial_image_and_exits() {
  local script fallocate_line rm_line exit_line
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_contains "$script" "sudo fallocate -l 20G ${HYBRID_NFS_IMAGE_PATH} || {" \
    "a fallocate failure (insufficient disk) must be caught, not left to a bare set -e exit with no explanation"
  fallocate_line="$(echo "$script" | grep -n 'sudo fallocate' | head -1 | cut -d: -f1)"
  rm_line="$(echo "$script" | grep -n "sudo rm -f ${HYBRID_NFS_IMAGE_PATH}" | head -1 | cut -d: -f1)"
  assert_true "$([[ -n "$rm_line" && "$rm_line" -eq "$fallocate_line" ]] && echo true || echo false)" \
    "on failure the partial image must be removed in the same statement, so a retry's [ ! -e ] guard doesn't see a half-allocated file and skip mkfs"
  assert_contains "$script" "insufficient disk space" "the failure message must say why, not just fail silently"
}

test_nfs_export_script_mounts_before_exporting() {
  local script
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_contains "$script" "x-systemd.before=nfs-server.service" \
    "the fstab entry must order the mount before the NFS server unit starts"
  assert_contains "$script" "x-systemd.required-by=nfs-server.service" \
    "before= alone only orders the units; required-by= is what makes nfs-server actually depend on the mount, so it won't start if the mount fails"
  assert_contains "$script" "if ! mountpoint -q /srv/scion-shared; then" "must check whether the export root is already mounted"
  assert_contains "$script" "sudo mount /srv/scion-shared" "must mount the export filesystem"
}

test_nfs_export_script_stale_fstab_line_fails_instead_of_silently_trusting_it() {
  local script
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_contains "$script" "grep -qxF \"${HYBRID_NFS_IMAGE_PATH} /srv/scion-shared ext4 loop,nofail" \
    "must check for an exact, whole-line match of the expected fstab entry"
  assert_contains "$script" "does not match the expected entry; refusing to continue" \
    "a pre-existing line for the export root that doesn't match must fail with a message, never be silently trusted or silently replaced"
  assert_contains "$script" "Expected: ${HYBRID_NFS_IMAGE_PATH} /srv/scion-shared ext4 loop,nofail" \
    "the failure message must show the operator the exact line it expected"
}

test_nfs_export_script_verifies_mount_source_is_the_image_loop_device() {
  local script
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_contains "$script" "findmnt -n -o SOURCE --mountpoint /srv/scion-shared" \
    "must look up what's actually mounted at the export root, not just that something is"
  assert_contains "$script" "losetup -n -O BACK-FILE" \
    "must resolve the mount source's backing file through losetup, since the mount source is a loop device, not the image path itself"
  assert_contains "$script" "not from the loop device backing ${HYBRID_NFS_IMAGE_PATH}" \
    "must refuse a stray tmpfs/bind mount masquerading as the export filesystem"
}

test_nfs_export_script_hub_service_requires_mounts_for_export_root() {
  local script
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  assert_contains "$script" "/etc/systemd/system/scion-hub.service.d/10-scion-shared.conf" \
    "a tier-on drop-in must make scion-hub.service depend on the export mount"
  assert_contains "$script" "RequiresMountsFor=/srv/scion-shared" \
    "the hub must never start against an unmounted export and write shared-dir paths to the boot disk's root filesystem instead"
  local dropin_line reload_line
  dropin_line="$(echo "$script" | grep -n '10-scion-shared.conf' | tail -1 | cut -d: -f1)"
  reload_line="$(echo "$script" | grep -n 'sudo systemctl daemon-reload' | tail -1 | cut -d: -f1)"
  assert_true "$([[ -n "$reload_line" && "$reload_line" -gt "$dropin_line" ]] && echo true || echo false)" \
    "daemon-reload must run after the drop-in is written, so systemd actually picks it up"
}

test_nfs_export_script_fstab_line_nofail_and_no_fsck() {
  local script fstab_line
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  fstab_line="$(echo "$script" | grep "${HYBRID_NFS_IMAGE_PATH} /srv/scion-shared ext4")"
  assert_contains "$fstab_line" "loop,nofail,x-systemd.before=nfs-server.service,x-systemd.required-by=nfs-server.service" \
    "nofail must be present alongside the loop and before=/required-by= options -- it protects boot from a scratchpad-mount failure without touching the separate required-by=nfs-server.service dependency"
  assert_contains "$fstab_line" " 0 0" \
    "the fsck pass must be 0 -- systemd-fstab-generator never schedules fsck for a loop-mounted regular file, so a nonzero pass is a no-op boot-time warning"
}

test_nfs_export_script_fails_closed_when_not_mounted() {
  local script mount_check_line exit_line
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$HYBRID_NFS_IMAGE_PATH" "20")"
  # There must be THREE mountpoint checks: the pre-fallocate existing-
  # mount refusal, the before-mount check, and the fail-closed guard
  # after the mount attempt -- whose failure branch exits non-zero
  # before the export root is ever chowned/chmoded or the exports file
  # written.
  assert_eq "3" "$(echo "$script" | grep -c 'mountpoint -q /srv/scion-shared')" \
    "there must be the pre-fallocate refusal check, a before-mount check, and a fail-closed check after attempting to mount"
  mount_check_line="$(echo "$script" | grep -n 'mountpoint -q /srv/scion-shared' | tail -1 | cut -d: -f1)"
  exit_line="$(echo "$script" | grep -n 'is not a mountpoint after attempting to mount' | head -1 | cut -d: -f1)"
  assert_true "$([[ -n "$exit_line" && "$exit_line" -gt "$mount_check_line" ]] && echo true || echo false)" \
    "the fail-closed exit must come after the last (post-mount) mountpoint check"
  local chown_line
  chown_line="$(echo "$script" | grep -n 'sudo chown scion:scion /srv/scion-shared' | head -1 | cut -d: -f1)"
  assert_true "$([[ "$exit_line" -lt "$chown_line" ]] && echo true || echo false)" \
    "the fail-closed exit must come before anything is chowned/exported"
}

# Executes the rendered export script end to end against fake system
# binaries, proving the create-once and fail-closed behaviors actually
# hold at runtime, not just in the rendered text -- text alone can't
# distinguish "creates once" from "always creates" without running it
# twice, and can't distinguish a fail-closed guard that actually stops
# the script from one that's dead code after an early `set -e` exit
# elsewhere. Every command the script invokes that could otherwise touch
# a REAL absolute path on the machine running the tests (mkdir, install,
# mkfs.ext4, fallocate, mount, mountpoint, systemctl, dpkg, apt-get,
# exportfs, chown, chmod, tee, findmnt, losetup, and sudo itself) is
# faked and put first on PATH. `grep` and `awk` are real (the script's
# own FSTAB_PATH parameter, not a PATH trick, is what points its fstab
# reads at a per-test fixture file instead of the real /etc/fstab --
# see hybrid_nfs_export_script's own fstab_path parameter). `cat`
# delegates to the real binary except for a read of the literal path
# /etc/nfs.conf.d/scion-hub.conf, which is redirected to a per-test
# fixture file (dir/.nfs-conf-existing, absent by default) so a test can
# exercise the conditional-restart logic without touching the real
# machine's file.
# MOUNTED selects the `mountpoint`/`mount` fakes' behavior, tracked via
# a marker file (dir/.mounted) rather than a fixed answer, so the three
# checks the rendered script makes can behave differently within one run:
#   true    -- the marker pre-exists: mountpoint succeeds from the very
#              first check, so `mount` must never be called at all.
#   false   -- no marker, and `mount` never creates one: mountpoint
#              fails every time, exercising the fail-closed exit.
#   becomes -- no marker at start, but `mount` creates one when run: the
#              first check fails, `mount` runs exactly once, and the
#              fail-closed check then succeeds.
# `findmnt`/`losetup` are wired to report IMAGE_PATH as the mount's
# backing file, so the new mount-source verification passes for every
# existing test that doesn't care about it -- MOUNT_BACK_OVERRIDE lets a
# test point that verification at a *different* path, to exercise its
# failure mode.
_setup_export_script_fakebins() {
  local dir="$1" mounted="$2"
  local image_path="${3:-${dir}/export.img}" mount_back_override="${4:-}"
  local back="${mount_back_override:-$image_path}"
  mkdir -p "$dir"
  rm -f "$dir/.mounted"
  printf '#!/bin/bash\necho "$*" >> "%s/mkfs.log"\n' "$dir" > "$dir/mkfs.ext4"
  # Real fallocate's last argument is the target file; touch it so a
  # second run's "does the image already exist" check sees it.
  # shellcheck disable=SC2016 # writing a literal fake-binary script body, not expanding now
  printf '#!/bin/bash\necho "$*" >> "%s/fallocate.log"\ntouch "${@: -1}"\n' "$dir" > "$dir/fallocate"
  case "$mounted" in
    true) touch "$dir/.mounted" ;;
  esac
  printf '#!/bin/bash\necho "$*" >> "%s/mountpoint.log"\n[ -e "%s/.mounted" ] && exit 0 || exit 1\n' "$dir" "$dir" > "$dir/mountpoint"
  if [ "$mounted" = "becomes" ]; then
    printf '#!/bin/bash\necho "$*" >> "%s/mount.log"\ntouch "%s/.mounted"\nexit 0\n' "$dir" "$dir" > "$dir/mount"
  else
    printf '#!/bin/bash\necho "$*" >> "%s/mount.log"\nexit 0\n' "$dir" > "$dir/mount"
  fi
  printf '#!/bin/bash\nexit 0\n' > "$dir/mkdir"
  printf '#!/bin/bash\nexit 0\n' > "$dir/install"
  printf '#!/bin/bash\nexit 0\n' > "$dir/dpkg"
  printf '#!/bin/bash\nexit 0\n' > "$dir/apt-get"
  # Realistic systemctl fake, not a blanket exit-0: `mask --now UNIT...`
  # records each unit as masked (unless MASK_FAILS below asks it not to,
  # for the "mask silently didn't take" test); `is-enabled UNIT` then
  # answers the way the real command does -- prints "masked" and exits 1
  # for a masked unit (the exact behavior that broke the old
  # `| grep -q masked` check under pipefail), "enabled" and exits 0 for a
  # unit marked enabled, or a "not found"-shaped error on stderr and a
  # non-zero exit for anything else. Every other subcommand (daemon-
  # reload, enable, restart) just succeeds, matching the rest of this
  # fakebin set.
  # shellcheck disable=SC2016 # writing a literal fake-binary script body, not expanding now
  printf '%s\n' \
    '#!/bin/bash' \
    "echo \"\$*\" >> \"${dir}/systemctl.log\"" \
    'if [ "$1" = "mask" ]; then' \
    '  shift' \
    '  [ "${1:-}" = "--now" ] && shift' \
    "  if [ ! -f \"${dir}/.systemctl-mask-fails\" ]; then" \
    "    for u in \"\$@\"; do touch \"${dir}/.masked-\${u}\"; done" \
    '  fi' \
    '  exit 0' \
    'fi' \
    'if [ "$1" = "is-enabled" ]; then' \
    '  u="$2"' \
    "  if [ -f \"${dir}/.masked-\${u}\" ]; then echo masked; exit 1; fi" \
    "  if [ -f \"${dir}/.enabled-\${u}\" ]; then echo enabled; exit 0; fi" \
    '  echo "Failed to get unit file state for ${u}: No such file or directory" >&2' \
    '  exit 1' \
    'fi' \
    'if [ "$1" = "is-active" ]; then' \
    "  [ -f \"${dir}/.nfs-server-active\" ] && exit 0 || exit 3" \
    'fi' \
    'exit 0' \
    > "$dir/systemctl"
  printf '#!/bin/bash\necho "$*" >> "%s/exportfs.log"\nexit 0\n' "$dir" > "$dir/exportfs"
  printf '#!/bin/bash\nexit 0\n' > "$dir/chown"
  printf '#!/bin/bash\nexit 0\n' > "$dir/chmod"
  printf '#!/bin/bash\n"$@"\n' > "$dir/sudo"
  printf '#!/bin/bash\ncat >> "%s/tee.log"\n' "$dir" > "$dir/tee"
  printf '#!/bin/bash\necho "/dev/loop0"\n' > "$dir/findmnt"
  printf '#!/bin/bash\necho "%s"\n' "$back" > "$dir/losetup"
  # Delegates to the real `cat` for everything except a read of
  # /etc/nfs.conf.d/scion-hub.conf, redirected to a per-test fixture
  # (absent by default, so every existing test -- which never seeds
  # one -- sees "no prior config", matching this fake's own default of
  # "restart always needed"), and a read of /proc/fs/nfsd/versions,
  # defaulting to a fixture with v2/v3/v4.0 already disabled (the state
  # this script's own config produces), so a test that doesn't care
  # about this check sees "no restart needed" from it.
  # shellcheck disable=SC2016 # writing a literal fake-binary script body, not expanding now
  printf '%s\n' \
    '#!/bin/bash' \
    'if [ "$#" -eq 1 ] && [ "$1" = "/etc/nfs.conf.d/scion-hub.conf" ]; then' \
    "  if [ -f \"${dir}/.nfs-conf-existing\" ]; then" \
    "    exec /bin/cat \"${dir}/.nfs-conf-existing\"" \
    '  else' \
    '    exit 1' \
    '  fi' \
    'fi' \
    'if [ "$#" -eq 1 ] && [ "$1" = "/proc/fs/nfsd/versions" ]; then' \
    "  if [ -f \"${dir}/.nfsd-versions-unreadable\" ]; then" \
    '    exit 1' \
    "  elif [ -f \"${dir}/.nfsd-versions\" ]; then" \
    "    exec /bin/cat \"${dir}/.nfsd-versions\"" \
    '  else' \
    '    echo "-2 -3 +4 -4.0 +4.1"' \
    '    exit 0' \
    '  fi' \
    'fi' \
    'exec /bin/cat "$@"' \
    > "$dir/cat"
  chmod +x "$dir"/mkfs.ext4 "$dir"/fallocate "$dir"/mountpoint "$dir"/mount "$dir"/mkdir "$dir"/install \
    "$dir"/dpkg "$dir"/apt-get "$dir"/systemctl "$dir"/exportfs "$dir"/chown "$dir"/chmod "$dir"/sudo "$dir"/tee \
    "$dir"/findmnt "$dir"/losetup "$dir"/cat
}

# set_export_script_nfs_conf_existing DIR CONTENT — seeds the fake `cat`
# (set up by _setup_export_script_fakebins above) to answer CONTENT for
# a read of /etc/nfs.conf.d/scion-hub.conf, simulating a prior run
# having already written it.
set_export_script_nfs_conf_existing() {
  local dir="$1" content="$2"
  printf '%s' "$content" > "${dir}/.nfs-conf-existing"
}

# set_export_script_nfs_server_active DIR — the fake systemctl (set up
# by _setup_export_script_fakebins above) answers `is-active nfs-server`
# as active. Without this, it answers inactive, matching a fresh
# install where the server was never started by this script.
set_export_script_nfs_server_active() {
  touch "${1}/.nfs-server-active"
}

# set_export_script_nfsd_versions DIR CONTENT — seeds the fake `cat`'s
# answer for a read of /proc/fs/nfsd/versions, in the space-separated
# +/-prefixed shape the kernel actually renders it.
set_export_script_nfsd_versions() {
  printf '%s' "$2" > "${1}/.nfsd-versions"
}

# set_export_script_nfsd_versions_unreadable DIR — the fake `cat` fails
# reading /proc/fs/nfsd/versions, as if the nfsd kernel module weren't
# loaded.
set_export_script_nfsd_versions_unreadable() {
  touch "${1}/.nfsd-versions-unreadable"
}

test_probe_export_script_executed_fails_closed_when_never_mounts() {
  local d out rc script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "false" "$image_path"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" "must exit non-zero when the export root never becomes a mountpoint"
  assert_contains "$out" "is not a mountpoint" "must explain why it refused"
  assert_false "$([[ -f "${d}/tee.log" ]] && grep -q "scion-hub-demohub.exports" "${d}/tee.log" 2>/dev/null && echo true)" \
    "must never write the exports file when the export root isn't actually mounted"
  assert_false "$([[ -f "${d}/exportfs.log" ]] && echo true)" \
    "must never call exportfs at all when the export root isn't actually mounted"
  rm -rf "$d"
}

test_probe_export_script_executed_refuses_existing_mount_from_other_source_before_fallocate() {
  local d out rc script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  # mounted=true (so the pre-check's own `mountpoint -q` succeeds) with a
  # MOUNT_BACK_OVERRIDE pointing away from image_path, simulating a
  # manually provisioned layout already mounted at the export root from
  # a different image entirely.
  _setup_export_script_fakebins "$d" "true" "$image_path" "/var/lib/scion-nfs/export.img"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" \
    "must refuse when the export root is already mounted from a different source"
  assert_contains "$out" "not from the image this script manages" "error should explain why"
  assert_false "$([[ -f "${d}/fallocate.log" ]] && echo true)" \
    "must never allocate a second image on top of an existing, unrecognized layout"
  assert_false "$([[ -f "${d}/mkfs.log" ]] && echo true)" \
    "must never format anything before this refusal"
  rm -rf "$d"
}

test_probe_export_script_executed_refuses_fstab_line_for_export_root_with_different_device() {
  local d out rc script image_path fake_fstab
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  fake_fstab="${d}/fstab"
  _setup_export_script_fakebins "$d" "false" "$image_path"
  printf '%s\n' "/var/lib/scion-nfs/export.img /srv/scion-shared ext4 loop,nofail 0 0" > "$fake_fstab"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20" "$fake_fstab")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" \
    "must refuse when fstab already has a different device mounted at the export root"
  assert_contains "$out" "does not match the expected entry" "error should explain why"
  assert_false "$([[ -f "${d}/fallocate.log" ]] && echo true)" \
    "must never allocate an image before this refusal"
  assert_false "$([[ -f "${d}/mkfs.log" ]] && echo true)" \
    "must never format anything before this refusal"
  rm -rf "$d"
}

test_probe_export_script_executed_refuses_stale_fstab_line_exact_match() {
  local d out rc script image_path fake_fstab
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  fake_fstab="${d}/fstab"
  _setup_export_script_fakebins "$d" "false" "$image_path"
  # Same device and mountpoint, but missing nofail -- a substring match
  # would accept this as "already correct" since the expected line
  # contains this one as a prefix; grep -qxF must match the exact line
  # only, not a prefix.
  printf '%s\n' "${image_path} /srv/scion-shared ext4 loop 0 0" > "$fake_fstab"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20" "$fake_fstab")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" \
    "a same-device fstab line with different options must fail exact-line matching, not be accepted as a prefix match"
  assert_contains "$out" "does not match the expected entry" "error should explain why"
  rm -rf "$d"
}

test_probe_export_script_executed_ignores_commented_out_fstab_line() {
  local d rc script image_path fake_fstab
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  fake_fstab="${d}/fstab"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  printf '%s\n' "# ${image_path} /srv/scion-shared ext4 loop,nofail,x-systemd.before=nfs-server.service,x-systemd.required-by=nfs-server.service 0 0" > "$fake_fstab"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20" "$fake_fstab")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1; rc=$?
  assert_eq "0" "$rc" "a commented-out copy of the line must never be read as the real thing"
  assert_true "$([[ -f "${d}/tee.log" ]] && grep -qF "${image_path} /srv/scion-shared ext4 loop,nofail" "${d}/tee.log" 2>/dev/null && echo true || echo false)" \
    "the real fstab line must still be appended when only a commented copy exists"
  rm -rf "$d"
}

test_probe_export_script_executed_ignores_commented_out_fstab_line_with_no_space_after_hash() {
  local d rc script image_path fake_fstab
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  fake_fstab="${d}/fstab"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  # No space between "#" and the path: with a space, the shifted-by-one
  # field split already happens to put the image path (not the
  # mountpoint) in the position the conflict check reads, so that shape
  # passes even without an explicit comment skip. Glued directly to the
  # path, the fields land the same as a live entry, so this specifically
  # exercises the comment check itself.
  printf '%s\n' "#${image_path} /srv/scion-shared ext4 loop,nofail,x-systemd.before=nfs-server.service,x-systemd.required-by=nfs-server.service 0 0" > "$fake_fstab"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20" "$fake_fstab")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1; rc=$?
  assert_eq "0" "$rc" "a commented-out line must be ignored even with no space after the '#'"
  assert_true "$([[ -f "${d}/tee.log" ]] && grep -qF "${image_path} /srv/scion-shared ext4 loop,nofail" "${d}/tee.log" 2>/dev/null && echo true || echo false)" \
    "the real fstab line must still be appended when only a commented copy exists"
  rm -rf "$d"
}

test_probe_export_script_executed_refuses_fstab_line_with_trailing_slash_mountpoint() {
  local d out rc script image_path fake_fstab
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  fake_fstab="${d}/fstab"
  _setup_export_script_fakebins "$d" "false" "$image_path"
  # A trailing slash on the mountpoint field is the same mountpoint to
  # the kernel, but not to a literal string comparison -- must still be
  # recognized as a conflict, not missed as "a different, unrelated
  # mountpoint".
  printf '%s\n' "/var/lib/other/export.img /srv/scion-shared/ ext4 loop,nofail 0 0" > "$fake_fstab"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20" "$fake_fstab")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" \
    "a trailing-slash mountpoint for the same export root must still be refused as a conflict"
  assert_contains "$out" "does not match the expected entry" "error should explain why"
  rm -rf "$d"
}

test_probe_export_script_executed_refuses_when_exact_line_present_alongside_a_conflicting_one() {
  local d out rc script image_path fake_fstab
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  fake_fstab="${d}/fstab"
  _setup_export_script_fakebins "$d" "false" "$image_path"
  # The expected line is present, verbatim, but so is a second, foreign
  # line for the same mountpoint -- the conflict check must not skip
  # itself just because a match for the exact line exists elsewhere in
  # the file; a stray second entry for /srv/scion-shared is exactly the
  # ambiguous state this script must refuse to guess about.
  printf '%s\n%s\n' \
    "${image_path} /srv/scion-shared ext4 loop,nofail,x-systemd.before=nfs-server.service,x-systemd.required-by=nfs-server.service 0 0" \
    "/var/lib/other/export.img /srv/scion-shared ext4 loop,nofail 0 0" \
    > "$fake_fstab"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20" "$fake_fstab")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" \
    "a foreign line for the same mountpoint must be refused even when the exact expected line is also present"
  assert_contains "$out" "does not match the expected entry" "error should explain why"
  rm -rf "$d"
}

test_probe_export_script_executed_restarts_nfs_server_on_first_run() {
  local d script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_true "$([[ -f "${d}/systemctl.log" ]] && grep -qF "restart nfs-server" "${d}/systemctl.log" 2>/dev/null && echo true || echo false)" \
    "the first run, with no prior config on disk, must restart to actually pick up v4.1/TCP-only"
  rm -rf "$d"
}

test_probe_export_script_executed_skips_restart_when_config_unchanged_and_already_active() {
  local d script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  set_export_script_nfs_conf_existing "$d" "$(printf '[nfsd]\nvers2=n\nvers3=n\nvers4.0=n\nudp=n')"
  set_export_script_nfs_server_active "$d"
  set_export_script_nfsd_versions "$d" "-2 -3 +4 -4.0 +4.1"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_false "$([[ -f "${d}/systemctl.log" ]] && grep -qF "restart nfs-server" "${d}/systemctl.log" 2>/dev/null && echo true)" \
    "an unchanged config with an already-active server confirmed at v4.1-only must not restart -- avoids an NFSv4 grace-period stall on every redeploy"
  assert_true "$([[ -f "${d}/systemctl.log" ]] && grep -qF "enable nfs-server" "${d}/systemctl.log" 2>/dev/null && echo true || echo false)" \
    "enable must still run even when restart is skipped"
  rm -rf "$d"
}

test_probe_export_script_executed_restarts_when_config_changed_and_already_active() {
  local d script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  # The existing drop-in content differs from what this run would write,
  # and the server is already active with v4.1-only versions -- restart
  # must still fire on the config-changed branch alone, not rely on the
  # inactive-server branch to cover it.
  set_export_script_nfs_conf_existing "$d" "$(printf '[nfsd]\nvers2=y')"
  set_export_script_nfs_server_active "$d"
  set_export_script_nfsd_versions "$d" "-2 -3 +4 -4.0 +4.1"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_true "$([[ -f "${d}/systemctl.log" ]] && grep -qF "restart nfs-server" "${d}/systemctl.log" 2>/dev/null && echo true || echo false)" \
    "a changed config must restart even when the server was already active"
  rm -rf "$d"
}

test_probe_export_script_executed_restarts_when_active_but_kernel_still_serves_v3() {
  local d script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  # Config unchanged and server active, but the kernel's own enabled-
  # version set still shows v3 on -- the interrupted-first-run scenario:
  # the drop-in was written, but nfs-server was never actually restarted
  # to pick it up, so it's still serving whatever it started with.
  set_export_script_nfs_conf_existing "$d" "$(printf '[nfsd]\nvers2=n\nvers3=n\nvers4.0=n\nudp=n')"
  set_export_script_nfs_server_active "$d"
  set_export_script_nfsd_versions "$d" "-2 +3 +4 -4.0 +4.1"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_true "$([[ -f "${d}/systemctl.log" ]] && grep -qF "restart nfs-server" "${d}/systemctl.log" 2>/dev/null && echo true || echo false)" \
    "the kernel still serving v3 must force a restart even with an unchanged config and an active server"
  rm -rf "$d"
}

test_probe_export_script_executed_restarts_when_v3_off_but_v40_still_on() {
  local d script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  # v3 is off but v4.0 is still on -- both must be off for the kernel to
  # count as confirmed v4.1-only; checking only the "-3" token and
  # ignoring "-4.0" would wrongly call this OK.
  set_export_script_nfs_conf_existing "$d" "$(printf '[nfsd]\nvers2=n\nvers3=n\nvers4.0=n\nudp=n')"
  set_export_script_nfs_server_active "$d"
  set_export_script_nfsd_versions "$d" "-2 -3 +4 +4.0 +4.1"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_true "$([[ -f "${d}/systemctl.log" ]] && grep -qF "restart nfs-server" "${d}/systemctl.log" 2>/dev/null && echo true || echo false)" \
    "the kernel still serving v4.0 must force a restart even though v3 itself is off"
  rm -rf "$d"
}

test_probe_export_script_executed_restarts_when_both_v3_and_v40_still_on() {
  local d script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  # Both v3 and v4.0 still on -- the worst case, distinct from the
  # v3-on/v4.0-off fixture above: neither the outer "-3" check nor the
  # inner "-4.0" check may be skipped.
  set_export_script_nfs_conf_existing "$d" "$(printf '[nfsd]\nvers2=n\nvers3=n\nvers4.0=n\nudp=n')"
  set_export_script_nfs_server_active "$d"
  set_export_script_nfsd_versions "$d" "-2 +3 +4 +4.0 +4.1"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_true "$([[ -f "${d}/systemctl.log" ]] && grep -qF "restart nfs-server" "${d}/systemctl.log" 2>/dev/null && echo true || echo false)" \
    "the kernel still serving both v3 and v4.0 must force a restart"
  rm -rf "$d"
}

test_probe_export_script_executed_restarts_when_nfsd_versions_unreadable() {
  local d script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  set_export_script_nfs_conf_existing "$d" "$(printf '[nfsd]\nvers2=n\nvers3=n\nvers4.0=n\nudp=n')"
  set_export_script_nfs_server_active "$d"
  set_export_script_nfsd_versions_unreadable "$d"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_true "$([[ -f "${d}/systemctl.log" ]] && grep -qF "restart nfs-server" "${d}/systemctl.log" 2>/dev/null && echo true || echo false)" \
    "an unreadable /proc/fs/nfsd/versions must fail closed toward restarting, not be treated as confirmation"
  rm -rf "$d"
}

test_probe_export_script_executed_restarts_when_config_unchanged_but_not_confirmed_active() {
  local d script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  set_export_script_nfs_conf_existing "$d" "$(printf '[nfsd]\nvers2=n\nvers3=n\nvers4.0=n\nudp=n')"
  # Deliberately not calling set_export_script_nfs_server_active: an
  # inconclusive "is it active" answer must fail closed toward
  # restarting, not toward skipping it.
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_true "$([[ -f "${d}/systemctl.log" ]] && grep -qF "restart nfs-server" "${d}/systemctl.log" 2>/dev/null && echo true || echo false)" \
    "an unconfirmed-active server must still restart even with unchanged config -- fail closed toward restarting"
  rm -rf "$d"
}

test_probe_export_script_executed_uses_the_given_non_default_size() {
  local d script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  # Every other executed test passes "20", the same as the default, which
  # can't distinguish "the given size was used" from "a hard-coded size
  # was used" -- a non-default size is the only way to tell the two
  # apart.
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "37")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_contains "$(cat "${d}/fallocate.log" 2>/dev/null || true)" "-l 37G" \
    "the image must actually be allocated at the given, non-default size"
  rm -rf "$d"
}

test_probe_export_script_executed_creates_image_only_once() {
  local d script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_eq "1" "$(wc -l < "${d}/mkfs.log" 2>/dev/null || echo 0)" "the first run must format the image exactly once"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_eq "1" "$(wc -l < "${d}/mkfs.log" 2>/dev/null || echo 0)" "a second run must never re-format an image that already exists"
  rm -rf "$d"
}

test_probe_export_script_executed_mount_called_once_when_absent_then_present() {
  local d script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "becomes" "$image_path"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  assert_eq "1" "$(wc -l < "${d}/mount.log" 2>/dev/null || echo 0)" \
    "mount must be called exactly once to bring up an absent export"
  rm -rf "$d"
}

test_probe_export_script_executed_mount_not_called_when_already_mounted() {
  local d script image_path mount_calls
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1
  mount_calls="0"
  [ -f "${d}/mount.log" ] && mount_calls="$(wc -l < "${d}/mount.log")"
  assert_eq "0" "$mount_calls" \
    "mount must never be called when the export root is already a mountpoint before the script even tries"
  rm -rf "$d"
}

test_probe_export_script_executed_refuses_wrong_mount_source() {
  local d out rc script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  # A pre-existing, already-mounted-elsewhere export root is caught by
  # the pre-fallocate existing-mount refusal, earlier than the post-mount
  # fail-closed check -- both checks use the same findmnt/losetup fakes,
  # so this exercises "mounted from the wrong source" via the earlier of
  # the two guards.
  _setup_export_script_fakebins "$d" "true" "$image_path" "/some/other/image.img"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" \
    "must exit non-zero when the export root is mounted from something other than this image's loop device"
  assert_contains "$out" "not from the image this script manages" "must explain why it refused"
  assert_false "$([[ -f "${d}/tee.log" ]] && grep -q "scion-hub-demohub.exports" "${d}/tee.log" 2>/dev/null && echo true)" \
    "must never write the exports file against the wrong mount"
  rm -rf "$d"
}

test_probe_export_script_executed_refuses_wrong_mount_source_after_fresh_mount() {
  local d out rc script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  # Not mounted at start (so the pre-fallocate existing-mount refusal
  # doesn't fire), but the `mount` fake's backing file doesn't match once
  # it does mount -- exercises the separate, post-mount fail-closed check
  # specifically.
  _setup_export_script_fakebins "$d" "becomes" "$image_path" "/some/other/image.img"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" \
    "must exit non-zero when a freshly attempted mount comes up from the wrong source"
  assert_contains "$out" "not from the loop device backing" "must explain why it refused"
  assert_false "$([[ -f "${d}/tee.log" ]] && grep -q "scion-hub-demohub.exports" "${d}/tee.log" 2>/dev/null && echo true)" \
    "must never write the exports file against the wrong mount"
  rm -rf "$d"
}

test_probe_export_script_executed_fallocate_failure_never_calls_mkfs() {
  local d out rc script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  printf '#!/bin/bash\necho "$*" >> "%s/fallocate.log"\nexit 1\n' "$d" > "$d/fallocate"
  chmod +x "$d/fallocate"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" "must exit non-zero when fallocate can't reserve the space"
  assert_contains "$out" "insufficient disk space" "must explain why it refused"
  assert_false "$([[ -f "${d}/mkfs.log" ]] && echo true)" "must never format an image that fallocate couldn't create"
  rm -rf "$d"
}

# The fake systemctl's `is-enabled` behavior, checked directly against
# what the real command does for a masked unit, an enabled one, and one
# it doesn't recognize at all -- proving the fake itself is faithful,
# not just convenient for the tests below to pass.
test_systemctl_fake_is_enabled_matches_real_behavior() {
  local d out rc
  d="$(mktemp -d)"
  _setup_export_script_fakebins "$d" "true"
  PATH="$d:$PATH" systemctl mask --now some.socket >/dev/null 2>&1
  out="$(PATH="$d:$PATH" systemctl is-enabled some.socket 2>&1)"; rc=$?
  assert_eq "masked" "$out" "a masked unit's is-enabled must print exactly 'masked'"
  assert_eq "1" "$rc" "a masked unit's is-enabled must exit non-zero, matching real systemctl"

  touch "${d}/.enabled-other.service"
  out="$(PATH="$d:$PATH" systemctl is-enabled other.service 2>&1)"; rc=$?
  assert_eq "enabled" "$out" "an enabled unit's is-enabled must print exactly 'enabled'"
  assert_eq "0" "$rc" "an enabled unit's is-enabled must exit zero"

  out="$(PATH="$d:$PATH" systemctl is-enabled never-heard-of-this.service 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" "an unknown unit's is-enabled must exit non-zero"
  rm -rf "$d"
}

# Prove-it: piping `systemctl is-enabled` through `grep -q masked` under
# pipefail would report failure for a masked unit even when grep itself
# matched, because the pipeline's exit status comes from is-enabled, not
# grep -- aborting the export step on every tier-on deploy. The check
# instead reads is-enabled's own stdout via `$(... || true)`, so a
# masked unit is detected correctly and the export step proceeds. These
# two tests pin that behaviour.
test_probe_export_script_executed_happy_path_reaches_exportfs() {
  local d rc script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  PATH="$d:$PATH" bash -c "$script" >/dev/null 2>&1; rc=$?
  assert_eq "0" "$rc" "the export script's happy path must exit zero once the rpcbind mask is correctly verified"
  assert_true "$([[ -f "${d}/exportfs.log" ]] && echo true || echo false)" \
    "exportfs must actually run once the rpcbind check passes"
  rm -rf "$d"
}

test_probe_export_script_executed_rpcbind_mask_not_verified_fails() {
  local d out rc script image_path
  d="$(mktemp -d)"
  image_path="${d}/export.img"
  _setup_export_script_fakebins "$d" "true" "$image_path"
  # Simulate `systemctl mask --now` silently not taking effect (the fake
  # systemctl's own mask handler is a no-op when this marker is present).
  touch "${d}/.systemctl-mask-fails"
  script="$(hybrid_nfs_export_script "/srv/scion-shared" "10.128.0.0/20" "6001" "6000" "abc123" "demohub" "$image_path" "20")"
  out="$(PATH="$d:$PATH" bash -c "$script" 2>&1)"; rc=$?
  assert_true "$([[ $rc -ne 0 ]] && echo true || echo false)" "an unmasked rpcbind.socket must fail the script, not be assumed masked"
  assert_contains "$out" "rpcbind.socket did not mask" "the failure must name why"
  assert_false "$([[ -f "${d}/exportfs.log" ]] && echo true)" "exportfs must never run when rpcbind isn't confirmed masked"
  rm -rf "$d"
}

# =====================================================================
# settings.yaml's server.shared_dir_storage block: the render function
# itself, and the conditional-splice mechanism deploy.sh uses to include
# it only when the tier is on, with zero bytes of difference otherwise.
# =====================================================================

test_settings_shared_dir_storage_yaml_fields() {
  local yaml
  yaml="$(hybrid_settings_shared_dir_storage_yaml "10.128.0.5" "/srv/scion-shared" "scion-hub-demohub-shared")"
  assert_contains "$yaml" "backend: nfs" "backend must be nfs"
  # mount_root/id must split so mount_root/id resolves back to the export
  # root -- the Docker broker computes its local mount path as
  # filepath.Join(mount_root, shares[0].id) -- not mount_root alone equal
  # to the export root, which would add an extra path segment and leave
  # Docker and GKE with two different trees.
  assert_contains "$yaml" 'mount_root: "/srv"' "mount_root must be the export root's parent directory"
  assert_contains "$yaml" 'id: "scion-shared"' "the share id must be the export root's base name"
  assert_contains "$yaml" 'subpath_root: "projects"' "subpath_root must be the fixed projects subdirectory"
  assert_contains "$yaml" 'server: "10.128.0.5"' "the share's server must be the VM's internal IP"
  assert_contains "$yaml" 'export: "/srv/scion-shared"' "the share's export path must be the full export root"
  assert_contains "$yaml" 'pv_name: "scion-hub-demohub-shared"' \
    "the share's pv_name field must carry the PVC name (that's what the Go side consumes as the claimName), not the PV's own name"
}

test_settings_shared_dir_storage_yaml_mount_root_and_id_rejoin_to_export_root() {
  local yaml mount_root share_id
  yaml="$(hybrid_settings_shared_dir_storage_yaml "10.128.0.5" "/srv/scion-shared" "scion-hub-demohub-shared")"
  mount_root="$(echo "$yaml" | "$PYTHON" -c "import sys; print(sys.stdin.read().split('mount_root: \"')[1].split('\"')[0])")"
  share_id="$(echo "$yaml" | "$PYTHON" -c "import sys; print(sys.stdin.read().split('id: \"')[1].split('\"')[0])")"
  assert_eq "/srv/scion-shared" "${mount_root}/${share_id}" \
    "mount_root joined with id must equal the export root, matching the PV's nfs.path and the Docker broker's own local mount path"
}

test_settings_shared_dir_storage_yaml_indented_under_server() {
  local yaml first_line
  yaml="$(hybrid_settings_shared_dir_storage_yaml "10.128.0.5" "/srv/scion-shared" "scion-hub-demohub-shared")"
  first_line="$(echo "$yaml" | head -1)"
  assert_eq "  shared_dir_storage:" "$first_line" \
    "must be indented two spaces, to nest directly under server: alongside hub:/storage:/etc."
}

# The exact conditional-splice pattern deploy.sh uses to include this
# block in settings.yaml only when it's non-empty, with no byte of
# difference (not even a blank line) when it's empty -- mirrored here
# rather than driving deploy.sh's own SSH-bound heredoc directly.
test_settings_shared_dir_storage_splice_tier_off_is_byte_identical() {
  local rendered
  local hybrid_block=""
  rendered="$(cat <<EOF
  auth:
    mode: dev
${hybrid_block:+${hybrid_block}
}  listen_port: 8080
EOF
)"
  assert_eq "$(printf '  auth:\n    mode: dev\n  listen_port: 8080')" "$rendered" \
    "tier off must produce exactly the pre-existing text, with no blank line or other artifact"
}

test_settings_shared_dir_storage_splice_tier_on_includes_block() {
  local rendered
  local hybrid_block
  hybrid_block="$(hybrid_settings_shared_dir_storage_yaml "10.128.0.5" "/srv/scion-shared" "scion-hub-demohub-shared")"
  rendered="$(cat <<EOF
  auth:
    mode: dev
${hybrid_block:+${hybrid_block}
}  listen_port: 8080
EOF
)"
  assert_contains "$rendered" "shared_dir_storage:" "tier on must splice the block in"
  assert_contains "$rendered" "  listen_port: 8080" "the rest of the block must still follow, unchanged"
}

test_cloud_run_label_args_new_service_gets_label() {
  fresh_gcloud_state
  local args
  args="$(hybrid_cloud_run_label_args "demohub-iap-proxy" "$PROJECT" "us-central1" "demohub")"
  assert_eq "--labels=scion-deployment=demohub" "$args" \
    "a not-yet-existing service (NOT_FOUND) is the first-create case and must get the marker"
}

# metadata.name=NAME is an equality match on the service name.
test_cloud_run_label_args_list_filter_form() {
  fresh_gcloud_state
  hybrid_cloud_run_label_args "demohub-iap-proxy" "$PROJECT" "us-central1" "demohub" >/dev/null
  assert_eq "run services list --project=${PROJECT} --region=us-central1 --filter=metadata.name=demohub-iap-proxy --format=value(metadata.name)" \
    "$(gcloud_log | grep '^run services list' || true)" \
    "the absence check must list the service by exact name, in exactly this filter form"
}

test_cloud_run_label_args_existing_service_no_label() {
  fresh_gcloud_state
  seed_run_service_exists "demohub-iap-proxy"
  local args
  args="$(hybrid_cloud_run_label_args "demohub-iap-proxy" "$PROJECT" "us-central1" "demohub")"
  assert_eq "" "$args" "an already-existing service is a redeploy and must not be (re-)labeled"
}

test_cloud_run_label_args_describe_error_but_list_confirms_exists_no_label() {
  fresh_gcloud_state
  set_run_service_describe_error "demohub-iap-proxy"
  seed_run_service_exists "demohub-iap-proxy"
  local args
  args="$(hybrid_cloud_run_label_args "demohub-iap-proxy" "$PROJECT" "us-central1" "demohub")"
  assert_eq "" "$args" \
    "describe can fail (a permissions problem, for example) while list still positively confirms the service exists -- no label"
}

test_cloud_run_label_args_describe_error_and_list_error_fails_safe_no_label() {
  fresh_gcloud_state
  set_run_service_describe_error "demohub-iap-proxy"
  set_run_service_list_error
  local args
  args="$(hybrid_cloud_run_label_args "demohub-iap-proxy" "$PROJECT" "us-central1" "demohub")"
  assert_eq "" "$args" \
    "when neither describe nor the positive-absence list call succeeds, existence is truly unknown and must fail safe toward no label"
}

test_cloud_run_label_args_real_cannot_find_service_text_still_yields_label() {
  fresh_gcloud_state
  # Real gcloud's Cloud Run 404 text ("Cannot find service [X]") carries
  # none of the NOT_FOUND/404 tokens the old text-matching implementation
  # looked for -- this proves the label still gets added via the `list`
  # call regardless of what describe's error text says.
  local args
  args="$(hybrid_cloud_run_label_args "demohub-iap-proxy" "$PROJECT" "us-central1" "demohub")"
  assert_eq "--labels=scion-deployment=demohub" "$args" \
    "the stub's describe error is real gcloud's own 'Cannot find service' text, which must still resolve to a label via list"
}

# =====================================================================
# Not-found matching: tightened to gcloud's/kubectl's own specific
# signals, never a bare "not found" substring, since some permission-
# denied responses are deliberately worded to avoid confirming whether a
# resource exists (for example "...not found or permission denied").
# =====================================================================

test_gcloud_not_found_matches_NOT_FOUND_token() {
  assert_true "$(_hybrid_gcloud_not_found 'gcloud-stub: NOT_FOUND: Cluster x not found' && echo true || echo false)" \
    "a NOT_FOUND status token must be recognized"
}

test_gcloud_not_found_matches_404_code() {
  assert_true "$(_hybrid_gcloud_not_found 'ResponseError: code=404, message=Not Found' && echo true || echo false)" \
    "an HTTP 404 code must be recognized"
}

test_gcloud_not_found_matches_requested_entity_message() {
  assert_true "$(_hybrid_gcloud_not_found 'gcloud-stub: NOT_FOUND: Requested entity was not found.' && echo true || echo false)" \
    "the literal 'Requested entity was not found' message must be recognized"
}

test_gcloud_not_found_rejects_permission_masked_message() {
  assert_true "$(_hybrid_gcloud_not_found 'Cluster x not found or permission denied' && echo false || echo true)" \
    "a permission-denied message that also happens to say 'not found' must never be read as NOT_FOUND"
}

test_gcloud_not_found_rejects_forbidden_message() {
  assert_true "$(_hybrid_gcloud_not_found 'ERROR: 403 Forbidden: resource not found' && echo false || echo true)" \
    "a forbidden message must never be read as NOT_FOUND, even if it also mentions 'not found'"
}

test_gcloud_not_found_rejects_bare_substring() {
  assert_true "$(_hybrid_gcloud_not_found 'the widget was not found in the drawer' && echo false || echo true)" \
    "a bare 'not found' substring with none of gcloud's own signals must not match"
}

test_kubectl_not_found_matches_NotFound_reason() {
  assert_true "$(_hybrid_kubectl_not_found 'Error from server (NotFound): persistentvolumes "x" not found' && echo true || echo false)" \
    "kubectl's own (NotFound) reason token must be recognized"
}

test_kubectl_not_found_rejects_permission_masked_message() {
  assert_true "$(_hybrid_kubectl_not_found 'Error from server: pv "x" not found or permission denied' && echo false || echo true)" \
    "a permission-denied message that also happens to say 'not found' must never be read as NotFound"
}

# Realistic fixtures against real gcloud/kubectl error corpora (not
# invented text), proving the permission/forbidden exclusion is doing
# real work (a genuine not-found TOKEN alongside permission wording must
# still read as "not gone"), and that a bare 404-shaped substring
# embedded in an unrelated name, URL, or timeout message is never
# mistaken for gcloud's own absence signal.
test_gcloud_not_found_rejects_404_with_permission_text() {
  assert_true "$(_hybrid_gcloud_not_found 'ResponseError: code=404, message=Not found or permission denied' && echo false || echo true)" \
    "a 404 whose text also mentions permission must not read as gone"
}
test_gcloud_not_found_rejects_cluster_named_with_404_in_url() {
  assert_true "$(_hybrid_gcloud_not_found "ERROR: gcloud crashed (ConnectionError): HTTPSConnectionPool(host='container.googleapis.com', port=443): Max retries exceeded with url: /v1/projects/p1/locations/us-central1/clusters/hub-404?alt=json" && echo false || echo true)" \
    "a cluster literally named with '404' in the URL must not read as gone"
}
test_gcloud_not_found_rejects_project_id_with_404_in_url() {
  assert_true "$(_hybrid_gcloud_not_found "ERROR: gcloud crashed (ConnectionError): HTTPSConnectionPool(host='container.googleapis.com', port=443): Max retries exceeded with url: /v1/projects/team-404-prod/locations/us-central1/clusters/c1?alt=json" && echo false || echo true)" \
    "a project id containing '404' must not read as gone"
}
test_gcloud_not_found_rejects_proxy_status_line_with_404_in_request_id() {
  assert_true "$(_hybrid_gcloud_not_found 'ERROR: HTTPError 502: Bad gateway from proxy 10.0.0.1 (request 404-7a1)' && echo false || echo true)" \
    "an unrelated 502 whose request id happens to contain '404' must not read as gone"
}
test_gcloud_not_found_rejects_timeout_seconds_404() {
  assert_true "$(_hybrid_gcloud_not_found 'ERROR: Timeout after 404 seconds' && echo false || echo true)" \
    "a timeout duration that happens to be 404 seconds must not read as gone"
}
test_gcloud_not_found_rejects_service_unavailable() {
  assert_true "$(_hybrid_gcloud_not_found 'ResponseError: code=503, message=The service is currently unavailable.' && echo false || echo true)" \
    "a 503 must not read as gone"
}
test_gcloud_not_found_rejects_quota_exceeded() {
  assert_true "$(_hybrid_gcloud_not_found 'ResponseError: code=429, message=Quota exceeded for quota metric' && echo false || echo true)" \
    "a 429 must not read as gone"
}
test_gcloud_not_found_rejects_missing_required_flag() {
  assert_true "$(_hybrid_gcloud_not_found 'One of [--location, --region, --zone] must be supplied.' && echo false || echo true)" \
    "a usage error must not read as gone"
}
test_gcloud_not_found_rejects_did_you_mean_suggestion() {
  assert_true "$(_hybrid_gcloud_not_found "ERROR: (gcloud.container.clusters.describe) NOT_FOUND: Did you mean 'my-cluster'?" && echo false || echo true)" \
    "a suggestion response means gcloud found something close, not that the requested name is confirmed absent"
}

test_kubectl_not_found_matches_real_pvc_and_namespace_fixtures() {
  assert_true "$(_hybrid_kubectl_not_found 'Error from server (NotFound): persistentvolumeclaims "scion-hub-h-shared" not found' && echo true || echo false)" \
    "a real PVC not-found message must be recognized"
  assert_true "$(_hybrid_kubectl_not_found 'Error from server (NotFound): namespaces "scion-hub-h" not found' && echo true || echo false)" \
    "a real namespace not-found message must be recognized"
}
test_kubectl_not_found_rejects_forbidden_with_notfound_token() {
  assert_true "$(_hybrid_kubectl_not_found 'Error from server (Forbidden): persistentvolumeclaims "x" is forbidden: User "u" cannot get resource "persistentvolumeclaims" in API group "" in the namespace "ns"' && echo false || echo true)" \
    "a Forbidden response must never read as absent"
}
test_kubectl_not_found_rejects_connection_timeout() {
  assert_true "$(_hybrid_kubectl_not_found 'Unable to connect to the server: dial tcp 10.1.2.3:443: i/o timeout' && echo false || echo true)" \
    "a connectivity failure must not read as absent"
}
test_kubectl_not_found_rejects_unrecognized_resource_type() {
  assert_true "$(_hybrid_kubectl_not_found "error: the server doesn't have a resource type \"pvc\"" && echo false || echo true)" \
    "an unrecognized resource type error must not read as absent"
}
test_kubectl_not_found_rejects_missing_auth_plugin() {
  assert_true "$(_hybrid_kubectl_not_found 'Unable to connect to the server: getting credentials: exec: executable gke-gcloud-auth-plugin not found' && echo false || echo true)" \
    "a missing-auth-plugin error (which itself says 'not found') must not read as the object being absent"
}
test_kubectl_not_found_rejects_unauthorized() {
  assert_true "$(_hybrid_kubectl_not_found 'error: You must be logged in to the server (Unauthorized)' && echo false || echo true)" \
    "an unauthorized error must not read as absent"
}
test_kubectl_not_found_rejects_generic_server_could_not_find_resource() {
  assert_true "$(_hybrid_kubectl_not_found 'Error from server (NotFound): the server could not find the requested resource' && echo false || echo true)" \
    "the API server's own generic 'could not find the requested resource' (an endpoint/version skew, not the object being checked) must not read as absent"
}

# =====================================================================
# Kubernetes objects: naming, kubeconfig setup, manifest rendering,
# create/refuse/drift, and teardown. No test here contacts a real
# cluster; the stub kubectl records every invocation and serves
# fixtures the same way the stub gcloud does.
# =====================================================================

K8S_HUB="demohub"
K8S_NS="scion-hub-${K8S_HUB}"
K8S_PVC="scion-hub-${K8S_HUB}-shared"
K8S_PV="scion-hub-${K8S_HUB}-shared"
K8S_VM_IP="10.128.0.5"

test_k8s_pv_name() {
  assert_eq "scion-hub-demohub-shared" "$(hybrid_k8s_pv_name "demohub")" "PV name must be scion-hub-<hub>-shared"
}
test_k8s_default_namespace() {
  assert_eq "scion-hub-demohub" "$(hybrid_k8s_default_namespace "demohub")" "default namespace must be scion-hub-<hub>"
}
test_k8s_default_pvc_name() {
  assert_eq "scion-hub-demohub-shared" "$(hybrid_k8s_default_pvc_name "demohub")" "default PVC name must be scion-hub-<hub>-shared"
}

test_k8s_pv_manifest_fields() {
  local yaml
  yaml="$(hybrid_k8s_pv_manifest "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "$K8S_NS" "$K8S_PVC")"
  assert_contains "$yaml" "name: ${K8S_PV}" "must name the PV"
  assert_contains "$yaml" "scion-deployment: ${K8S_HUB}" "must carry the marker label"
  assert_contains "$yaml" "server: ${K8S_VM_IP}" "must point the NFS server at the VM's IP"
  assert_contains "$yaml" "path: /srv/scion-shared" "must point at the export root"
  assert_contains "$yaml" "persistentVolumeReclaimPolicy: Retain" "reclaim policy must be Retain"
  assert_contains "$yaml" 'storageClassName: ""' "must never be dynamically provisioned"
  assert_contains "$yaml" "- ReadWriteMany" "access mode must be RWX"
  assert_contains "$yaml" "- nfsvers=4.1" "mount options must pin the NFS version"
  assert_contains "$yaml" "- hard" "mount options must use hard, not soft"
  assert_contains "$yaml" "namespace: ${K8S_NS}" "claimRef must pin the namespace"
  assert_contains "$yaml" "name: ${K8S_PVC}" "claimRef must pin the PVC name"
}

test_k8s_namespace_manifest_fields() {
  local yaml
  yaml="$(hybrid_k8s_namespace_manifest "$K8S_NS" "$K8S_HUB")"
  assert_contains "$yaml" "kind: Namespace" "must be a Namespace object"
  assert_contains "$yaml" "name: ${K8S_NS}" "must name the namespace"
  assert_contains "$yaml" "scion-deployment: ${K8S_HUB}" "must carry the marker label"
}

test_k8s_pvc_manifest_fields() {
  local yaml
  yaml="$(hybrid_k8s_pvc_manifest "$K8S_PVC" "$K8S_NS" "$K8S_HUB" "$K8S_PV")"
  assert_contains "$yaml" "name: ${K8S_PVC}" "must name the PVC"
  assert_contains "$yaml" "namespace: ${K8S_NS}" "must be in the target namespace"
  assert_contains "$yaml" "scion-deployment: ${K8S_HUB}" "must carry the marker label"
  assert_contains "$yaml" "volumeName: ${K8S_PV}" "must bind to the expected PV"
  assert_contains "$yaml" 'storageClassName: ""' "must never be dynamically provisioned"
  assert_contains "$yaml" "- ReadWriteMany" "access mode must be RWX"
}

test_k8s_setup_kubeconfig_uses_distinct_temp_files() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  local first="$HYBRID_KUBECONFIG"
  hybrid_k8s_setup_kubeconfig
  local second="$HYBRID_KUBECONFIG"
  assert_true "$([[ "$first" != "$second" ]] && echo true || echo false)" "each setup call must use its own fresh temp file"
  assert_not_contains "$first" ".kube" "must never point at the operator's default kubeconfig location"
  rm -f "$first" "$second"
}

test_k8s_setup_kubeconfig_missing_kubectl_fails() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  local old_path="$PATH"
  # shellcheck disable=SC2123 # deliberately hiding kubectl for this one test
  PATH="/nonexistent-bin-dir-for-this-test"
  run_expect_fail hybrid_k8s_setup_kubeconfig
  PATH="$old_path"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "missing kubectl must fail the run"
  assert_contains "$RUN_OUTPUT" "kubectl is required" "error should say why"
}

test_k8s_setup_kubeconfig_missing_auth_plugin_fails() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  local d
  d="$(mktemp -d)"
  ln -s "$(command -v kubectl)" "${d}/kubectl"
  local old_path="$PATH"
  PATH="${d}"
  run_expect_fail hybrid_k8s_setup_kubeconfig
  PATH="$old_path"
  rm -rf "$d"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "missing gke-gcloud-auth-plugin must fail the run even when kubectl itself is present"
  assert_contains "$RUN_OUTPUT" "gke-gcloud-auth-plugin is required" "error should say why"
}

test_k8s_setup_kubeconfig_get_credentials_failure() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  set_get_credentials_will_fail
  run_expect_fail hybrid_k8s_setup_kubeconfig
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a get-credentials failure must fail the run"
  assert_contains "$RUN_OUTPUT" "Could not get credentials" "error should explain what failed"
}

# Proves every kubectl call uses HYBRID_KUBECONFIG specifically, never an
# ambient value -- even when the caller's own shell has KUBECONFIG set to
# something else (run.sh exports exactly such a sentinel, bogus value
# globally; this test additionally overrides it to a second, distinct
# bogus value of its own, to prove the property doesn't depend on which
# ambient value happens to be set). Runs the full create-mode surface
# (preflight, ensure, teardown check, teardown delete) through one
# KUBECONFIG override, not just setup_kubeconfig alone.
test_probe_every_kubectl_call_used_task_private_kubeconfig() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  local saved="${KUBECONFIG-__unset__}"
  export KUBECONFIG="/nonexistent/a-different-ambient-kubeconfig-for-this-test"
  hybrid_k8s_preflight "$K8S_HUB"
  local preflight_kubeconfig="$HYBRID_KUBECONFIG"
  hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP" >/dev/null 2>&1
  hybrid_k8s_teardown_check "$K8S_HUB" >/dev/null 2>&1
  hybrid_k8s_teardown_delete >/dev/null 2>&1
  local total good
  total="$(kubectl_log | grep -c . || true)"
  good="$(kubectl_log | grep -c "^KUBECONFIG=${preflight_kubeconfig} " || true)"
  assert_true "$([[ "$total" -gt 0 ]] && echo true || echo false)" "probe must have exercised kubectl"
  assert_eq "$total" "$good" "every kubectl call must use HYBRID_KUBECONFIG, not the ambient KUBECONFIG"
  assert_eq "false" "$([[ -e /nonexistent/a-different-ambient-kubeconfig-for-this-test ]] && echo true || echo false)" \
    "get-credentials must never write the ambient KUBECONFIG"
  rm -f "$preflight_kubeconfig" "$HYBRID_KUBECONFIG"
  if [[ "$saved" == "__unset__" ]]; then unset KUBECONFIG; else export KUBECONFIG="$saved"; fi
}

# =====================================================================
# hybrid_k8s_preflight: everything about the Kubernetes objects that
# doesn't depend on the VM's IP, meant to run right after
# hybrid_discover, before the VM/NFS/firewall rules exist.
# =====================================================================

test_k8s_preflight_refuses_unmarked_pv_without_creating_namespace() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_k8s_pv_unmarked "$K8S_PV"
  run_expect_fail hybrid_k8s_preflight "$K8S_HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unmarked PV must refuse the run"
  assert_contains "$RUN_OUTPUT" "without this deployment's marker" "error should explain why"
  assert_false "$([[ -f "${KUBECTL_STUB_STATE_DIR}/namespace/${K8S_NS}.json" ]] && echo true)" \
    "the namespace must never be created after a PV refusal -- namespace creation only happens once the PV/PVC checks pass"
}

test_k8s_preflight_refuses_unmarked_pvc() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_k8s_pvc_unmarked "$K8S_PVC" "$K8S_NS"
  run_expect_fail hybrid_k8s_preflight "$K8S_HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unmarked PVC must refuse the run"
  assert_contains "$RUN_OUTPUT" "without this deployment's marker" "error should explain why"
}

test_k8s_preflight_pv_non_ip_drift_fails() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/other-path" "$K8S_NS" "$K8S_PVC"
  run_expect_fail hybrid_k8s_preflight "$K8S_HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a drifted (non-IP) PV field must fail before anything else is created"
  assert_contains "$RUN_OUTPUT" "path:" "error should name the drifted field"
}

test_k8s_preflight_pv_drift_reclaim_policy_fails() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "$K8S_NS" "$K8S_PVC" "Delete"
  run_expect_fail hybrid_k8s_preflight "$K8S_HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a drifted reclaim policy must fail preflight"
  assert_contains "$RUN_OUTPUT" "persistentVolumeReclaimPolicy:" "error should name the drifted field"
}

test_k8s_preflight_pv_drift_claim_name_fails() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "$K8S_NS" "some-other-pvc"
  run_expect_fail hybrid_k8s_preflight "$K8S_HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a drifted claimRef.name must fail preflight"
  assert_contains "$RUN_OUTPUT" "claimRef.name:" "error should name the drifted field"
}

test_k8s_preflight_pv_drift_claim_namespace_fails() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "some-other-namespace" "$K8S_PVC"
  run_expect_fail hybrid_k8s_preflight "$K8S_HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a drifted claimRef.namespace must fail preflight"
  assert_contains "$RUN_OUTPUT" "claimRef.namespace:" "error should name the drifted field"
}

test_k8s_preflight_namespace_get_error_fails_closed() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  set_k8s_get_error namespace "$K8S_NS"
  run_expect_fail hybrid_k8s_preflight "$K8S_HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "an unknown namespace check must fail preflight closed, not be treated as absent (which would create over it)"
}

test_k8s_preflight_pvc_get_error_fails_closed() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  set_k8s_get_error pvc "${K8S_NS}__${K8S_PVC}"
  run_expect_fail hybrid_k8s_preflight "$K8S_HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "an unknown PVC check must fail preflight closed, not be treated as absent"
}

test_k8s_preflight_does_not_check_pv_server_ip() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  # An IP that will NOT match the eventual VM IP -- preflight must not
  # care, since it runs before the VM's IP is read or reserved.
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "10.99.99.99" "/srv/scion-shared" "$K8S_NS" "$K8S_PVC"
  hybrid_k8s_preflight "$K8S_HUB"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_preflight_creates_namespace_when_absent_and_pv_pvc_checks_pass() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_preflight "$K8S_HUB"
  assert_true "$([[ -f "${KUBECTL_STUB_STATE_DIR}/namespace/${K8S_NS}.json" ]] && echo true || echo false)" \
    "the namespace must be created when absent and nothing else refused first"
  assert_false "$([[ -f "${KUBECTL_STUB_STATE_DIR}/pv/${K8S_PV}.json" ]] && echo true)" \
    "the PV must NOT be created here -- creation stays in hybrid_k8s_ensure_objects, once the VM's IP is known"
  assert_false "$([[ -f "${KUBECTL_STUB_STATE_DIR}/pvc/${K8S_NS}__${K8S_PVC}.json" ]] && echo true)" \
    "the PVC must NOT be created here either"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_preflight_unmarked_namespace_used_as_is() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_k8s_namespace_unmarked "$K8S_NS"
  hybrid_k8s_preflight "$K8S_HUB"
  assert_eq "0" "$(kubectl_log | grep -c 'create -f' || true)" "an existing, unmarked namespace must not be re-created (labeled)"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_creates_all_three_when_absent() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ -f "${KUBECTL_STUB_STATE_DIR}/namespace/${K8S_NS}.json" ]] && echo true || echo false)" "namespace must be created"
  assert_true "$([[ -f "${KUBECTL_STUB_STATE_DIR}/pv/${K8S_PV}.json" ]] && echo true || echo false)" "PV must be created"
  assert_true "$([[ -f "${KUBECTL_STUB_STATE_DIR}/pvc/${K8S_NS}__${K8S_PVC}.json" ]] && echo true || echo false)" "PVC must be created"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_refuses_unmarked_pv() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pv_unmarked "$K8S_PV"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unmarked PV must refuse the run"
  assert_contains "$RUN_OUTPUT" "without this deployment's marker" "error should explain why"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_refuses_unmarked_pvc() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pvc_unmarked "$K8S_PVC" "$K8S_NS"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unmarked PVC must refuse the run"
  assert_contains "$RUN_OUTPUT" "without this deployment's marker" "error should explain why"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_unmarked_namespace_used_not_labeled_not_refused() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_namespace_unmarked "$K8S_NS"
  hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_eq "2" "$(kubectl_log | grep -c 'create -f' || true)" \
    "only the PV and PVC should be created -- the unmarked-but-usable namespace must not be re-created (labeled)"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_pv_drift_ip_change_fails_with_remediation() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "10.128.0.99" "/srv/scion-shared" "$K8S_NS" "$K8S_PVC"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a PV whose server IP no longer matches must fail the run"
  assert_contains "$RUN_OUTPUT" "server:" "error should name the drifted field"
  assert_contains "$RUN_OUTPUT" "kubectl delete pvc" "remediation must include deleting the PVC first"
  assert_contains "$RUN_OUTPUT" "kubectl delete pv" "remediation must include deleting the PV"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_pvc_drift_wrong_volume_fails() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "$K8S_NS" "$K8S_PVC"
  seed_k8s_pvc "$K8S_PVC" "$K8S_NS" "$K8S_HUB" "some-other-pv"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a PVC bound to the wrong PV must fail the run"
  assert_contains "$RUN_OUTPUT" "some-other-pv" "error should name the actual, wrong binding"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_pv_drift_path_fails() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/other-path" "$K8S_NS" "$K8S_PVC"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a drifted export path must fail the run"
  assert_contains "$RUN_OUTPUT" "path:" "error should name the drifted field"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_pv_drift_reclaim_policy_fails() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "$K8S_NS" "$K8S_PVC" "Delete"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a drifted reclaim policy must fail the run"
  assert_contains "$RUN_OUTPUT" "persistentVolumeReclaimPolicy:" "error should name the drifted field"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_pv_drift_claim_name_fails() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "$K8S_NS" "some-other-pvc"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a drifted claimRef.name must fail the run"
  assert_contains "$RUN_OUTPUT" "claimRef.name:" "error should name the drifted field"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_pv_drift_claim_namespace_fails() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "some-other-namespace" "$K8S_PVC"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a drifted claimRef.namespace must fail the run"
  assert_contains "$RUN_OUTPUT" "claimRef.namespace:" "error should name the drifted field"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_namespace_get_error_fails_closed() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  set_k8s_get_error namespace "$K8S_NS"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "an unknown namespace check must fail closed, not be treated as absent"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_pvc_get_error_fails_closed() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  set_k8s_get_error pvc "${K8S_NS}__${K8S_PVC}"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "an unknown PVC check must fail closed, not be treated as absent (which would create a duplicate)"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_get_notfound_shaped_permission_denied_treated_as_unknown() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  # Unlike set_k8s_get_permission_masked (whose text never matches
  # kubectl's strict NotFound shape in the first place, so the
  # permission/forbidden exclusion in _hybrid_kubectl_not_found is never
  # actually the deciding factor), this message DOES match that exact
  # shape ('Error from server (NotFound): <kind> "<name>" not found')
  # while also being permission-worded, isolating the exclusion itself:
  # without it, this would read as a genuine "absent" match.
  printf 'Error from server (NotFound): pv "%s" not found: permission denied (RBAC)' "$K8S_PV" \
    > "${KUBECTL_STUB_STATE_DIR}/pv/${K8S_PV}.json.get-error"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a NotFound-shaped message that also says permission/forbidden must still fail closed as unknown, not absent"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_matching_marked_objects_are_reused_without_recreating() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_namespace "$K8S_NS" "$K8S_HUB"
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "$K8S_NS" "$K8S_PVC"
  seed_k8s_pvc "$K8S_PVC" "$K8S_NS" "$K8S_HUB" "$K8S_PV"
  hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_eq "0" "$(kubectl_log | grep -c 'apply -f' || true)" \
    "matching, marked objects must be reused, not recreated"
}

test_k8s_ensure_get_error_on_pv_fails_closed() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  set_k8s_get_error pv "$K8S_PV"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unknown PV state must fail closed, not be treated as absent"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_ensure_get_permission_masked_on_pv_fails_closed_not_absent() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  set_k8s_get_permission_masked pv "$K8S_PV"
  run_expect_fail hybrid_k8s_ensure_objects "$K8S_HUB" "$K8S_VM_IP"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a permission-denied message that also says 'not found' must fail closed, not be treated as absent (which would create a duplicate PV)"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_check_all_marked_queues_all_three_in_order() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pvc "$K8S_PVC" "$K8S_NS" "$K8S_HUB" "$K8S_PV"
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "$K8S_NS" "$K8S_PVC"
  seed_k8s_namespace "$K8S_NS" "$K8S_HUB"
  hybrid_k8s_teardown_check "$K8S_HUB"
  assert_eq "false" "$HYBRID_K8S_TEARDOWN_FAILED" "all-marked must not fail the preflight"
  assert_eq "3" "${#HYBRID_K8S_TEARDOWN_DELETE[@]}" "all three objects must be queued"
  assert_eq "pvc" "${HYBRID_K8S_TEARDOWN_DELETE[0]:-}" "PVC must be first in deletion order"
  assert_eq "pv" "${HYBRID_K8S_TEARDOWN_DELETE[1]:-}" "PV must be second"
  assert_eq "namespace" "${HYBRID_K8S_TEARDOWN_DELETE[2]:-}" "namespace must be last"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_check_unmarked_pvc_aborts() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pvc_unmarked "$K8S_PVC" "$K8S_NS"
  hybrid_k8s_teardown_check "$K8S_HUB"
  assert_eq "true" "$HYBRID_K8S_TEARDOWN_FAILED" "an unmarked PVC must abort the whole teardown"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_check_unmarked_pv_aborts() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pv_unmarked "$K8S_PV"
  hybrid_k8s_teardown_check "$K8S_HUB"
  assert_eq "true" "$HYBRID_K8S_TEARDOWN_FAILED" "an unmarked PV must abort the whole teardown"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_check_unmarked_namespace_skipped_not_aborted() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_namespace_unmarked "$K8S_NS"
  hybrid_k8s_teardown_check "$K8S_HUB"
  assert_eq "false" "$HYBRID_K8S_TEARDOWN_FAILED" "an unmarked namespace must not abort -- using an existing one is allowed"
  local k found=false
  for k in ${HYBRID_K8S_TEARDOWN_DELETE[@]+"${HYBRID_K8S_TEARDOWN_DELETE[@]}"}; do
    [[ "$k" == "namespace" ]] && found=true
  done
  assert_eq "false" "$found" "an unmarked namespace must never be queued for deletion"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_check_get_error_aborts() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  set_k8s_get_error namespace "$K8S_NS"
  hybrid_k8s_teardown_check "$K8S_HUB"
  assert_eq "true" "$HYBRID_K8S_TEARDOWN_FAILED" "an unknown check result must abort, not be treated as absent"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_check_pvc_get_error_aborts() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  set_k8s_get_error pvc "${K8S_NS}__${K8S_PVC}"
  hybrid_k8s_teardown_check "$K8S_HUB"
  assert_eq "true" "$HYBRID_K8S_TEARDOWN_FAILED" "an unknown PVC check must abort teardown, not be treated as absent"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_check_refuses_when_pod_references_pvc() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pvc "$K8S_PVC" "$K8S_NS" "$K8S_HUB" "$K8S_PV"
  seed_k8s_pod_using_pvc "$K8S_NS" "some-agent-pod" "$K8S_PVC"
  hybrid_k8s_teardown_check "$K8S_HUB"
  assert_eq "true" "$HYBRID_K8S_TEARDOWN_FAILED" "a pod still mounting the PVC must refuse teardown before any delete"
  local k found=false
  for k in ${HYBRID_K8S_TEARDOWN_DELETE[@]+"${HYBRID_K8S_TEARDOWN_DELETE[@]}"}; do
    [[ "$k" == "pvc" ]] && found=true
  done
  assert_eq "false" "$found" "the PVC must not be queued for deletion while a pod still mounts it"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_check_pods_get_error_aborts() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pvc "$K8S_PVC" "$K8S_NS" "$K8S_HUB" "$K8S_PV"
  set_k8s_pods_get_error "$K8S_NS"
  hybrid_k8s_teardown_check "$K8S_HUB"
  assert_eq "true" "$HYBRID_K8S_TEARDOWN_FAILED" "an unknown pod-reference check must abort, not be treated as safe to delete"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_check_no_referencing_pods_proceeds() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pvc "$K8S_PVC" "$K8S_NS" "$K8S_HUB" "$K8S_PV"
  hybrid_k8s_teardown_check "$K8S_HUB"
  assert_eq "false" "$HYBRID_K8S_TEARDOWN_FAILED" "no referencing pods must not fail the preflight"
  local k found=false
  for k in ${HYBRID_K8S_TEARDOWN_DELETE[@]+"${HYBRID_K8S_TEARDOWN_DELETE[@]}"}; do
    [[ "$k" == "pvc" ]] && found=true
  done
  assert_eq "true" "$found" "the PVC must still be queued when nothing references it"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_delete_bounds_every_delete_with_a_timeout() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pvc "$K8S_PVC" "$K8S_NS" "$K8S_HUB" "$K8S_PV"
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "$K8S_NS" "$K8S_PVC"
  seed_k8s_namespace "$K8S_NS" "$K8S_HUB"
  hybrid_k8s_teardown_check "$K8S_HUB"
  hybrid_k8s_teardown_delete
  local deletes
  deletes="$(kubectl_log | grep -c ' delete ' || true)"
  local timed_out
  timed_out="$(kubectl_log | grep ' delete ' | grep -c -- '--timeout=' || true)"
  assert_eq "$deletes" "$timed_out" "every kubectl delete call must carry an explicit --timeout"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_delete_stops_at_first_failure() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pvc "$K8S_PVC" "$K8S_NS" "$K8S_HUB" "$K8S_PV"
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "$K8S_NS" "$K8S_PVC"
  seed_k8s_namespace "$K8S_NS" "$K8S_HUB"
  set_k8s_delete_will_fail pvc "${K8S_NS}__${K8S_PVC}"
  hybrid_k8s_teardown_check "$K8S_HUB"
  hybrid_k8s_teardown_delete
  assert_eq "3" "${#HYBRID_K8S_TEARDOWN_DELETE_FAILED[@]}" \
    "the failed PVC plus the PV and namespace queued behind it must all be recorded as not deleted"
  assert_true "$([[ -f "${KUBECTL_STUB_STATE_DIR}/pv/${K8S_PV}.json" ]] && echo true || echo false)" \
    "the PV must never be deleted after the PVC delete failed"
  assert_true "$([[ -f "${KUBECTL_STUB_STATE_DIR}/namespace/${K8S_NS}.json" ]] && echo true || echo false)" \
    "the namespace must never be deleted after the PVC delete failed"
  rm -f "$HYBRID_KUBECONFIG"
}

test_k8s_teardown_delete_all_succeed() {
  fresh_gcloud_state
  GKE_NAME="mycluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  hybrid_k8s_setup_kubeconfig
  seed_k8s_pvc "$K8S_PVC" "$K8S_NS" "$K8S_HUB" "$K8S_PV"
  seed_k8s_pv "$K8S_PV" "$K8S_HUB" "$K8S_VM_IP" "/srv/scion-shared" "$K8S_NS" "$K8S_PVC"
  seed_k8s_namespace "$K8S_NS" "$K8S_HUB"
  hybrid_k8s_teardown_check "$K8S_HUB"
  hybrid_k8s_teardown_delete
  assert_eq "0" "${#HYBRID_K8S_TEARDOWN_DELETE_FAILED[@]}" "nothing should be recorded as failed"
  assert_eq "3" "${#HYBRID_K8S_TEARDOWN_DELETED[@]}" "all three objects must be deleted"
  assert_true "$([[ ! -f "${KUBECTL_STUB_STATE_DIR}/pvc/${K8S_NS}__${K8S_PVC}.json" ]] && echo true || echo false)" "PVC must be gone"
  assert_true "$([[ ! -f "${KUBECTL_STUB_STATE_DIR}/pv/${K8S_PV}.json" ]] && echo true || echo false)" "PV must be gone"
  assert_true "$([[ ! -f "${KUBECTL_STUB_STATE_DIR}/namespace/${K8S_NS}.json" ]] && echo true || echo false)" "namespace must be gone"
  rm -f "$HYBRID_KUBECONFIG"
}

# =====================================================================
# Firewall rules: names, marker, target tag, shape, reuse, and
# spec-drift verification on reuse.
# =====================================================================

test_firewall_rules_created_with_names_marker_tag_and_shape() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"

  assert_eq "$ALLOW_NAME" "$HYBRID_ALLOW_NAME" "allow rule name"
  assert_eq "$DENY_NAME" "$HYBRID_DENY_NAME" "deny rule name"

  local log allow_line deny_line
  log="$(gcloud_log)"
  allow_line="$(echo "$log" | grep "firewall-rules create ${ALLOW_NAME} ")"
  deny_line="$(echo "$log" | grep "firewall-rules create ${DENY_NAME} ")"

  assert_contains "$allow_line" "--description=${MARKER}" "allow rule carries the exact ownership marker"
  assert_contains "$deny_line" "--description=${MARKER}" "deny rule carries the exact ownership marker"

  assert_contains "$allow_line" "--target-tags=${TARGET_TAG}" "allow rule has the deployment-specific target tag"
  assert_contains "$deny_line" "--target-tags=${TARGET_TAG}" "deny rule has the deployment-specific target tag"

  assert_contains "$allow_line" "--network=${NETWORK}" "allow rule is on the hub's network"
  assert_contains "$allow_line" "--direction=INGRESS" "allow rule is ingress"
  assert_contains "$allow_line" "--action=ALLOW" "allow rule action"
  assert_contains "$allow_line" "--rules=tcp:2049" "allow rule port"
  assert_contains "$allow_line" "--source-tags=${GKE_NODE_TAG}" "allow rule sources from the discovered node tag"
  assert_contains "$allow_line" "--priority=900" "allow rule priority"

  assert_contains "$deny_line" "--network=${NETWORK}" "deny rule is on the hub's network"
  assert_contains "$deny_line" "--direction=INGRESS" "deny rule is ingress"
  assert_contains "$deny_line" "--action=DENY" "deny rule action"
  assert_contains "$deny_line" "--rules=tcp:2049" "deny rule port"
  assert_contains "$deny_line" "--source-ranges=0.0.0.0/0" "deny rule denies the world"
  assert_contains "$deny_line" "--priority=950" "deny rule priority"
}

test_hub_deny_rule_created_with_name_marker_tag_and_shape() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_eq "$HUB_DENY_NAME" "$HYBRID_HUB_DENY_NAME" "hub-deny rule name"
  local hub_deny_line
  hub_deny_line="$(gcloud_log | grep "firewall-rules create ${HUB_DENY_NAME} ")"
  assert_contains "$hub_deny_line" "--description=${MARKER}" "hub-deny rule carries the exact ownership marker"
  assert_contains "$hub_deny_line" "--target-tags=${TARGET_TAG}" "hub-deny rule targets the hub's own network tag"
  assert_contains "$hub_deny_line" "--network=${NETWORK}" "hub-deny rule is on the hub's network"
  assert_contains "$hub_deny_line" "--direction=INGRESS" "hub-deny rule is ingress"
  assert_contains "$hub_deny_line" "--action=DENY" "hub-deny rule action"
  assert_contains "$hub_deny_line" "--rules=all " "hub-deny rule denies all protocols and ports"
  assert_contains "$hub_deny_line" "--source-ranges=${GKE_POD_CIDR}" "hub-deny rule sources from the discovered pod CIDR"
  assert_contains "$hub_deny_line" "--priority=950" "hub-deny rule uses the nfs-deny priority scheme"
}

# =====================================================================
# Cloud Run egress / hub-deny overlap check.
# =====================================================================

test_hub_deny_overlap_check_refuses_overlapping_range() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  # The Cloud Run proxy's egress subnet ("default") has a primary range
  # that fully contains the discovered pod CIDR.
  seed_subnet "$NETWORK" "10.48.0.0/12"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "an overlapping Cloud Run egress range must refuse before creating any rule"
  assert_contains "$RUN_OUTPUT" "overlaps" "error should explain the overlap"
  assert_eq "0" "$(gcloud_log | grep -c 'firewall-rules create' || true)" \
    "nothing must be created before the overlap check passes"
}

test_hub_deny_overlap_check_refuses_on_subnet_describe_failure() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  set_subnet_describe_will_fail "$NETWORK"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a subnet describe failure must fail closed, not be treated as no overlap"
  assert_contains "$RUN_OUTPUT" "Could not describe subnet" "error should explain the describe failure"
  assert_eq "0" "$(gcloud_log | grep -c 'firewall-rules create' || true)" \
    "nothing must be created when the overlap check itself can't complete"
}

# CIDR blocks either nest or are disjoint, so "overlap" has two shapes:
# the subnet containing the pod CIDR (above) and the pod CIDR containing
# the subnet (here). Both must refuse.
test_hub_deny_overlap_check_refuses_subnet_inside_pod_cidr() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_subnet "$NETWORK" "10.53.16.0/20"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a Cloud Run egress range inside the pod CIDR must refuse too"
  assert_contains "$RUN_OUTPUT" "overlaps" "error should explain the overlap"
  assert_eq "0" "$(gcloud_log | grep -c 'firewall-rules create' || true)" \
    "nothing must be created before the overlap check passes"
}

test_hub_deny_overlap_check_refuses_empty_subnet_range() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_subnet "$NETWORK" ""
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a subnet with no primary range must refuse, not be read as no overlap"
  assert_contains "$RUN_OUTPUT" "has no primary IP range" "error should explain the missing range"
  assert_eq "0" "$(gcloud_log | grep -c 'firewall-rules create' || true)" \
    "nothing must be created when the range is unknown"
}

test_hub_deny_overlap_check_refuses_unparsable_subnet_range() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_subnet "$NETWORK" "not-a-cidr"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a range the comparison cannot parse must refuse, not be read as no overlap"
  assert_contains "$RUN_OUTPUT" "Could not compare subnet" "error should explain the failed comparison"
  assert_eq "0" "$(gcloud_log | grep -c 'firewall-rules create' || true)" \
    "nothing must be created when the comparison fails"
}

test_hub_deny_overlap_check_passes_on_disjoint_range() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_subnet "$NETWORK" "10.128.0.0/20"
  hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_eq "1" "$(gcloud_log | grep -c "firewall-rules create ${HUB_DENY_NAME} " || true)" \
    "a disjoint egress range must let hub-deny be created"
}

# A rule this tier just created must itself pass its own reuse check on
# the very next run -- otherwise every real deploy.sh re-run would fail
# immediately after its own first-time creation.
test_firewall_rules_idempotent_across_two_runs() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  local log
  log="$(gcloud_log)"
  assert_eq "1" "$(echo "$log" | grep -c "firewall-rules create ${ALLOW_NAME} ")" \
    "the allow rule must be created exactly once across two runs"
  assert_eq "1" "$(echo "$log" | grep -c "firewall-rules create ${DENY_NAME} ")" \
    "the deny rule must be created exactly once across two runs"
}

test_firewall_rule_reused_when_marked_and_matching() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_json "$ALLOW_NAME" "$MARKER" "$NETWORK" "INGRESS" "ALLOW" "tcp" "2049" \
    "$GKE_NODE_TAG" "" "$TARGET_TAG" "900"
  seed_firewall_rule_json "$DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "950"
  seed_firewall_rule_json "$HUB_DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "all" "" \
    "" "$GKE_POD_CIDR" "$TARGET_TAG" "950"
  hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_eq "0" "$(gcloud_log | grep -c 'firewall-rules create' || true)" \
    "an already-marked, matching-spec rule triple must not be recreated"
}

test_firewall_rule_refused_when_unmarked() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_desc_only "$ALLOW_NAME" "some other unrelated rule"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unmarked name collision must fail the run"
  assert_contains "$RUN_OUTPUT" "does not carry this deployment's marker" "error should explain the refusal"
}

# Same protection as above, but for the hub-deny rule specifically: it
# must not get a pass just because it's the newest of the three
# rules this function manages.
test_firewall_hub_deny_rule_refused_when_unmarked() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_desc_only "$HUB_DENY_NAME" "some other unrelated rule"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unmarked hub-deny name collision must fail the run"
  assert_contains "$RUN_OUTPUT" "does not carry this deployment's marker" "error should explain the refusal"
}

# The marker check is an exact match, not a substring: a rule owned by a
# different, prefix-colliding hub name must not be treated as a match.
test_firewall_marker_check_is_exact_not_substring() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_desc_only "$ALLOW_NAME" "scion-deployment=${HUB}2"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a marker for a different, prefix-colliding hub must not be treated as a match"
  # Specifically the marker check, not merely "failed for some reason" (a
  # substring-match regression here would fall through to the spec-drift
  # check instead and fail with different text, which must not count as a
  # pass for this test).
  assert_contains "$RUN_OUTPUT" "does not carry this deployment's marker" \
    "must fail at the marker check itself, not some other check"
}

# --- Spec drift on an already-marked rule: fail, list the differing
# fields, and print a remediation command. Never auto-correct. ------------

test_drift_widened_allow_source_fails_with_remediation() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  # Drifted: source is some other tag than the currently discovered one --
  # the realistic case is a recreated cluster whose new node tag no longer
  # matches what the rule was created with.
  seed_firewall_rule_json "$ALLOW_NAME" "$MARKER" "$NETWORK" "INGRESS" "ALLOW" "tcp" "2049" \
    "some-stale-tag" "" "$TARGET_TAG" "900"
  seed_firewall_rule_json "$DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "950"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a widened/changed source must fail the run"
  assert_contains "$RUN_OUTPUT" "$ALLOW_NAME" "drift output should name the drifted rule"
  assert_contains "$RUN_OUTPUT" "some-stale-tag" "drift output should show the actual (drifted) source"
  assert_contains "$RUN_OUTPUT" "${GKE_NODE_TAG}" "drift output should show the expected source"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "drift output should include a runnable delete remediation"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "a source-only drift should also offer an in-place update remediation"
  assert_contains "$RUN_OUTPUT" "--source-tags=${GKE_NODE_TAG}" "the update remediation should restore the expected source"
}

test_drift_lower_deny_priority_fails_with_remediation() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_json "$ALLOW_NAME" "$MARKER" "$NETWORK" "INGRESS" "ALLOW" "tcp" "2049" \
    "$GKE_NODE_TAG" "" "$TARGET_TAG" "900"
  # Drifted: priority lowered from 950.
  seed_firewall_rule_json "$DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "100"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a lowered priority must fail the run"
  assert_contains "$RUN_OUTPUT" "$DENY_NAME" "drift output should name the drifted rule"
  assert_contains "$RUN_OUTPUT" "priority" "drift output should mention the drifted field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${DENY_NAME}" "drift output should include a runnable delete remediation"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${DENY_NAME}" "a priority-only drift should also offer an in-place update remediation"
  assert_contains "$RUN_OUTPUT" "--priority=950" "the update remediation should restore the expected priority"
}

test_drift_hub_deny_narrowed_source_fails_with_remediation() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_json "$ALLOW_NAME" "$MARKER" "$NETWORK" "INGRESS" "ALLOW" "tcp" "2049" \
    "$GKE_NODE_TAG" "" "$TARGET_TAG" "900"
  seed_firewall_rule_json "$DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "950"
  # Drifted: the hub-deny rule's source narrowed away from the whole
  # discovered pod CIDR, which would let some pods reach the hub the
  # rule is supposed to block.
  seed_firewall_rule_json "$HUB_DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "all" "" \
    "" "10.52.0.0/16" "$TARGET_TAG" "950"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a hub-deny rule narrowed away from the discovered pod CIDR must fail the run, not be adopted as-is"
  assert_contains "$RUN_OUTPUT" "$HUB_DENY_NAME" "drift output should name the drifted rule"
  assert_contains "$RUN_OUTPUT" "10.52.0.0/16" "drift output should show the actual (drifted) source"
  assert_contains "$RUN_OUTPUT" "${GKE_POD_CIDR}" "drift output should show the expected source"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${HUB_DENY_NAME}" "drift output should include a runnable delete remediation"
}

# The six tests below each drift exactly one more field of the hub-deny
# rule (everything else matching), isolating that the shared drift check
# -- already well covered through the NFS allow/deny rule fixtures above
# -- is actually reached for hub-deny too, not skipped for it.

test_drift_hub_deny_wrong_ports_fails() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_json "$ALLOW_NAME" "$MARKER" "$NETWORK" "INGRESS" "ALLOW" "tcp" "2049" \
    "$GKE_NODE_TAG" "" "$TARGET_TAG" "900"
  seed_firewall_rule_json "$DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "950"
  # A hub-deny that covers only tcp:8080 is narrower than the expected
  # all-protocol deny, and must be caught as drift, not adopted.
  seed_firewall_rule_json "$HUB_DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "8080" \
    "" "${GKE_POD_CIDR}" "$TARGET_TAG" "950"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a hub-deny rule limited to one port must fail the run, not be adopted as-is"
  assert_contains "$RUN_OUTPUT" "ports:" "drift output should name the drifted field"
  assert_contains "$RUN_OUTPUT" "expected 'all', found 'tcp:8080'" "drift output should show both protocol specs"
}

test_drift_hub_deny_wrong_priority_fails() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_json "$ALLOW_NAME" "$MARKER" "$NETWORK" "INGRESS" "ALLOW" "tcp" "2049" \
    "$GKE_NODE_TAG" "" "$TARGET_TAG" "900"
  seed_firewall_rule_json "$DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "950"
  seed_firewall_rule_json "$HUB_DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "all" "" \
    "" "${GKE_POD_CIDR}" "$TARGET_TAG" "800"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a hub-deny rule with the wrong priority must fail the run, not be adopted as-is"
  assert_contains "$RUN_OUTPUT" "priority:" "drift output should name the drifted field"
}

test_drift_hub_deny_wrong_target_tags_fails() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_json "$ALLOW_NAME" "$MARKER" "$NETWORK" "INGRESS" "ALLOW" "tcp" "2049" \
    "$GKE_NODE_TAG" "" "$TARGET_TAG" "900"
  seed_firewall_rule_json "$DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "950"
  seed_firewall_rule_json "$HUB_DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "all" "" \
    "" "${GKE_POD_CIDR}" "some-other-tag" "950"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a hub-deny rule with the wrong target tag must fail the run, not be adopted as-is"
  assert_contains "$RUN_OUTPUT" "target tags:" "drift output should name the drifted field"
}

test_drift_hub_deny_wrong_direction_fails() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_json "$ALLOW_NAME" "$MARKER" "$NETWORK" "INGRESS" "ALLOW" "tcp" "2049" \
    "$GKE_NODE_TAG" "" "$TARGET_TAG" "900"
  seed_firewall_rule_json "$DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "950"
  seed_firewall_rule_json "$HUB_DENY_NAME" "$MARKER" "$NETWORK" "EGRESS" "DENY" "all" "" \
    "" "${GKE_POD_CIDR}" "$TARGET_TAG" "950"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a hub-deny rule with the wrong direction must fail the run, not be adopted as-is"
  assert_contains "$RUN_OUTPUT" "direction:" "drift output should name the drifted field"
}

test_drift_hub_deny_wrong_action_fails() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_json "$ALLOW_NAME" "$MARKER" "$NETWORK" "INGRESS" "ALLOW" "tcp" "2049" \
    "$GKE_NODE_TAG" "" "$TARGET_TAG" "900"
  seed_firewall_rule_json "$DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "950"
  # Drifted: someone flipped hub-deny to ALLOW, which would open the
  # port this rule exists to block.
  seed_firewall_rule_json "$HUB_DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "ALLOW" "tcp" "8080" \
    "" "${GKE_POD_CIDR}" "$TARGET_TAG" "950"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a hub-deny rule with the wrong action must fail the run, not be adopted as-is"
  assert_contains "$RUN_OUTPUT" "action:" "drift output should name the drifted field"
}

test_drift_hub_deny_disabled_fails() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  seed_firewall_rule_json "$ALLOW_NAME" "$MARKER" "$NETWORK" "INGRESS" "ALLOW" "tcp" "2049" \
    "$GKE_NODE_TAG" "" "$TARGET_TAG" "900"
  seed_firewall_rule_json "$DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "950"
  seed_firewall_rule_json "$HUB_DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "all" "" \
    "" "${GKE_POD_CIDR}" "$TARGET_TAG" "950" "" "" "" "true"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a disabled hub-deny rule must fail the run, not be adopted as-is"
  assert_contains "$RUN_OUTPUT" "disabled:" "drift output should name the drifted field"
}

test_drift_action_change_offers_delete_but_not_update() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  # Drifted: someone flipped the allow rule to DENY. `update` cannot
  # change action, so only the delete remediation should be offered.
  seed_firewall_rule_json "$ALLOW_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "900"
  seed_firewall_rule_json "$DENY_NAME" "$MARKER" "$NETWORK" "INGRESS" "DENY" "tcp" "2049" \
    "" "0.0.0.0/0" "$TARGET_TAG" "950"
  run_expect_fail hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an action change must fail the run"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "an action drift should still offer the delete remediation"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "an action drift must not offer an in-place update (unsupported by update)"
}

# --- VM tag ------------------------------------------------------------
test_vm_tag_value() {
  assert_eq "$TARGET_TAG" "$(hybrid_vm_tag "$HUB")" "VM tag derivation"
}

test_apply_vm_tag_idempotent() {
  fresh_gcloud_state
  hybrid_apply_vm_tag "scion-hub-${HUB}" "us-central1-b" "$PROJECT" "$HUB"
  hybrid_apply_vm_tag "scion-hub-${HUB}" "us-central1-b" "$PROJECT" "$HUB"
  local log
  log="$(gcloud_log)"
  assert_eq "2" "$(echo "$log" | grep -c 'instances add-tags')" "add-tags should be callable repeatedly without error"
  assert_contains "$log" "--tags=${TARGET_TAG}" "add-tags should use the derived VM tag"
}

# --- pre-existing non-hybrid hub, re-run with the tier newly enabled -------
test_reenable_on_preexisting_nonhybrid_hub() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  # No pre-existing firewall-rules state: this hub predates the hybrid tier,
  # so the rules don't exist yet even though the VM/hub itself does.
  hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  hybrid_apply_vm_tag "scion-hub-${HUB}" "us-central1-b" "$PROJECT" "$HUB"
  assert_eq "1" "$(gcloud_log | grep -c "firewall-rules create ${ALLOW_NAME} " || true)" \
    "hybrid rules should be created fresh on an existing, previously non-hybrid hub"
  assert_eq "1" "$(gcloud_log | grep -c 'instances add-tags' || true)" \
    "the existing VM should get the tag idempotently"
}

# =====================================================================
# Teardown
# =====================================================================

test_teardown_check_all_marked() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  seed_firewall_rule_desc_only "$DENY_NAME" "$MARKER"
  hybrid_teardown_check "$HUB" "$PROJECT"
  assert_eq "false" "$HYBRID_TEARDOWN_FAILED" "an all-marked pair should not fail teardown"
  assert_eq "2" "${#HYBRID_TEARDOWN_DELETE[@]}" "both marked rules should be queued for deletion"
  assert_eq "0" "${#HYBRID_TEARDOWN_SKIP[@]}" "nothing should be skipped"

  hybrid_teardown_delete "$PROJECT"
  local log
  log="$(gcloud_log)"
  assert_eq "1" "$(echo "$log" | grep -c "firewall-rules delete ${ALLOW_NAME}" || true)" \
    "the allow rule should be deleted"
  assert_eq "1" "$(echo "$log" | grep -c "firewall-rules delete ${DENY_NAME}" || true)" \
    "the deny rule should be deleted"
}

# name=(A B C) is an equality match against any of the listed names.
test_teardown_check_list_filter_form() {
  fresh_gcloud_state
  hybrid_teardown_check "$HUB" "$PROJECT"
  assert_eq "compute firewall-rules list --project=${PROJECT} --filter=name=(scion-hub-${HUB}-nfs-allow scion-hub-${HUB}-nfs-deny scion-hub-${HUB}-hub-deny) --format=json" \
    "$(gcloud_log | grep '^compute firewall-rules list' || true)" \
    "the ownership check must list the tier's three rules by exact name, in exactly this filter form"
}

test_teardown_check_none_found() {
  fresh_gcloud_state
  hybrid_teardown_check "$HUB" "$PROJECT"
  assert_eq "false" "$HYBRID_TEARDOWN_FAILED" "no rules present is not a failure"
  assert_eq "0" "${#HYBRID_TEARDOWN_DELETE[@]}" "nothing to delete"
  assert_eq "0" "${#HYBRID_TEARDOWN_SKIP[@]}" "nothing to skip"
}

test_teardown_check_unmarked_fails_and_skips() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  seed_firewall_rule_desc_only "$DENY_NAME" "unrelated-rule-not-ours"
  hybrid_teardown_check "$HUB" "$PROJECT"
  assert_eq "true" "$HYBRID_TEARDOWN_FAILED" "an unmarked name match must fail the teardown run"
  assert_eq "1" "${#HYBRID_TEARDOWN_DELETE[@]}" "the marked rule is still queued"
  assert_eq "1" "${#HYBRID_TEARDOWN_SKIP[@]}" "the unmarked rule is listed as SKIPPED"
  assert_eq "$DENY_NAME" "${HYBRID_TEARDOWN_SKIP[0]:-}" "the deny rule is the one skipped"
}

test_teardown_check_unmarked_hub_deny_fails_and_skips() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  seed_firewall_rule_desc_only "$HUB_DENY_NAME" "unrelated-rule-not-ours"
  hybrid_teardown_check "$HUB" "$PROJECT"
  assert_eq "true" "$HYBRID_TEARDOWN_FAILED" "an unmarked hub-deny name match must fail the teardown run too, not just the two NFS rules"
  assert_eq "1" "${#HYBRID_TEARDOWN_DELETE[@]}" "the marked rule is still queued"
  assert_eq "1" "${#HYBRID_TEARDOWN_SKIP[@]}" "the unmarked hub-deny rule is listed as SKIPPED"
  assert_eq "$HUB_DENY_NAME" "${HYBRID_TEARDOWN_SKIP[0]:-}" "the hub-deny rule is the one skipped"
}

test_teardown_delete_only_deletes_queued() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  hybrid_teardown_check "$HUB" "$PROJECT"
  hybrid_teardown_delete "$PROJECT"
  assert_true "$([[ -f "${GCLOUD_STUB_STATE_DIR}/firewall-rules/${ALLOW_NAME}.json" ]] && echo false || echo true)" \
    "the marked rule should actually be deleted from stub state"
}

# --- Create order: deny before allow, so an allow rule can never exist
# without its paired deny. -------------------------------------------------
test_firewall_rules_created_deny_before_allow() {
  fresh_gcloud_state
  GKE_NODE_TAG="gke-democluster-abc12345-node"
  GKE_POD_CIDR="10.52.0.0/14"
  hybrid_ensure_firewall_rules "$HUB" "$PROJECT" "$NETWORK" "$REGION"
  local log allow_line_num deny_line_num
  log="$(gcloud_log)"
  allow_line_num="$(echo "$log" | grep -n "firewall-rules create ${ALLOW_NAME} " | head -1 | cut -d: -f1)"
  deny_line_num="$(echo "$log" | grep -n "firewall-rules create ${DENY_NAME} " | head -1 | cut -d: -f1)"
  assert_true "$([[ -n "$deny_line_num" && -n "$allow_line_num" && "$deny_line_num" -lt "$allow_line_num" ]] && echo true || echo false)" \
    "the deny rule must be created before the allow rule"
}

# --- Teardown delete order: allow before deny (the reverse of create). ---
test_teardown_delete_order_allow_before_deny() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  seed_firewall_rule_desc_only "$DENY_NAME" "$MARKER"
  hybrid_teardown_check "$HUB" "$PROJECT"
  hybrid_teardown_delete "$PROJECT"
  local log allow_line_num deny_line_num
  log="$(gcloud_log)"
  allow_line_num="$(echo "$log" | grep -n "firewall-rules delete ${ALLOW_NAME}" | head -1 | cut -d: -f1)"
  deny_line_num="$(echo "$log" | grep -n "firewall-rules delete ${DENY_NAME}" | head -1 | cut -d: -f1)"
  assert_true "$([[ -n "$allow_line_num" && -n "$deny_line_num" && "$allow_line_num" -lt "$deny_line_num" ]] && echo true || echo false)" \
    "the allow rule must be deleted before the deny rule"
}

# --- Teardown exact-marker matching: a prefix-colliding hub name, or a
# marker with trailing text, must both be treated as unmarked. -----------
test_teardown_marker_prefix_collision_is_skipped() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "scion-deployment=${HUB}2"
  hybrid_teardown_check "$HUB" "$PROJECT"
  assert_eq "true" "$HYBRID_TEARDOWN_FAILED" "a prefix-colliding marker must not be treated as a match"
  assert_eq "1" "${#HYBRID_TEARDOWN_SKIP[@]}" "the rule should be SKIPPED, not queued for deletion"
  assert_eq "0" "${#HYBRID_TEARDOWN_DELETE[@]}" "nothing should be queued for deletion"
}

test_teardown_marker_with_trailing_text_is_skipped() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "${MARKER} extra"
  hybrid_teardown_check "$HUB" "$PROJECT"
  assert_eq "true" "$HYBRID_TEARDOWN_FAILED" "a marker with trailing text must not be treated as a match"
  assert_eq "1" "${#HYBRID_TEARDOWN_SKIP[@]}" "the rule should be SKIPPED, not queued for deletion"
  assert_eq "0" "${#HYBRID_TEARDOWN_DELETE[@]}" "nothing should be queued for deletion"
}

# --- An unmarked rule with an otherwise fully matching spec must still be
# refused at the marker check -- proving the run actually stops there,
# not that some later check happens to also reject it. --------------------
test_firewall_rule_unmarked_but_fully_matching_spec_refused() {
  fresh_gcloud_state
  seed_firewall_rule_json "$ALLOW_NAME" "some-unrelated-description" "$NETWORK" "INGRESS" "ALLOW" "tcp" "2049" \
    "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "900"
  run_expect_fail _hybrid_ensure_firewall_rule "$ALLOW_NAME" "$PROJECT" "$MARKER" \
    "default" "INGRESS" "ALLOW" "tcp:2049" "tag" "$DRIFT_NODE_TAG" "$TARGET_TAG" "900"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "an unmarked rule must be refused even when its spec fully matches"
  assert_eq "0" "$(gcloud_log | grep -c 'firewall-rules create' || true)" \
    "nothing should be created when an unmarked rule blocks the run"
}

# =====================================================================
# hybrid_teardown_preflight: the extracted, directly testable teardown
# safety check (classification printing + abort decision).
# =====================================================================

test_teardown_preflight_none_found_succeeds_silently() {
  fresh_gcloud_state
  local out
  out="$(hybrid_teardown_preflight "$HUB" "$PROJECT")"
  local rc=$?
  assert_eq "0" "$rc" "no rules present should not abort"
  assert_eq "" "$out" "nothing found means nothing to print"
}

test_teardown_preflight_marked_prints_found_and_succeeds() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  seed_firewall_rule_desc_only "$DENY_NAME" "$MARKER"
  local out
  out="$(hybrid_teardown_preflight "$HUB" "$PROJECT")"
  local rc=$?
  assert_eq "0" "$rc" "an all-marked pair should not abort"
  assert_contains "$out" "found (marked): ${ALLOW_NAME}" "should print the documented found-marked line"
  assert_contains "$out" "found (marked): ${DENY_NAME}" "should print the documented found-marked line"
}

test_teardown_preflight_unmarked_prints_skipped_and_aborts() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  seed_firewall_rule_desc_only "$DENY_NAME" "unrelated-rule-not-ours"
  run_expect_fail hybrid_teardown_preflight "$HUB" "$PROJECT"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unmarked match must abort"
  assert_contains "$RUN_OUTPUT" "found (marked): ${ALLOW_NAME}" "should still print the marked rule as found"
  assert_contains "$RUN_OUTPUT" "SKIPPED (unmarked): ${DENY_NAME}" "should print the documented SKIPPED line"
  assert_contains "$RUN_OUTPUT" "Refusing to tear down" "should explain the abort"
}

test_teardown_preflight_list_failure_aborts_with_no_classification() {
  fresh_gcloud_state
  set_firewall_list_will_fail
  run_expect_fail hybrid_teardown_preflight "$HUB" "$PROJECT"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a failed list call must abort"
  assert_not_contains "$RUN_OUTPUT" "found (marked)" "a list failure is not the same as finding nothing"
  assert_not_contains "$RUN_OUTPUT" "SKIPPED (unmarked)" "a list failure is not an unmarked-match abort"
  assert_contains "$RUN_OUTPUT" "aborting teardown rather than assuming none exist" "should explain that the failure itself is the reason"
}

# --- hybrid_teardown_delete: a real delete failure (rule still present
# afterward) must be reported as a failure, never as "deleted". ----------
test_teardown_delete_failure_is_reported_not_deleted() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  set_firewall_delete_will_fail "$ALLOW_NAME"
  hybrid_teardown_check "$HUB" "$PROJECT"
  hybrid_teardown_delete "$PROJECT"
  assert_eq "1" "${#HYBRID_TEARDOWN_DELETE_FAILED[@]}" "the failed delete should be recorded as failed"
  # shellcheck disable=SC2153 # HYBRID_TEARDOWN_DELETED, set by hybrid_teardown_delete (hybrid-tier.sh), not a typo of HYBRID_TEARDOWN_DELETE
  assert_eq "0" "${#HYBRID_TEARDOWN_DELETED[@]}" "the failed delete must not be recorded as deleted"
  assert_true "$([[ -f "${GCLOUD_STUB_STATE_DIR}/firewall-rules/${ALLOW_NAME}.json" ]] && echo true || echo false)" \
    "the rule should still be present in stub state after a failed delete"
}

# =====================================================================
# Single-field drift battery: from a fully matching allow-rule
# seed, change exactly one field, and check the drift message names that
# field, always offers the delete remediation, and offers the update
# remediation only for fields `update` can converge in place (the
# expected-type source value, target tags, priority) -- never for
# direction, action, ports/rule-shape, the other source type, service
# accounts, disabled, or network.
# =====================================================================

DRIFT_NODE_TAG="gke-x-node"

# drift_seed_allow [EXTRA seed_firewall_rule_json ARGS...] — seeds
# $ALLOW_NAME with every field at its expected value, then overridden by
# any extra positional args the caller appends (seed_firewall_rule_json's
# own argument order: DESC NETWORK DIRECTION ACTION PROTO PORTS
# SOURCE_TAGS SOURCE_RANGES TARGET_TAGS PRIORITY [SOURCE_SAS [TARGET_SAS
# [DEST_RANGES [DISABLED [EXTRA_PROTO [EXTRA_PORTS]]]]]]).
drift_seed_allow() {
  seed_firewall_rule_json "$ALLOW_NAME" "$@"
}

drift_check_allow() {
  run_expect_fail _hybrid_ensure_firewall_rule "$ALLOW_NAME" "$PROJECT" "$MARKER" \
    "default" "INGRESS" "ALLOW" "tcp:2049" "tag" "$DRIFT_NODE_TAG" "$TARGET_TAG" "900"
}

test_drift_field_direction() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "EGRESS" "ALLOW" "tcp" "2049" "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "900"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "direction drift must fail"
  assert_contains "$RUN_OUTPUT" "direction:" "should name the direction field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_contains "$RUN_OUTPUT" "re-run deploy.sh" "an allow-rule delete remediation must also say to re-run deploy.sh, not just delete"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "update cannot change direction"
}

test_drift_field_action() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "DENY" "tcp" "2049" "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "900"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "action drift must fail"
  assert_contains "$RUN_OUTPUT" "action:" "should name the action field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "update cannot change action"
}

test_drift_field_ports() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "ALLOW" "tcp" "22" "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "900"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "ports drift must fail"
  assert_contains "$RUN_OUTPUT" "ports:" "should name the ports field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "update cannot change ports"
}

test_drift_field_extra_rule_entry() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "ALLOW" "tcp" "2049" "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "900" \
    "" "" "" "false" "all" ""
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an extra allowed entry must fail"
  assert_contains "$RUN_OUTPUT" "ports:" "an extra rule entry changes the ports/rules signature"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "update cannot remove a rule entry"
}

test_drift_field_source_tag_value() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "ALLOW" "tcp" "2049" "some-other-tag" "" "$TARGET_TAG" "900"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "source-tag drift must fail"
  assert_contains "$RUN_OUTPUT" "source tags:" "should name the source tags field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "a same-type source drift converges via update"
  assert_contains "$RUN_OUTPUT" "--source-tags=${DRIFT_NODE_TAG}" "the update remediation should restore the expected source tag"
}

test_drift_field_stray_source_ranges() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "ALLOW" "tcp" "2049" "$DRIFT_NODE_TAG" "0.0.0.0/0" "$TARGET_TAG" "900"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a stray sourceRanges must fail"
  assert_contains "$RUN_OUTPUT" "source ranges:" "should name the source ranges field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" \
    "update cannot clear a stray sourceRanges, so only delete may be offered"
}

test_drift_field_source_service_account() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "ALLOW" "tcp" "2049" "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "900" \
    "sa@example.iam.gserviceaccount.com"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a source service account must fail"
  assert_contains "$RUN_OUTPUT" "source service accounts:" "should name the source service accounts field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "update cannot clear a service account"
}

test_drift_field_disabled() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "ALLOW" "tcp" "2049" "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "900" \
    "" "" "" "true"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a disabled rule must fail"
  assert_contains "$RUN_OUTPUT" "disabled:" "should name the disabled field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "update is never offered for a disabled rule"
}

test_drift_field_priority() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "ALLOW" "tcp" "2049" "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "800"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "priority drift must fail"
  assert_contains "$RUN_OUTPUT" "priority:" "should name the priority field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "priority-only drift converges via update"
  assert_contains "$RUN_OUTPUT" "--priority=900" "the update remediation should restore the expected priority"
}

test_drift_field_target_tags() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "ALLOW" "tcp" "2049" "$DRIFT_NODE_TAG" "" "some-other-target" "900"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "target-tag drift must fail"
  assert_contains "$RUN_OUTPUT" "target tags:" "should name the target tags field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "target-tag-only drift converges via update"
  assert_contains "$RUN_OUTPUT" "--target-tags=${TARGET_TAG}" "the update remediation should restore the expected target tag"
}

test_drift_field_network() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "othernet" "INGRESS" "ALLOW" "tcp" "2049" "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "900"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "network drift must fail"
  assert_contains "$RUN_OUTPUT" "network:" "should name the network field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "update cannot change network"
}

test_drift_field_matching_reuse_passes() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "ALLOW" "tcp" "2049" "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "900"
  drift_check_allow
  assert_eq "0" "$RUN_EXIT_CODE" "a fully matching rule must be reused without error"
  assert_eq "0" "$(gcloud_log | grep -c 'firewall-rules create' || true)" "a fully matching rule must not be recreated"
}

# =====================================================================
# Discovery: regex anchoring
# =====================================================================

# The node-tag pattern is fully anchored: a tag that merely *contains*
# "gke-...-node" as a substring, rather than matching it exactly, must
# never be accepted as the target tag.

test_discover_anchor_rejects_tag_missing_end_anchor() {
  fresh_gcloud_state
  GKE_NAME="anchortest1"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "anchortest1" "$NETWORK"
  seed_pod_cidr "anchortest1" "10.44.0.0/17"
  seed_gke_node_tag_rules "anchortest1" "$NETWORK" "10.44.0.0/17" "gke-foo-node-y"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a tag whose start matches but doesn't end in -node must not be accepted"
  assert_contains "$RUN_OUTPUT" "gke-foo-node-y" "error should list the tag as seen, not silently ignore it"
}

test_discover_anchor_rejects_tag_missing_start_anchor() {
  fresh_gcloud_state
  GKE_NAME="anchortest2"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "anchortest2" "$NETWORK"
  seed_pod_cidr "anchortest2" "10.44.0.0/17"
  seed_gke_node_tag_rules "anchortest2" "$NETWORK" "10.44.0.0/17" "x-gke-foo-node"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a tag that ends in -node but doesn't start with gke- must not be accepted"
  assert_contains "$RUN_OUTPUT" "x-gke-foo-node" "error should list the tag as seen, not silently ignore it"
}

# The -all rule NAME pattern is also fully anchored: a rule name that
# merely starts with "gke-...-all" as a prefix, with trailing characters
# after it, must not be accepted as a candidate.
test_discover_anchor_rejects_rule_name_missing_end_anchor() {
  fresh_gcloud_state
  GKE_NAME="anchortest3"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "anchortest3" "$NETWORK"
  seed_pod_cidr "anchortest3" "10.44.0.0/17"
  clear_gke_node_tag_rules "anchortest3"
  # A -vms rule at the name an unanchored match would (wrongly) derive
  # by stripping the literal last 4 characters of "gke-anchortest3-allish"
  # ("gke-anchortest3-al-vms") is seeded too, agreeing on the tag: if the
  # end anchor were dropped, every other check would also pass and
  # discovery would succeed outright, giving a clean signal rather than a
  # different failure (a wrong-named missing -vms rule) that would make
  # this test pass either way.
  "$PYTHON" -c "
import json, sys
d, tag, network, project = sys.argv[1:5]
network_url = 'https://www.googleapis.com/compute/v1/projects/%s/global/networks/%s' % (project, network)
json.dump({
    'name': 'gke-anchortest3-allish', 'network': network_url, 'direction': 'INGRESS',
    'sourceRanges': ['10.44.0.0/17'], 'targetTags': [tag],
}, open(d + '/gke-anchortest3-allish.json', 'w'))
json.dump({
    'name': 'gke-anchortest3-al-vms', 'network': network_url, 'direction': 'INGRESS',
    'sourceRanges': ['10.128.0.0/9'], 'targetTags': [tag],
}, open(d + '/gke-anchortest3-al-vms.json', 'w'))
" "${GCLOUD_STUB_STATE_DIR}/gke-node-firewall-rules" "gke-anchortest3-x-node" "$NETWORK" "${GCLOUD_STUB_PROJECT:-demo-project}"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a rule name that starts with gke-...-all but doesn't end there must not be accepted"
  assert_contains "$RUN_OUTPUT" "missing or ambiguous" "error should say the cluster's firewall rules are missing or ambiguous"
}

test_discover_anchor_rejects_rule_name_missing_start_anchor() {
  fresh_gcloud_state
  GKE_NAME="anchortest4"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "anchortest4" "$NETWORK"
  seed_pod_cidr "anchortest4" "10.44.0.0/17"
  clear_gke_node_tag_rules "anchortest4"
  # A rule name ending in "gke-...-all" but with a leading prefix before
  # it -- an unanchored search() would still find "gke-...-all" starting
  # partway through the string; fullmatch must refuse it outright, not
  # just report it as an unrelated decoy. The matching -vms rule is
  # seeded too (agreeing on the tag), so a bypassed start anchor would
  # show up as an unexpected SUCCESS, not a different failure.
  "$PYTHON" -c "
import json, sys
d, tag, network, project = sys.argv[1:5]
network_url = 'https://www.googleapis.com/compute/v1/projects/%s/global/networks/%s' % (project, network)
json.dump({
    'name': 'x-gke-anchortest4-all', 'network': network_url, 'direction': 'INGRESS',
    'sourceRanges': ['10.44.0.0/17'], 'targetTags': [tag],
}, open(d + '/x-gke-anchortest4-all.json', 'w'))
json.dump({
    'name': 'x-gke-anchortest4-vms', 'network': network_url, 'direction': 'INGRESS',
    'sourceRanges': ['10.128.0.0/9'], 'targetTags': [tag],
}, open(d + '/x-gke-anchortest4-vms.json', 'w'))
" "${GCLOUD_STUB_STATE_DIR}/gke-node-firewall-rules" "gke-anchortest4-x-node" "$NETWORK" "${GCLOUD_STUB_PROJECT:-demo-project}"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a rule name ending in gke-...-all but prefixed with other characters must not be accepted"
  assert_contains "$RUN_OUTPUT" "no firewall rule matching" "error should give the no-candidate reason"
}

# drift_seed_deny / drift_check_deny — same pattern as the allow-side
# helpers above, for the deny rule's expected spec (range-type source,
# priority 950), used for the fields the allow-side battery can't reach:
# a stray sourceTags on a range-type rule, and the deny-specific
# order-preserving remediation.
drift_seed_deny() {
  seed_firewall_rule_json "$DENY_NAME" "$@"
}

drift_check_deny() {
  run_expect_fail _hybrid_ensure_firewall_rule "$DENY_NAME" "$PROJECT" "$MARKER" \
    "default" "INGRESS" "DENY" "tcp:2049" "range" "0.0.0.0/0" "$TARGET_TAG" "950"
}

test_drift_field_target_service_account() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "ALLOW" "tcp" "2049" "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "900" \
    "" "sa@example.iam.gserviceaccount.com"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a target service account must fail"
  assert_contains "$RUN_OUTPUT" "target service accounts:" "should name the target service accounts field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "update cannot clear a target service account"
}

test_drift_field_destination_ranges() {
  fresh_gcloud_state
  drift_seed_allow "$MARKER" "default" "INGRESS" "ALLOW" "tcp" "2049" "$DRIFT_NODE_TAG" "" "$TARGET_TAG" "900" \
    "" "" "192.0.2.0/24"
  drift_check_allow
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a destination range must fail"
  assert_contains "$RUN_OUTPUT" "destination ranges:" "should name the destination ranges field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "delete remediation must be present"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${ALLOW_NAME}" "update cannot clear a destination range"
}

# The deny rule's expected source type is "range" (0.0.0.0/0); a stray
# sourceTags value is the "other" source field for a range-type rule,
# the mirror image of test_drift_field_stray_source_ranges on the allow
# side, and it isn't reachable from an allow-seeded test.
test_drift_field_deny_stray_source_tags() {
  fresh_gcloud_state
  drift_seed_deny "$MARKER" "default" "INGRESS" "DENY" "tcp" "2049" "some-stray-tag" "0.0.0.0/0" "$TARGET_TAG" "950"
  drift_check_deny
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a stray sourceTags on the deny rule must fail"
  assert_contains "$RUN_OUTPUT" "source tags:" "should name the source tags field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${ALLOW_NAME}" "deny remediation deletes the allow rule first"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules delete ${DENY_NAME}" "deny remediation also deletes the deny rule"
  assert_not_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${DENY_NAME}" \
    "update cannot clear a stray sourceTags, so only delete may be offered"
}

# A deny rule narrowed to a specific range (rather than 0.0.0.0/0) is a
# same-type source drift, which does converge via update -- the mirror
# image of test_drift_field_source_tag_value on the allow side.
test_drift_field_deny_source_ranges_value() {
  fresh_gcloud_state
  drift_seed_deny "$MARKER" "default" "INGRESS" "DENY" "tcp" "2049" "" "10.0.0.0/8" "$TARGET_TAG" "950"
  drift_check_deny
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a narrowed deny source range must fail"
  assert_contains "$RUN_OUTPUT" "source ranges:" "should name the source ranges field"
  assert_contains "$RUN_OUTPUT" "gcloud compute firewall-rules update ${DENY_NAME}" "a same-type source drift on the deny rule converges via update"
  assert_contains "$RUN_OUTPUT" "--source-ranges=0.0.0.0/0" "the update remediation should restore the expected deny source"
}

# --- Deny-drift remediation preserves the allow-before-deny order
# deleting only the deny rule would leave tcp:2049 reachable
# through the allow rule with nothing to deny it. -------------------------
test_drift_deny_delete_only_remediation_preserves_order() {
  fresh_gcloud_state
  drift_seed_deny "$MARKER" "othernet" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "$TARGET_TAG" "950"
  drift_check_deny
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "network drift on the deny rule must fail"
  assert_contains "$RUN_OUTPUT" "re-run deploy.sh" "should tell the operator to re-run afterward"
  local allow_line deny_line
  allow_line="$(echo "$RUN_OUTPUT" | grep -n "delete ${ALLOW_NAME}" | head -1 | cut -d: -f1)"
  deny_line="$(echo "$RUN_OUTPUT" | grep -n "delete ${DENY_NAME}" | head -1 | cut -d: -f1)"
  assert_true "$([[ -n "$allow_line" && -n "$deny_line" && "$allow_line" -lt "$deny_line" ]] && echo true || echo false)" \
    "the remediation must delete the allow rule before the deny rule"
}

# =====================================================================
# Teardown delete: stop at the first real failure, and only a
# positive not-found counts as gone.
# =====================================================================

test_teardown_delete_stops_after_allow_failure_deny_survives() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  seed_firewall_rule_desc_only "$DENY_NAME" "$MARKER"
  set_firewall_delete_will_fail "$ALLOW_NAME"
  hybrid_teardown_check "$HUB" "$PROJECT"
  # Captured without a subshell (not run_expect_fail): hybrid_teardown_delete
  # sets HYBRID_TEARDOWN_DELETED/_FAILED as globals, and a command
  # substitution's subshell would discard those mutations before this
  # function could assert on them.
  local stderr_file
  stderr_file="$(mktemp)"
  hybrid_teardown_delete "$PROJECT" 2>"${stderr_file}"
  local stderr_output
  stderr_output="$(cat "${stderr_file}")"
  rm -f "${stderr_file}"
  assert_true "$([[ -f "${GCLOUD_STUB_STATE_DIR}/firewall-rules/${DENY_NAME}.json" ]] && echo true || echo false)" \
    "the deny rule must never be deleted after the allow delete failed"
  local n is_deny_deleted=false
  for n in ${HYBRID_TEARDOWN_DELETED[@]+"${HYBRID_TEARDOWN_DELETED[@]}"}; do
    [[ "$n" == "$DENY_NAME" ]] && is_deny_deleted=true
  done
  assert_eq "false" "$is_deny_deleted" "the deny rule must not be reported as deleted"
  assert_eq "2" "${#HYBRID_TEARDOWN_DELETE_FAILED[@]}" \
    "the deny rule queued behind the failed allow must be recorded as not deleted too"
  assert_contains "$stderr_output" "Not attempted (kept" \
    "the operator must be told the deny rule was never attempted, not just that the allow failed"
  assert_contains "$stderr_output" "Not attempted (kept so tcp:2049 stays denied): ${DENY_NAME}" \
    "the not-attempted line must name the specific queued rule"
  assert_eq "$DENY_NAME" "${HYBRID_TEARDOWN_DELETE_FAILED[1]:-}" \
    "the deny rule (not just some rule) must be the one recorded as queued-behind"
}

test_teardown_delete_not_attempted_message_per_rule() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  seed_firewall_rule_desc_only "$HUB_DENY_NAME" "$MARKER"
  seed_firewall_rule_desc_only "$DENY_NAME" "$MARKER"
  set_firewall_delete_will_fail "$ALLOW_NAME"
  HYBRID_TEARDOWN_DELETE=("$ALLOW_NAME" "$HUB_DENY_NAME" "$DENY_NAME")
  local stderr_file stderr_output
  stderr_file="$(mktemp)"
  hybrid_teardown_delete "$PROJECT" 2>"${stderr_file}"
  stderr_output="$(cat "${stderr_file}")"
  rm -f "${stderr_file}"
  assert_contains "$stderr_output" "Not attempted (kept so pod-range traffic to the hub VM stays denied): ${HUB_DENY_NAME}" \
    "the not-attempted line for the pod-range deny must say what it keeps denied"
  assert_contains "$stderr_output" "Not attempted (kept so tcp:2049 stays denied): ${DENY_NAME}" \
    "the not-attempted line for the NFS deny must say what it keeps denied"
  assert_not_contains "$stderr_output" "tcp:2049 stays denied): ${HUB_DENY_NAME}" \
    "the pod-range deny must not be described as the NFS deny"
}

test_teardown_delete_confirms_already_gone_via_list() {
  fresh_gcloud_state
  HYBRID_TEARDOWN_DELETE=("$ALLOW_NAME")
  hybrid_teardown_delete "$PROJECT"
  assert_eq "1" "${#HYBRID_TEARDOWN_DELETED[@]}" "a rule already gone (delete fails, list confirms absent) should count as deleted"
  assert_eq "0" "${#HYBRID_TEARDOWN_DELETE_FAILED[@]}" "a positively-confirmed absence is not a failure"
  assert_eq "compute firewall-rules list --project=${PROJECT} --filter=name=(${ALLOW_NAME}) --format=json" \
    "$(gcloud_log | grep '^compute firewall-rules list' || true)" \
    "the absence check must list the rule by exact name, in exactly this filter form"
}

test_teardown_delete_recheck_list_failure_is_failure_not_gone() {
  fresh_gcloud_state
  HYBRID_TEARDOWN_DELETE=("$ALLOW_NAME")
  set_firewall_list_will_fail
  hybrid_teardown_delete "$PROJECT"
  assert_eq "0" "${#HYBRID_TEARDOWN_DELETED[@]}" "an unknown re-check must never count as deleted"
  assert_eq "1" "${#HYBRID_TEARDOWN_DELETE_FAILED[@]}" "an unknown re-check is a failure, not a gone rule"
}

# At the function level: seed an unmarked deny alongside a
# marked allow, run check then delete, and confirm hybrid_teardown_delete
# itself never touches the unmarked one -- not just that its caller
# happens to never call it with one queued.
test_teardown_delete_only_deletes_queued_not_skipped() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  seed_firewall_rule_desc_only "$DENY_NAME" "unrelated-rule-not-ours"
  hybrid_teardown_check "$HUB" "$PROJECT"
  hybrid_teardown_delete "$PROJECT"
  assert_true "$([[ -f "${GCLOUD_STUB_STATE_DIR}/firewall-rules/${DENY_NAME}.json" ]] && echo true || echo false)" \
    "hybrid_teardown_delete must never delete a rule that was only SKIPPED, not queued"
}

# =====================================================================
# Teardown check: python is only needed when something was found to
# classify.
# =====================================================================

test_teardown_check_no_python_needed_when_nothing_matches() {
  fresh_gcloud_state
  local saved_python="$PYTHON"
  PYTHON="/nonexistent/python3"
  hybrid_teardown_check "$HUB" "$PROJECT"
  PYTHON="$saved_python"
  assert_eq "false" "$HYBRID_TEARDOWN_FAILED" "no rules present means python is never needed"
}

test_teardown_check_python_missing_with_rules_fails_closed() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$ALLOW_NAME" "$MARKER"
  local saved_python="$PYTHON"
  PYTHON="/nonexistent/python3"
  hybrid_teardown_check "$HUB" "$PROJECT"
  PYTHON="$saved_python"
  assert_eq "true" "$HYBRID_TEARDOWN_FAILED" "missing python with rules present must fail closed"
  assert_eq "0" "${#HYBRID_TEARDOWN_SKIP[@]}" "must not falsely report rules as SKIPPED (unmarked) when the real problem is python"
}

# =====================================================================
# gke_target config validation
# =====================================================================

test_config_name_leading_hyphen_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="-mycluster"
  CONFIG["gke_target.location"]="us-central1"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a leading hyphen must be refused"
  assert_contains "$RUN_OUTPUT" "not a valid GKE cluster name" "error should name the field"
}

test_config_name_uppercase_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="MyCluster"
  CONFIG["gke_target.location"]="us-central1"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an uppercase cluster name must be refused"
}

test_config_name_trailing_hyphen_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster-"
  CONFIG["gke_target.location"]="us-central1"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a trailing hyphen must be refused"
}

test_config_location_invalid_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  CONFIG["gke_target.location"]="us-central"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a location missing its trailing digits must be refused"
  assert_contains "$RUN_OUTPUT" "not a valid GCP zone or region" "error should name the field"
}

test_config_project_invalid_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  CONFIG["gke_target.location"]="us-central1"
  CONFIG["gke_target.project"]="Not_A_Valid_Project"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an invalid project ID must be refused"
  assert_contains "$RUN_OUTPUT" "not a valid GCP project ID" "error should name the field"
}

test_config_image_size_gb_non_numeric_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  CONFIG["gke_target.location"]="us-central1"
  CONFIG["gke_target.shared_dir_image_size_gb"]="lots"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a non-numeric image size must be refused"
  assert_contains "$RUN_OUTPUT" "must be a positive integer" "error should name the field"
}

test_config_image_size_gb_zero_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  CONFIG["gke_target.location"]="us-central1"
  CONFIG["gke_target.shared_dir_image_size_gb"]="0"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a zero image size must be refused"
  assert_contains "$RUN_OUTPUT" "must be a positive integer" "error should name the field"
}

test_config_python_preflight_refused() {
  fresh_gcloud_state
  CONFIG["gke_target.name"]="mycluster"
  # shellcheck disable=SC2034 # read by config_get (harness.sh) via hybrid_read_config
  CONFIG["gke_target.location"]="us-central1"
  local saved_python="$PYTHON"
  PYTHON="/nonexistent/python3"
  run_expect_fail hybrid_read_config "$PROJECT" "$HUB"
  PYTHON="$saved_python"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a missing python interpreter must be refused when the tier is enabled"
  assert_contains "$RUN_OUTPUT" "Python interpreter" "error should name the problem"
}

# =====================================================================
# _hybrid_registry_is_loopback: every spelling of "this VM itself" that
# a container_images.registry value could take, tested one clause at a
# time so a single collapsed/overbroad pattern can't hide behind another
# passing case.
# =====================================================================

test_registry_loopback_bare_localhost() {
  assert_true "$(_hybrid_registry_is_loopback 'localhost/scion' && echo true || echo false)" \
    "a bare localhost/... registry must be refused"
}
test_registry_loopback_localhost_with_port() {
  assert_true "$(_hybrid_registry_is_loopback 'localhost:5000/scion' && echo true || echo false)" \
    "localhost with an explicit port must be refused"
}
test_registry_loopback_127_bare() {
  assert_true "$(_hybrid_registry_is_loopback '127.0.0.1/scion' && echo true || echo false)" \
    "a bare 127.0.0.1 registry must be refused"
}
test_registry_loopback_127_with_port() {
  assert_true "$(_hybrid_registry_is_loopback '127.0.0.1:5000/scion' && echo true || echo false)" \
    "127.0.0.1 with a port must be refused"
}
test_registry_loopback_127_other_host() {
  assert_true "$(_hybrid_registry_is_loopback '127.5.5.5/scion' && echo true || echo false)" \
    "the whole 127.0.0.0/8 range must be refused, not just 127.0.0.1"
}
test_registry_loopback_ipv6_bare() {
  assert_true "$(_hybrid_registry_is_loopback '[::1]/scion' && echo true || echo false)" \
    "a bare IPv6 loopback literal must be refused"
}
test_registry_loopback_ipv6_with_port() {
  assert_true "$(_hybrid_registry_is_loopback '[::1]:5000/scion' && echo true || echo false)" \
    "an IPv6 loopback literal with a port must be refused"
}
test_registry_loopback_zero_address() {
  assert_true "$(_hybrid_registry_is_loopback '0.0.0.0:5000/scion' && echo true || echo false)" \
    "0.0.0.0 (binds-everywhere, reaches the node itself) must be refused"
}
test_registry_loopback_rejects_legitimate_registry() {
  assert_true "$(_hybrid_registry_is_loopback 'us-docker.pkg.dev/demo-project/scion' && echo false || echo true)" \
    "a real Artifact Registry path must never be refused"
}
test_registry_loopback_rejects_lookalike_host() {
  assert_true "$(_hybrid_registry_is_loopback '1270.0.0.1/scion' && echo false || echo true)" \
    "a host that merely starts with the digits 127 but isn't in 127.0.0.0/8 must not be refused"
}

# =====================================================================
# Pod CIDR discovery: cross-checked against two fields in the same
# cluster describe JSON, used by the hub-deny firewall rule.
# =====================================================================

test_discover_pod_cidr_from_cluster_fixture() {
  fresh_gcloud_state
  GKE_NAME="podcidrcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "podcidrcluster" "$NETWORK"
  seed_pod_cidr "podcidrcluster" "10.60.0.0/14"
  hybrid_discover "$NETWORK"
  assert_eq "10.60.0.0/14" "$GKE_POD_CIDR" "must read the actual pod CIDR, not a hardcoded default"
}

test_discover_pod_cidr_mismatch_refused() {
  fresh_gcloud_state
  GKE_NAME="mismatchcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "mismatchcluster" "$NETWORK"
  seed_pod_cidr_mismatch "mismatchcluster" "10.60.0.0/14" "10.61.0.0/14"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "disagreeing pod CIDR fields must refuse before any create"
  assert_contains "$RUN_OUTPUT" "two different pod CIDRs" "error should explain why"
}

test_discover_pod_cidr_ignores_services_cidr() {
  fresh_gcloud_state
  GKE_NAME="svccidrcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "svccidrcluster" "$NETWORK"
  # A real cluster describe also carries servicesIpv4Cidr (the Service
  # range, not the pod range) alongside clusterIpv4Cidr -- make sure it's
  # never read as if it were the pod CIDR.
  seed_pod_cidr "svccidrcluster" "10.60.0.0/14" "10.70.0.0/20"
  hybrid_discover "$NETWORK"
  assert_eq "10.60.0.0/14" "$GKE_POD_CIDR" "must read the actual pod CIDR, not servicesIpv4Cidr"
}

test_discover_pod_cidr_alt_field_missing_refused() {
  fresh_gcloud_state
  GKE_NAME="halfpodcidrcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "halfpodcidrcluster" "$NETWORK"
  # clusterIpv4Cidr present, ipAllocationPolicy.clusterIpv4CidrBlock missing --
  # cross-checking requires both, not just the one that happens to be there.
  seed_pod_cidr_mismatch "halfpodcidrcluster" "10.60.0.0/14" ""
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a present clusterIpv4Cidr with a missing ipAllocationPolicy.clusterIpv4CidrBlock must still refuse, not fall back to the one present field"
  assert_contains "$RUN_OUTPUT" "Could not determine the pod CIDR" "error should explain why"
}

test_discover_pod_cidr_missing_refused() {
  fresh_gcloud_state
  GKE_NAME="nopodcidrcluster"; GKE_PROJECT="$PROJECT"; GKE_LOCATION="us-central1"
  seed_cluster "nopodcidrcluster" "$NETWORK"
  seed_pod_cidr_missing "nopodcidrcluster"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a missing pod CIDR must refuse before any create"
  assert_contains "$RUN_OUTPUT" "Could not determine the pod CIDR" "error should explain why"
}

test_discover_pod_cidr_refuses_broader_than_slash_8() {
  fresh_gcloud_state
  GKE_NAME="widepodcidrcluster"; GKE_PROJECT="$PROJECT"
  # shellcheck disable=SC2034 # read by hybrid_discover (hybrid-tier.sh)
  GKE_LOCATION="us-central1"
  seed_cluster "widepodcidrcluster" "$NETWORK"
  seed_pod_cidr "widepodcidrcluster" "10.0.0.0/7"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a pod CIDR broader than /8 must be refused"
  assert_contains "$RUN_OUTPUT" "invalid or dangerously broad" "error should explain why"
}

test_discover_pod_cidr_refuses_ipv6() {
  fresh_gcloud_state
  GKE_NAME="ipv6podcidrcluster"; GKE_PROJECT="$PROJECT"
  # shellcheck disable=SC2034 # read by hybrid_discover (hybrid-tier.sh)
  GKE_LOCATION="us-central1"
  seed_cluster "ipv6podcidrcluster" "$NETWORK"
  # A dual-stack or IPv6-only cluster's pod CIDR: everything downstream
  # of this (firewall ranges, address reservations) is IPv4-only, so an
  # IPv6 pod CIDR must be refused, not silently accepted.
  seed_pod_cidr "ipv6podcidrcluster" "fd00:abcd::/64"
  run_expect_fail hybrid_discover "$NETWORK"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an IPv6 pod CIDR must be refused"
  assert_contains "$RUN_OUTPUT" "invalid or dangerously broad" "error should explain why"
}

# =====================================================================
# Static internal IP reservation.
# =====================================================================

test_internal_ip_new_vm_reserves_fresh_when_absent() {
  fresh_gcloud_state
  hybrid_ensure_internal_ip_new_vm "$HUB" "$PROJECT" "us-central1" "default"
  assert_eq "10.128.0.9" "$HYBRID_INTERNAL_IP" "must read back the reserved address"
  assert_contains "$(gcloud_log)" "compute addresses create scion-hub-${HUB}-internal-ip" \
    "must reserve a fresh address when absent"
}

# name=(NAME) is an equality match on the address name; region:REGION is
# a word match on the address's region URL, which excludes global
# addresses (no region) and same-named addresses in other regions.
test_internal_ip_get_list_filter_form() {
  fresh_gcloud_state
  _hybrid_internal_ip_get "scion-hub-${HUB}-internal-ip" "$PROJECT" "us-central1" || true
  assert_eq "absent" "$HYBRID_INTERNAL_IP_STATUS" "no reservation is seeded"
  assert_eq "compute addresses list --project=${PROJECT} --filter=name=(scion-hub-${HUB}-internal-ip) region:us-central1 --format=json" \
    "$(gcloud_log | grep '^compute addresses list' || true)" \
    "the reservation lookup must list by exact name and region, in exactly this filter form"
}

test_internal_ip_new_vm_reserve_create_failure_is_explicit_error() {
  fresh_gcloud_state
  set_address_create_will_fail "scion-hub-${HUB}-internal-ip"
  run_expect_fail hybrid_ensure_internal_ip_new_vm "$HUB" "$PROJECT" "us-central1" "default"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a new-VM addresses-create failure must fail the run explicitly, not be silently treated as success"
  assert_contains "$RUN_OUTPUT" "Could not reserve a new internal IP address" "error should explain why"
}

test_internal_ip_new_vm_ignores_same_name_reservation_in_another_region() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.99.0.5" "scion-deployment=${HUB}" "INTERNAL" "default" "us-west1"
  hybrid_ensure_internal_ip_new_vm "$HUB" "$PROJECT" "us-central1" "default"
  assert_eq "10.128.0.9" "$HYBRID_INTERNAL_IP" \
    "a same-named reservation left behind in another region must not be mistaken for this one -- must reserve fresh in the target region"
  assert_contains "$(gcloud_log)" "compute addresses create scion-hub-${HUB}-internal-ip" \
    "must still reserve a fresh address in the target region, not adopt the other region's reservation"
}

# The cross-region tests below use a project ID that contains the target
# region as a word, so region:us-central1 (a word match on the region
# URL) also returns a same-named address in europe-west1.
DECOY_PROJECT="acme-us-central1-prod"

_address_list_count() {
  gcloud compute addresses list --project="$DECOY_PROJECT" \
    --filter="name=($1) region:$2" --format=json \
    | "$PYTHON" -c 'import json, sys; print(len(json.load(sys.stdin)))'
}

# Pins the stub to gcloud's ':' word-match semantics on the region URL.
test_stub_addresses_list_region_word_match() {
  fresh_gcloud_state
  local name="scion-hub-${HUB}-internal-ip"
  seed_address_extra "$name" "10.132.0.7" "$MARKER" "europe-west1" "$DECOY_PROJECT"
  seed_address_extra "$name" "10.138.0.7" "$MARKER" "us-west1" "demo-project"
  assert_eq "1" "$(_address_list_count "$name" "us-central1")" \
    "an entry whose region URL contains us-central1 as a word (in the project ID) is returned for region:us-central1"
  assert_eq "1" "$(_address_list_count "$name" "us-west1")" \
    "an entry in us-west1 is returned for region:us-west1"
  assert_eq "0" "$(_address_list_count "$name" "us-east1")" \
    "no entry has us-east1 in its region URL"
}

test_internal_ip_get_other_region_entry_matching_region_word_is_absent() {
  fresh_gcloud_state
  local name="scion-hub-${HUB}-internal-ip"
  seed_address_extra "$name" "10.132.0.7" "$MARKER" "europe-west1" "$DECOY_PROJECT"
  assert_eq "1" "$(_address_list_count "$name" "us-central1")" \
    "the list returns the europe-west1 entry for region:us-central1"
  _hybrid_internal_ip_get "$name" "$DECOY_PROJECT" "us-central1" || true
  assert_eq "absent" "$HYBRID_INTERNAL_IP_STATUS" \
    "an entry whose region URL does not end in /regions/us-central1 must not count as found"
  assert_eq "" "$HYBRID_INTERNAL_IP_ADDR" "no field is read from an entry that is not kept"
}

test_internal_ip_new_vm_does_not_adopt_other_region_entry_matching_region_word() {
  fresh_gcloud_state
  seed_address_extra "scion-hub-${HUB}-internal-ip" "10.132.0.7" "$MARKER" "europe-west1" "$DECOY_PROJECT"
  hybrid_ensure_internal_ip_new_vm "$HUB" "$DECOY_PROJECT" "us-central1" "default"
  assert_eq "10.128.0.9" "$HYBRID_INTERNAL_IP" \
    "the europe-west1 entry must not be reused; a fresh address is reserved in us-central1"
  assert_contains "$(gcloud_log)" "compute addresses create scion-hub-${HUB}-internal-ip" \
    "must reserve a fresh address in the target region"
}

test_internal_ip_teardown_check_ignores_other_region_entry_matching_region_word() {
  fresh_gcloud_state
  seed_address_extra "scion-hub-${HUB}-internal-ip" "10.132.0.7" "$MARKER" "europe-west1" "$DECOY_PROJECT"
  hybrid_internal_ip_teardown_check "$HUB" "$DECOY_PROJECT" "us-central1"
  assert_eq "false" "$HYBRID_INTERNAL_IP_TEARDOWN_READY" \
    "the europe-west1 entry must not be queued for delete"
  assert_eq "false" "$HYBRID_INTERNAL_IP_TEARDOWN_FAILED" \
    "with no entry in us-central1 the reservation is absent, which does not fail the preflight"
}

test_internal_ip_get_finds_target_region_entry_next_to_other_region_entry() {
  fresh_gcloud_state
  local name="scion-hub-${HUB}-internal-ip"
  seed_address "$name" "10.128.0.42" "$MARKER" "INTERNAL" "default" "us-central1" "$DECOY_PROJECT"
  # The europe-west1 entry is listed first and differs from the
  # us-central1 entry in every field that is read.
  seed_address_extra "$name" "10.132.0.7" "some other unrelated reservation" "europe-west1" "$DECOY_PROJECT" \
    "EXTERNAL" "decoy-subnet"
  assert_eq "2" "$(_address_list_count "$name" "us-central1")" \
    "the list returns both entries for region:us-central1"
  _hybrid_internal_ip_get "$name" "$DECOY_PROJECT" "us-central1"
  assert_eq "found" "$HYBRID_INTERNAL_IP_STATUS" "the us-central1 entry is found"
  assert_eq "10.128.0.42" "$HYBRID_INTERNAL_IP_ADDR" \
    "the address is read from the us-central1 entry, not from the first listed entry"
  assert_eq "$MARKER" "$HYBRID_INTERNAL_IP_DESC" "the description is read from the us-central1 entry"
  assert_eq "INTERNAL" "$HYBRID_INTERNAL_IP_ADDRESS_TYPE" \
    "the address type is read from the us-central1 entry, not from the first listed entry"
  assert_eq "default" "$HYBRID_INTERNAL_IP_SUBNET" \
    "the subnet is read from the us-central1 entry, not from the first listed entry"
  hybrid_ensure_internal_ip_new_vm "$HUB" "$DECOY_PROJECT" "us-central1" "default"
  assert_eq "10.128.0.42" "$HYBRID_INTERNAL_IP" "the marked us-central1 entry is reused"
  assert_not_contains "$(gcloud_log)" "compute addresses create" "no fresh address is reserved"
}

test_internal_ip_get_two_entries_in_target_region_fails_closed() {
  fresh_gcloud_state
  local name="scion-hub-${HUB}-internal-ip"
  seed_address "$name" "10.128.0.42" "$MARKER" "INTERNAL" "default" "us-central1" "$DECOY_PROJECT"
  seed_address_extra "$name" "10.128.0.43" "$MARKER" "us-central1" "other-project"
  _hybrid_internal_ip_get "$name" "$DECOY_PROJECT" "us-central1" || true
  assert_eq "unknown" "$HYBRID_INTERNAL_IP_STATUS" "two entries in the target region are ambiguous"
  assert_contains "$HYBRID_INTERNAL_IP_ERR" "2 addresses named ${name} are in region us-central1" \
    "the error names the count, the name and the region"
  run_expect_fail hybrid_ensure_internal_ip_new_vm "$HUB" "$DECOY_PROJECT" "us-central1" "default"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "an ambiguous lookup must fail the run"
  assert_contains "$RUN_OUTPUT" "Could not check internal IP reservation ${name}: 2 addresses named" \
    "the run fails with the ambiguity error"
  assert_not_contains "$(gcloud_log)" "compute addresses create" "nothing is reserved on an ambiguous lookup"
  hybrid_internal_ip_teardown_check "$HUB" "$DECOY_PROJECT" "us-central1"
  assert_eq "false" "$HYBRID_INTERNAL_IP_TEARDOWN_READY" "an ambiguous lookup is never queued for delete"
  assert_eq "true" "$HYBRID_INTERNAL_IP_TEARDOWN_FAILED" "an ambiguous lookup aborts the teardown"
}

# A list call that succeeds but prints something other than JSON says
# nothing about whether the reservation exists: it is unknown, never
# absent, so nothing is reserved and teardown does not proceed.
test_internal_ip_get_invalid_json_list_is_unknown_not_absent() {
  fresh_gcloud_state
  local name="scion-hub-${HUB}-internal-ip"
  set_address_list_invalid_json "$name"
  _hybrid_internal_ip_get "$name" "$PROJECT" "us-central1" || true
  assert_eq "unknown" "$HYBRID_INTERNAL_IP_STATUS" "a list result that is not JSON is unknown, not absent"
  assert_contains "$HYBRID_INTERNAL_IP_ERR" "could not parse the address list for ${name}" \
    "the error says the list could not be parsed"
  run_expect_fail hybrid_ensure_internal_ip_new_vm "$HUB" "$PROJECT" "us-central1" "default"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a list result that is not JSON must fail the run"
  assert_contains "$RUN_OUTPUT" "Could not check internal IP reservation ${name}" \
    "the run fails on the check itself"
  assert_not_contains "$(gcloud_log)" "compute addresses create" \
    "nothing is reserved when the list result is not JSON"
  hybrid_internal_ip_teardown_check "$HUB" "$PROJECT" "us-central1"
  assert_eq "false" "$HYBRID_INTERNAL_IP_TEARDOWN_READY" "a list result that is not JSON is never queued for delete"
  assert_eq "true" "$HYBRID_INTERNAL_IP_TEARDOWN_FAILED" "a list result that is not JSON aborts the teardown"
}

# _count_step_python_wrapper MODE REAL_PYTHON — prints the path of a
# PYTHON wrapper that runs REAL_PYTHON for every call except the address-count step
# (`print(len(...))`). MODE "fail" makes that step exit 1 with no
# output; MODE "garbage" makes it exit 0 and print a non-numeric line.
_count_step_python_wrapper() {
  local mode="$1" real_python="$2" wrapper
  wrapper="$(mktemp)"
  cat > "$wrapper" <<WRAPPER
#!/usr/bin/env bash
if [[ "\$1" == "-c" && "\$2" == *"print(len("* ]]; then
  cat >/dev/null
  if [[ "${mode}" == "fail" ]]; then
    exit 1
  fi
  echo "not-a-number"
  exit 0
fi
exec "${real_python}" "\$@"
WRAPPER
  chmod +x "$wrapper"
  echo "$wrapper"
}

test_internal_ip_get_count_step_failure_is_unknown_with_clear_error() {
  local mode name saved_python wrapper
  name="scion-hub-${HUB}-internal-ip"
  for mode in fail garbage; do
    fresh_gcloud_state
    seed_address "$name" "10.128.0.42" "$MARKER"
    saved_python="$PYTHON"
    wrapper="$(_count_step_python_wrapper "$mode" "$saved_python")"
    PYTHON="$wrapper"
    _hybrid_internal_ip_get "$name" "$PROJECT" "us-central1" || true
    assert_eq "unknown" "$HYBRID_INTERNAL_IP_STATUS" \
      "count step ${mode}: the reservation state is unknown"
    assert_eq "could not count the addresses named ${name} in region us-central1" "$HYBRID_INTERNAL_IP_ERR" \
      "count step ${mode}: the error names the address and region and prints no count"
    assert_eq "" "$HYBRID_INTERNAL_IP_ADDR" "count step ${mode}: no address is read"
    run_expect_fail hybrid_ensure_internal_ip_new_vm "$HUB" "$PROJECT" "us-central1" "default"
    assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
      "count step ${mode}: the run fails"
    assert_not_contains "$(gcloud_log)" "compute addresses create" \
      "count step ${mode}: nothing is reserved"
    hybrid_internal_ip_teardown_check "$HUB" "$PROJECT" "us-central1"
    assert_eq "false" "$HYBRID_INTERNAL_IP_TEARDOWN_READY" "count step ${mode}: never queued for delete"
    assert_eq "true" "$HYBRID_INTERNAL_IP_TEARDOWN_FAILED" "count step ${mode}: the teardown is aborted"
    PYTHON="$saved_python"
    rm -f "$wrapper"
  done
}

test_internal_ip_new_vm_reuses_existing_marked_reservation() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.42" "$MARKER"
  hybrid_ensure_internal_ip_new_vm "$HUB" "$PROJECT" "us-central1" "default"
  assert_eq "10.128.0.42" "$HYBRID_INTERNAL_IP" "a marked existing reservation must be reused as-is"
  assert_eq "0" "$(gcloud_log | grep -c 'addresses create' || true)" \
    "an already-marked, existing reservation must not be recreated"
}

test_internal_ip_new_vm_refuses_unmarked_reservation() {
  fresh_gcloud_state
  seed_address_unmarked "scion-hub-${HUB}-internal-ip" "10.128.0.42"
  run_expect_fail hybrid_ensure_internal_ip_new_vm "$HUB" "$PROJECT" "us-central1" "default"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unmarked same-name reservation must refuse the run"
  assert_contains "$RUN_OUTPUT" "without this deployment's marker" "error should explain why"
}

test_internal_ip_new_vm_list_error_fails_closed() {
  fresh_gcloud_state
  set_address_list_will_fail "scion-hub-${HUB}-internal-ip"
  run_expect_fail hybrid_ensure_internal_ip_new_vm "$HUB" "$PROJECT" "us-central1" "default"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unknown list result must fail closed, not be treated as absent"
  assert_contains "$RUN_OUTPUT" "Could not check internal IP reservation" "must fail on the check itself, not fall through to a later step"
  assert_not_contains "$(gcloud_log)" "addresses create scion-hub-${HUB}-internal-ip" "must never attempt to reserve a new address when the check itself is unknown"
}

test_internal_ip_existing_vm_list_error_fails_closed() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME_TEST" "us-central1-b"
  set_address_list_will_fail "scion-hub-${HUB}-internal-ip"
  run_expect_fail hybrid_ensure_internal_ip_existing_vm "$HUB" "$PROJECT" "us-central1" "default" \
    "10.128.0.5" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unknown list result must fail closed, not be treated as absent"
  assert_contains "$RUN_OUTPUT" "Could not check internal IP reservation" "must fail on the check itself, not fall through to promoting the VM's current IP"
  assert_not_contains "$(gcloud_log)" "addresses create scion-hub-${HUB}-internal-ip" "must never attempt to promote the VM's current IP when the check itself is unknown"
}

test_internal_ip_existing_vm_promotes_current_ip_when_absent() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME_TEST" "us-central1-b"
  hybrid_ensure_internal_ip_existing_vm "$HUB" "$PROJECT" "us-central1" "default" \
    "10.128.0.5" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_eq "10.128.0.5" "$HYBRID_INTERNAL_IP" "must promote the VM's current IP"
  assert_contains "$(gcloud_log)" "addresses create scion-hub-${HUB}-internal-ip" "must promote via addresses create"
  assert_contains "$(gcloud_log)" "--addresses=10.128.0.5" "must promote the exact current IP, not a fresh one"
  # Without the marker, this same reservation would look unmarked on the
  # very next deploy and get permanently refused as "already exists
  # without this deployment's marker".
  assert_contains "$(gcloud_log)" "--description=${MARKER}" "must mark the promoted reservation, or every later deploy will refuse to adopt it"
}

test_internal_ip_existing_vm_promote_create_failure_is_explicit_error() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME_TEST" "us-central1-b"
  set_address_create_will_fail "scion-hub-${HUB}-internal-ip"
  run_expect_fail hybrid_ensure_internal_ip_existing_vm "$HUB" "$PROJECT" "us-central1" "default" \
    "10.128.0.5" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a promote addresses-create failure must fail the run explicitly, not be silently treated as success"
  assert_contains "$RUN_OUTPUT" "Could not promote the VM's current internal IP" "error should explain why"
}

test_internal_ip_existing_vm_reuses_matching_marked_reservation() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.5" "$MARKER"
  hybrid_ensure_internal_ip_existing_vm "$HUB" "$PROJECT" "us-central1" "default" \
    "10.128.0.5" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_eq "10.128.0.5" "$HYBRID_INTERNAL_IP" "a matching marked reservation must be reused"
  assert_eq "0" "$(gcloud_log | grep -c 'addresses create' || true)" "must not be promoted/recreated when already matching"
}

test_internal_ip_existing_vm_drift_fails_with_remediation() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.99" "$MARKER"
  run_expect_fail hybrid_ensure_internal_ip_existing_vm "$HUB" "$PROJECT" "us-central1" "default" \
    "10.128.0.5" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a mismatched marked reservation must fail the run"
  assert_contains "$RUN_OUTPUT" "no longer matches the VM's current internal IP" "error should explain the drift"
  assert_contains "$RUN_OUTPUT" "addresses delete" "error should include remediation"
}

test_internal_ip_existing_vm_refuses_unmarked_reservation() {
  fresh_gcloud_state
  seed_address_unmarked "scion-hub-${HUB}-internal-ip" "10.128.0.5"
  run_expect_fail hybrid_ensure_internal_ip_existing_vm "$HUB" "$PROJECT" "us-central1" "default" \
    "10.128.0.5" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an unmarked same-name reservation must refuse the run"
  assert_contains "$RUN_OUTPUT" "without this deployment's marker" "error should explain why"
}

test_internal_ip_new_vm_refuses_reused_reservation_with_wrong_address_type() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.42" "$MARKER" "EXTERNAL" "default"
  run_expect_fail hybrid_ensure_internal_ip_new_vm "$HUB" "$PROJECT" "us-central1" "default"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a marked reservation whose address type isn't INTERNAL must refuse the run"
  assert_contains "$RUN_OUTPUT" "not INTERNAL" "error should explain why"
}

test_internal_ip_new_vm_refuses_reused_reservation_with_wrong_subnet() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.42" "$MARKER" "INTERNAL" "some-other-subnet"
  run_expect_fail hybrid_ensure_internal_ip_new_vm "$HUB" "$PROJECT" "us-central1" "default"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a marked reservation on the wrong subnet must refuse the run"
  assert_contains "$RUN_OUTPUT" "not the expected 'default'" "error should explain why"
}

test_internal_ip_new_vm_create_then_get_fails_is_explicit_error() {
  fresh_gcloud_state
  set_address_list_will_fail_after_create "scion-hub-${HUB}-internal-ip"
  run_expect_fail hybrid_ensure_internal_ip_new_vm "$HUB" "$PROJECT" "us-central1" "default"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a list failure right after a successful create must be a clear, explicit failure, not a silent exit"
  assert_contains "$RUN_OUTPUT" "could not read it back" "error should explain what happened"
}

test_internal_ip_existing_vm_refuses_reused_reservation_with_wrong_address_type() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.5" "$MARKER" "EXTERNAL" "default"
  run_expect_fail hybrid_ensure_internal_ip_existing_vm "$HUB" "$PROJECT" "us-central1" "default" \
    "10.128.0.5" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a marked reservation whose address type isn't INTERNAL must refuse the run"
  assert_contains "$RUN_OUTPUT" "not INTERNAL" "error should explain why"
}

test_internal_ip_existing_vm_refuses_reused_reservation_with_wrong_subnet() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.5" "$MARKER" "INTERNAL" "some-other-subnet"
  run_expect_fail hybrid_ensure_internal_ip_existing_vm "$HUB" "$PROJECT" "us-central1" "default" \
    "10.128.0.5" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a marked reservation on the wrong subnet must refuse the run"
  assert_contains "$RUN_OUTPUT" "not the expected 'default'" "error should explain why"
}

test_internal_ip_existing_vm_promote_recheck_ip_changed_fails() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME_TEST" "us-central1-b"
  # The stub's instances-describe recheck answers 10.128.0.5 here
  # (the default); passing a different current_ip here simulates the
  # VM's own IP changing out from under the promotion, between the
  # caller's earlier read and this function's own recheck.
  run_expect_fail hybrid_ensure_internal_ip_existing_vm "$HUB" "$PROJECT" "us-central1" "default" \
    "10.128.0.77" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a VM IP that changed between the read and the promotion recheck must fail, not silently promote the stale value"
  assert_contains "$RUN_OUTPUT" "changed from 10.128.0.77 to 10.128.0.5" "error should show both values"
}

test_internal_ip_existing_vm_refuses_non_ipv4_current_ip_before_promoting() {
  fresh_gcloud_state
  run_expect_fail hybrid_ensure_internal_ip_existing_vm "$HUB" "$PROJECT" "us-central1" "default" \
    "" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a blank or malformed current IP must be refused before it's ever passed to addresses create --addresses="
  assert_contains "$RUN_OUTPUT" "doesn't look like an IPv4 address" "error should explain why"
  assert_eq "0" "$(gcloud_log | grep -c 'addresses create' || true)" \
    "must never attempt the create call with a non-IPv4 value"
}

test_internal_ip_teardown_check_marked_ready_for_delete() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.5" "$MARKER"
  hybrid_internal_ip_teardown_check "$HUB" "$PROJECT" "us-central1"
  assert_eq "true" "$HYBRID_INTERNAL_IP_TEARDOWN_READY" "a marked reservation must be ready for delete"
  assert_eq "false" "$HYBRID_INTERNAL_IP_TEARDOWN_FAILED" "a marked reservation must not fail the preflight"
}

test_internal_ip_teardown_check_unmarked_aborts() {
  fresh_gcloud_state
  seed_address_unmarked "scion-hub-${HUB}-internal-ip" "10.128.0.5"
  hybrid_internal_ip_teardown_check "$HUB" "$PROJECT" "us-central1"
  assert_eq "false" "$HYBRID_INTERNAL_IP_TEARDOWN_READY" "an unmarked reservation must not be queued for delete"
  assert_eq "true" "$HYBRID_INTERNAL_IP_TEARDOWN_FAILED" "an unmarked reservation must abort the whole teardown"
}

test_internal_ip_teardown_check_absent_is_inert() {
  fresh_gcloud_state
  hybrid_internal_ip_teardown_check "$HUB" "$PROJECT" "us-central1"
  assert_eq "false" "$HYBRID_INTERNAL_IP_TEARDOWN_READY" "nothing to delete when the reservation was never created"
  assert_eq "false" "$HYBRID_INTERNAL_IP_TEARDOWN_FAILED" "an absent reservation must not fail the preflight"
}

test_internal_ip_teardown_check_list_failure_is_failure_not_absent() {
  fresh_gcloud_state
  set_address_list_will_fail "scion-hub-${HUB}-internal-ip"
  hybrid_internal_ip_teardown_check "$HUB" "$PROJECT" "us-central1"
  assert_eq "false" "$HYBRID_INTERNAL_IP_TEARDOWN_READY" \
    "an unconfirmable list result must never be queued for delete, the same as a genuinely absent one, but for a different reason"
  assert_eq "true" "$HYBRID_INTERNAL_IP_TEARDOWN_FAILED" \
    "a failed list call must abort the whole teardown -- unknown must never be read as absent"
}

test_internal_ip_teardown_delete_when_vm_gone() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.5" "$MARKER"
  hybrid_internal_ip_teardown_check "$HUB" "$PROJECT" "us-central1"
  hybrid_internal_ip_teardown_delete "$PROJECT" "us-central1" "true"
  assert_eq "true" "$HYBRID_INTERNAL_IP_DELETED" "must delete once the VM is confirmed gone"
  assert_eq "false" "$HYBRID_INTERNAL_IP_DELETE_FAILED" "a successful delete must not be reported as failed"
}

test_internal_ip_teardown_delete_skipped_when_vm_not_gone() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.5" "$MARKER"
  hybrid_internal_ip_teardown_check "$HUB" "$PROJECT" "us-central1"
  hybrid_internal_ip_teardown_delete "$PROJECT" "us-central1" "false"
  assert_eq "false" "$HYBRID_INTERNAL_IP_DELETED" "must not delete while the VM's deletion isn't confirmed"
  assert_eq "true" "$HYBRID_INTERNAL_IP_DELETE_FAILED" "must be reported as kept/failed, not silently skipped"
  assert_eq "0" "$(gcloud_log | grep -c 'addresses delete' || true)" "no delete call may even be attempted"
}

test_internal_ip_teardown_delete_noop_when_nothing_queued() {
  fresh_gcloud_state
  hybrid_internal_ip_teardown_check "$HUB" "$PROJECT" "us-central1"
  hybrid_internal_ip_teardown_delete "$PROJECT" "us-central1" "true"
  assert_eq "false" "$HYBRID_INTERNAL_IP_DELETED" "nothing was queued, so nothing should be reported deleted"
  assert_eq "false" "$HYBRID_INTERNAL_IP_DELETE_FAILED" "nothing queued must not be reported as failed either"
}

# =====================================================================
# Internal IP guard: the post-create verification half. Confirms only
# the reservation the NFS PV needs; it has nothing to say about
# hub-deny or any other firewall rule -- those are checked as part of
# the firewall rules themselves (see the drift battery above).
# =====================================================================

test_internal_ip_guard_verify_passes_when_everything_is_in_place() {
  fresh_gcloud_state
  HYBRID_INTERNAL_IP="10.128.0.5"
  seed_instance "$INSTANCE_NAME_TEST" "us-central1-b"
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.5" "$MARKER"
  assert_true "$(hybrid_internal_ip_guard_verify "$HUB" "$PROJECT" "us-central1" "$INSTANCE_NAME_TEST" "us-central1-b" && echo true || echo false)" \
    "the guard must pass when the reservation is marked and matches the VM's actual internal IP"
}

test_internal_ip_guard_verify_fails_when_reservation_unmarked() {
  fresh_gcloud_state
  HYBRID_INTERNAL_IP="10.128.0.5"
  seed_instance "$INSTANCE_NAME_TEST" "us-central1-b"
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.5" "some-other-marker"
  run_expect_fail hybrid_internal_ip_guard_verify "$HUB" "$PROJECT" "us-central1" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a reservation that no longer carries this deployment's marker must fail the guard"
  assert_contains "$RUN_OUTPUT" "no longer carries this deployment's marker" "error should explain why"
}

test_internal_ip_guard_verify_fails_when_reservation_address_mismatches_vm_ip() {
  fresh_gcloud_state
  HYBRID_INTERNAL_IP="10.128.0.5"
  seed_instance "$INSTANCE_NAME_TEST" "us-central1-b"
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.99" "$MARKER"
  run_expect_fail hybrid_internal_ip_guard_verify "$HUB" "$PROJECT" "us-central1" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a reservation whose address doesn't match the VM's resolved internal IP must fail the guard"
  assert_contains "$RUN_OUTPUT" "wrong address" "error should explain why"
}

test_internal_ip_guard_verify_fails_when_vm_actual_ip_diverges_even_if_hybrid_internal_ip_still_matches() {
  fresh_gcloud_state
  HYBRID_INTERNAL_IP="10.128.0.5"
  export GCLOUD_STUB_VM_IP="10.128.0.77"
  seed_instance "$INSTANCE_NAME_TEST" "us-central1-b"
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.5" "$MARKER"
  run_expect_fail hybrid_internal_ip_guard_verify "$HUB" "$PROJECT" "us-central1" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "the guard must catch the VM's real internal IP having diverged from the reservation, even when \$HYBRID_INTERNAL_IP (read earlier, from the reservation itself on the new-VM path) still equals it -- comparing against \$HYBRID_INTERNAL_IP alone could never catch this"
  assert_contains "$RUN_OUTPUT" "wrong address" "error should explain why"
  unset GCLOUD_STUB_VM_IP
}

test_internal_ip_guard_verify_fails_when_resolved_ip_does_not_match_vm_ip() {
  fresh_gcloud_state
  # HYBRID_INTERNAL_IP disagrees with the VM's actual (default-stub)
  # internal IP directly -- defense in depth on top of the later
  # reservation-address check, which this scenario never even reaches.
  HYBRID_INTERNAL_IP="10.128.0.99"
  seed_instance "$INSTANCE_NAME_TEST" "us-central1-b"
  run_expect_fail hybrid_internal_ip_guard_verify "$HUB" "$PROJECT" "us-central1" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a resolved internal IP that doesn't match the VM's actual internal IP must fail the guard"
  assert_contains "$RUN_OUTPUT" "does not match VM" "error should explain why"
  assert_not_contains "$(gcloud_log)" "addresses describe" \
    "must fail before ever describing the reservation"
}

test_internal_ip_guard_verify_fails_when_reservation_address_is_not_valid_ipv4() {
  fresh_gcloud_state
  HYBRID_INTERNAL_IP="10.128.0.5"
  seed_instance "$INSTANCE_NAME_TEST" "us-central1-b"
  seed_address "scion-hub-${HUB}-internal-ip" "not-an-ip" "$MARKER"
  run_expect_fail hybrid_internal_ip_guard_verify "$HUB" "$PROJECT" "us-central1" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a reservation address that isn't valid IPv4 must fail the guard"
  assert_contains "$RUN_OUTPUT" "not a valid IPv4 address" "error should explain why"
}

test_internal_ip_guard_verify_fails_on_empty_internal_ip() {
  fresh_gcloud_state
  HYBRID_INTERNAL_IP=""
  run_expect_fail hybrid_internal_ip_guard_verify "$HUB" "$PROJECT" "us-central1" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "an empty internal IP must fail the guard"
  assert_contains "$RUN_OUTPUT" "no valid internal IP is resolved" "error should explain why"
}

test_internal_ip_guard_verify_fails_on_non_ipv4_internal_ip() {
  fresh_gcloud_state
  # Non-empty but not IPv4-shaped -- a blank-then-fallback bug or a stray
  # hostname/IPv6 value must not slip past the emptiness check alone.
  HYBRID_INTERNAL_IP="not-an-ip"
  run_expect_fail hybrid_internal_ip_guard_verify "$HUB" "$PROJECT" "us-central1" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a non-IPv4-shaped internal IP must fail the guard"
  assert_contains "$RUN_OUTPUT" "no valid internal IP is resolved" "error should explain why"
}

test_internal_ip_guard_verify_fails_when_reservation_missing() {
  fresh_gcloud_state
  HYBRID_INTERNAL_IP="10.128.0.5"
  seed_instance "$INSTANCE_NAME_TEST" "us-central1-b"
  run_expect_fail hybrid_internal_ip_guard_verify "$HUB" "$PROJECT" "us-central1" "$INSTANCE_NAME_TEST" "us-central1-b"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "a missing reservation must fail the guard"
  assert_contains "$RUN_OUTPUT" "could not be confirmed after create" "error should name the reservation"
  # Must stop right there, not fall through into the marker check with an
  # empty/garbage addr_json -- that would print a second, misleading
  # diagnostic ("no longer carries the marker") for what is actually a
  # totally different failure (the describe call itself failing).
  assert_not_contains "$RUN_OUTPUT" "no longer carries this deployment's marker" "must not fall through past the describe failure into the marker check"
}

# =====================================================================
# No pod-to-hub allow rule, or guard function for one, exists in the
# scripts or docs.
# =====================================================================

test_no_hub_allow_artifact_remains_in_scripts_or_docs() {
  # Scans every text file under scripts/single-node-vm (including the
  # extensionless stubs in tests/lib), docs/ and .design/project-log/,
  # for any spelling of the name (hub-allow, hub_allow, "hub allow",
  # hubAllow, in any case) and the guard function's name. The one exclusion is
  # this file, where this test's own name and patterns spell the string
  # out.
  local hits repo_root
  repo_root="$(cd "${TIER_DIR}/../.." && pwd)"
  hits="$(grep -rIniE 'hub[-_ ]?allow|hybrid_hub_url_guard' \
    "${TIER_DIR}" "${repo_root}/docs" "${repo_root}/.design/project-log" \
    --exclude='test_hybrid_tier.sh' \
    2>/dev/null || true)"
  assert_eq "" "$hits" "no hub-allow naming, variable, or function-name artifact should remain under scripts/single-node-vm, docs or .design/project-log"
}

# =====================================================================
# Agent transport auth: IAP OAuth client ID discovery, the dedicated
# transport service account, and the grants that let it mint and use ID
# tokens.
# =====================================================================

test_discover_iap_client_id_lands_verbatim() {
  fresh_gcloud_state
  set_iap_client_id "999999999-realistic-client-id.apps.googleusercontent.com"
  hybrid_discover_iap_client_id "$PROJECT"
  assert_eq "999999999-realistic-client-id.apps.googleusercontent.com" "$HYBRID_IAP_CLIENT_ID" \
    "the discovered client id must land verbatim, unmodified"
}

test_discover_iap_client_id_refused_when_empty() {
  fresh_gcloud_state
  set_iap_client_id_empty
  run_expect_fail hybrid_discover_iap_client_id "$PROJECT"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "an IAP project with no OAuth client id configured must fail closed"
  assert_contains "$RUN_OUTPUT" "no OAuth client ID configured" "error should explain why"
}

test_discover_iap_client_id_refused_on_api_error() {
  fresh_gcloud_state
  set_iap_settings_get_will_fail
  run_expect_fail hybrid_discover_iap_client_id "$PROJECT"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "an IAP settings read failure must fail closed, not be treated as no client id"
  assert_contains "$RUN_OUTPUT" "Could not read project" "error should explain the read failure"
}

test_discover_iap_client_id_refused_when_malformed() {
  local bad
  for bad in 'x" injected: "y' '123-abc.apps.googleusercontent.com.evil.example' 'abc-def.apps.googleusercontent.com' \
      '123-abc def.apps.googleusercontent.com' "$(printf '123-abc.apps.googleusercontent.com\nmode: open')"; do
    fresh_gcloud_state
    set_iap_client_id "$bad"
    run_expect_fail hybrid_discover_iap_client_id "$PROJECT"
    assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
      "a client id not in the OAuth client id form must be refused (got '${bad}')"
    assert_contains "$RUN_OUTPUT" "is not in the expected" "the error should say the client id has the wrong form"
  done
}

test_transport_sa_name_shape_and_length() {
  local name hub
  for hub in x demohub team-alpha-prod a-very-long-hub-nm xxxxxxxxxxx-yyyyyyyy; do
    name="$(hybrid_transport_sa_name "$hub")"
    assert_true "$([[ ${#name} -ge 6 && ${#name} -le 30 ]] && echo true || echo false)" \
      "transport SA id for '${hub}' must be 6-30 chars (got '${name}', ${#name})"
    assert_true "$([[ "$name" =~ ^[a-z]([-a-z0-9]*[a-z0-9])$ ]] && echo true || echo false)" \
      "transport SA id for '${hub}' must use the service-account id charset (got '${name}')"
    assert_true "$([[ "$name" == scion-tp-* ]] && echo true || echo false)" \
      "transport SA id for '${hub}' must use the scion-tp- prefix (got '${name}')"
    assert_true "$([[ "$name" != *--* ]] && echo true || echo false)" \
      "transport SA id for '${hub}' must not contain a doubled hyphen (got '${name}')"
  done
  assert_eq "scion-tp-demohub-bcaae3f2" "$(hybrid_transport_sa_name "demohub")" \
    "a short hub name keeps its full name plus the 8-hex hash of the full name"
}

test_transport_sa_name_hash_keeps_leading_zeros() {
  # cksum("hub-6") is 127372129 (0x7978b61), seven hex digits unpadded.
  assert_eq "scion-tp-hub-6-07978b61" "$(hybrid_transport_sa_name "hub-6")" \
    "the hash is always 8 hex digits, zero-padded"
}

test_transport_sa_name_stable_across_runs() {
  local first second
  first="$(hybrid_transport_sa_name "team-alpha-prod")"
  # shellcheck disable=SC2016 # expanded by the child shell, not here
  second="$(bash -c 'source "$1"; hybrid_transport_sa_name "team-alpha-prod"' _ "${TIER_DIR}/hybrid-tier.sh")"
  assert_eq "$first" "$second" "the transport SA id must be the same in a separate process for the same hub name"
}

test_transport_sa_name_distinct_for_shared_long_prefix() {
  local a b c
  a="$(hybrid_transport_sa_name "team-alpha-prod")"
  b="$(hybrid_transport_sa_name "team-alpha-dev")"
  c="$(hybrid_transport_sa_name "team-alpha-p")"
  assert_true "$([[ "$a" != "$b" && "$a" != "$c" && "$b" != "$c" ]] && echo true || echo false)" \
    "hubs sharing a long name prefix must get distinct transport SA ids (got '${a}', '${b}', '${c}')"
}

# deploy.sh names the hub's base SA "scion-hub-<hub>", cut to 30 chars;
# a transport SA id must never equal any hub's base SA id, including a
# hub literally named "<other hub>-transport".
test_transport_sa_name_never_equals_a_base_sa_name() {
  local hub base t
  for hub in x x-transport demohub demohub-transport a-very-long-hub-nm; do
    t="$(hybrid_transport_sa_name "$hub")"
    for base in "scion-hub-x" "scion-hub-x-transport" "scion-hub-demohub" "scion-hub-demohub-transport" "scion-hub-a-very-long-hub-nm"; do
      assert_true "$([[ "$t" != "${base:0:30}" ]] && echo true || echo false)" \
        "transport SA id '${t}' (hub '${hub}') must not equal base SA id '${base:0:30}'"
    done
  done
  assert_true "$([[ "$(hybrid_transport_sa_name x)" != "$(hybrid_transport_sa_name x-transport)" ]] && echo true || echo false)" \
    "hubs x and x-transport must get distinct transport SA ids"
}

test_ensure_transport_sa_creates_when_absent() {
  fresh_gcloud_state
  local sa_id
  sa_id="$(hybrid_transport_sa_name "$HUB")"
  hybrid_ensure_transport_sa "$HUB" "$PROJECT"
  assert_eq "${sa_id}@${PROJECT}.iam.gserviceaccount.com" "$HYBRID_TRANSPORT_SA_EMAIL" \
    "the transport SA email must be derived from the hub name and project"
  assert_contains "$(gcloud_log)" "iam service-accounts create ${sa_id} " \
    "the transport SA must actually be created"
}

test_ensure_transport_sa_reuses_when_marked() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  hybrid_ensure_transport_sa "$HUB" "$PROJECT"
  assert_eq "0" "$(gcloud_log | grep -c 'iam service-accounts create' || true)" \
    "an already-marked, matching transport SA must not be recreated"
}

test_ensure_transport_sa_adopts_marked_without_user_keys() {
  fresh_gcloud_state
  local email keys_line
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  hybrid_ensure_transport_sa "$HUB" "$PROJECT"
  assert_eq "$email" "$HYBRID_TRANSPORT_SA_EMAIL" "a marked SA with no user-managed keys is adopted"
  keys_line="$(gcloud_log | grep '^iam service-accounts keys list' || true)"
  assert_contains "$keys_line" "--iam-account=${email}" "adoption must list the keys of the adopted SA"
  assert_contains "$keys_line" "--managed-by=user" "adoption must list user-managed keys only"
}

test_ensure_transport_sa_refused_when_user_keys_present() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  set_service_account_user_keys "$email" "projects/${PROJECT}/serviceAccounts/${email}/keys/0123abcd"
  run_expect_fail hybrid_ensure_transport_sa "$HUB" "$PROJECT"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a marked SA with user-managed keys must be refused, not adopted"
  assert_contains "$RUN_OUTPUT" "has user-managed keys" "the error should name the keys"
}

test_ensure_transport_sa_refused_on_keys_list_error() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  set_service_account_keys_list_error "$email"
  run_expect_fail hybrid_ensure_transport_sa "$HUB" "$PROJECT"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a keys list failure must refuse adoption, not read as no keys"
  assert_contains "$RUN_OUTPUT" "Could not list the user-managed keys" "the error should say the keys could not be listed"
  assert_contains "$RUN_OUTPUT" "PERMISSION_DENIED" "the list error must be printed"
}

test_ensure_transport_sa_refused_when_unmarked() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "some-other-marker"
  run_expect_fail hybrid_ensure_transport_sa "$HUB" "$PROJECT"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "an unmarked same-name transport SA must be refused, not adopted"
  assert_contains "$RUN_OUTPUT" "without this deployment's marker" "error should explain the refusal"
}

test_ensure_transport_sa_refused_on_describe_error() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  set_service_account_describe_error "$email"
  run_expect_fail hybrid_ensure_transport_sa "$HUB" "$PROJECT"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a describe error other than not-found must fail the run, not be read as absent"
  assert_contains "$RUN_OUTPUT" "Could not confirm whether transport service account" "error should explain the uncertainty"
  assert_eq "0" "$(gcloud_log | grep -c 'iam service-accounts create' || true)" \
    "nothing must be created when the SA's existence is unknown"
}

test_grant_transport_token_creator_uses_openid_token_creator_role_and_correct_member() {
  fresh_gcloud_state
  hybrid_grant_transport_token_creator "transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    "hub-sa@${PROJECT}.iam.gserviceaccount.com" "$PROJECT"
  local line
  line="$(gcloud_log | grep 'iam service-accounts add-iam-policy-binding')"
  assert_contains "$line" "transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    "the grant must be on the transport SA resource itself"
  assert_contains "$line" "--role=roles/iam.serviceAccountOpenIdTokenCreator" \
    "the grant must use the ID-token-only role, not the broader serviceAccountTokenCreator"
  assert_contains "$line" "--member=serviceAccount:hub-sa@${PROJECT}.iam.gserviceaccount.com" \
    "the grant's member must be the hub's own runtime SA"
}

test_grant_transport_token_creator_fails_closed_on_grant_failure() {
  fresh_gcloud_state
  set_service_account_grant_will_fail "transport-sa@${PROJECT}.iam.gserviceaccount.com"
  run_expect_fail hybrid_grant_transport_token_creator "transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    "hub-sa@${PROJECT}.iam.gserviceaccount.com" "$PROJECT"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a grant failure must fail the run, not be silently ignored"
}

test_grant_transport_sa_iap_access_uses_correct_role_and_member() {
  fresh_gcloud_state
  hybrid_grant_transport_sa_iap_access "transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    "$INSTANCE_NAME_TEST-iap-proxy" "us-central1" "$PROJECT"
  local line
  line="$(gcloud_log | grep 'iap web add-iam-policy-binding')"
  assert_contains "$line" "--resource-type=cloud-run" "the grant must target the Cloud Run resource type"
  assert_contains "$line" "--service=${INSTANCE_NAME_TEST}-iap-proxy" "the grant must target the hub's own Cloud Run service"
  assert_contains "$line" "--role=roles/iap.httpsResourceAccessor" "the grant must use the same role the operator's own grant uses"
  assert_contains "$line" "--member=serviceAccount:transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    "the grant's member must be the transport SA"
}

# --- resource-level bindings: --condition=None and gcloud's error text ---
# The stub answers `iam service-accounts add-iam-policy-binding` and `iap
# web add-iam-policy-binding` as for a policy that already contains
# conditional bindings: a call without --condition=None fails.

CONDITION_ERROR_TEXT="Adding a binding without specifying a condition to a policy containing conditions is prohibited in non-interactive mode. Run the command again with \`--condition=None\`"

test_gcloud_stub_resource_bindings_without_condition_fail() {
  fresh_gcloud_state
  local out rc
  out="$(gcloud iam service-accounts add-iam-policy-binding "transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    --project="$PROJECT" --member="serviceAccount:hub-sa@${PROJECT}.iam.gserviceaccount.com" \
    --role=roles/iam.serviceAccountOpenIdTokenCreator --quiet 2>&1 >/dev/null)"
  rc=$?
  assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" \
    "a service account binding without --condition must fail on a policy with conditions"
  assert_contains "$out" "(gcloud.iam.service-accounts.add-iam-policy-binding) ${CONDITION_ERROR_TEXT}" \
    "the service account binding failure must carry gcloud's error text"
  out="$(gcloud iap web add-iam-policy-binding --resource-type=cloud-run --service=svc \
    --region="$REGION" --project="$PROJECT" --member="serviceAccount:transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    --role=roles/iap.httpsResourceAccessor --quiet 2>&1 >/dev/null)"
  rc=$?
  assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" \
    "an IAP web binding without --condition must fail on a policy with conditions"
  assert_contains "$out" "(gcloud.iap.web.add-iam-policy-binding) ${CONDITION_ERROR_TEXT}" \
    "the IAP web binding failure must carry gcloud's error text"
}

test_grant_transport_token_creator_passes_condition_none_and_prints_nothing_extra() {
  fresh_gcloud_state
  local out
  out="$(hybrid_grant_transport_token_creator "transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    "hub-sa@${PROJECT}.iam.gserviceaccount.com" "$PROJECT" 2>&1)"
  assert_contains "$(gcloud_log | grep 'iam service-accounts add-iam-policy-binding')" " --condition=None " \
    "the token creator binding must pass --condition=None"
  assert_eq "  Granted roles/iam.serviceAccountOpenIdTokenCreator on transport-sa@${PROJECT}.iam.gserviceaccount.com to hub-sa@${PROJECT}.iam.gserviceaccount.com" "$out" \
    "a successful binding must print only the success line, not gcloud's stderr"
}

test_grant_transport_token_creator_failure_exits_with_gcloud_error() {
  fresh_gcloud_state
  local msg="ERROR: (gcloud.iam.service-accounts.add-iam-policy-binding) PERMISSION_DENIED: caller lacks iam.serviceAccounts.setIamPolicy"
  set_service_account_grant_will_fail "transport-sa@${PROJECT}.iam.gserviceaccount.com" "$msg"
  run_expect_fail hybrid_grant_transport_token_creator "transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    "hub-sa@${PROJECT}.iam.gserviceaccount.com" "$PROJECT"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a failed token creator binding must exit non-zero"
  assert_contains "$RUN_OUTPUT" "Could not grant hub-sa@${PROJECT}.iam.gserviceaccount.com permission to mint ID tokens for transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    "the error must name both service accounts"
  assert_contains "$RUN_OUTPUT" "  ${msg}" "the error must include gcloud's error text"
  assert_not_contains "$RUN_OUTPUT" "Granted roles/iam.serviceAccountOpenIdTokenCreator" \
    "a failed binding must not be reported as granted"
}

test_grant_transport_sa_iap_access_passes_condition_none_and_prints_nothing_extra() {
  fresh_gcloud_state
  local out
  out="$(hybrid_grant_transport_sa_iap_access "transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    "$INSTANCE_NAME_TEST-iap-proxy" "us-central1" "$PROJECT" 2>&1)"
  assert_contains "$(gcloud_log | grep 'iap web add-iam-policy-binding')" " --condition=None " \
    "the transport SA's IAP binding must pass --condition=None"
  assert_eq "  IAP access granted to: transport-sa@${PROJECT}.iam.gserviceaccount.com (agent transport)" "$out" \
    "a successful binding must print only the success line, not gcloud's stderr"
}

test_grant_transport_sa_iap_access_failure_exits_with_gcloud_error() {
  fresh_gcloud_state
  local msg="ERROR: (gcloud.iap.web.add-iam-policy-binding) PERMISSION_DENIED: caller lacks iap.webServices.setIamPolicy"
  set_iap_web_binding_will_fail "serviceAccount:transport-sa@${PROJECT}.iam.gserviceaccount.com" "$msg"
  run_expect_fail hybrid_grant_transport_sa_iap_access "transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    "$INSTANCE_NAME_TEST-iap-proxy" "us-central1" "$PROJECT"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" \
    "a failed transport SA IAP binding must exit non-zero"
  assert_contains "$RUN_OUTPUT" "Could not grant transport service account transport-sa@${PROJECT}.iam.gserviceaccount.com IAP access to Cloud Run service ${INSTANCE_NAME_TEST}-iap-proxy" \
    "the error must name the transport SA and the service"
  assert_contains "$RUN_OUTPUT" "  ${msg}" "the error must include gcloud's error text"
  assert_not_contains "$RUN_OUTPUT" "IAP access granted to:" "a failed binding must not be reported as granted"
}

test_settings_auth_transport_yaml_renders_expected_fields() {
  local yaml
  yaml="$(hybrid_settings_auth_transport_yaml "999999999-client.apps.googleusercontent.com" "transport-sa@${PROJECT}.iam.gserviceaccount.com")"
  assert_contains "$yaml" "mode: iap" "transport mode must be iap"
  assert_contains "$yaml" 'oidc_audience: "999999999-client.apps.googleusercontent.com"' \
    "oidc_audience must be the discovered client id, not a Cloud-Run-resource audience string"
  assert_contains "$yaml" "platform_auth_sa: \"transport-sa@${PROJECT}.iam.gserviceaccount.com\"" \
    "platform_auth_sa must be the transport SA email"
}

test_teardown_transport_sa_deletes_when_marked() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false"
  assert_eq "true" "$HYBRID_TRANSPORT_SA_DELETED" "a marked transport SA must be deleted"
  assert_eq "false" "$HYBRID_TRANSPORT_SA_DELETE_FAILED" "a successful delete must not be reported as failed"
  assert_eq "$email" "$HYBRID_TRANSPORT_SA_TEARDOWN_EMAIL" "the checked SA email must be recorded for the summary"
  assert_contains "$(gcloud_log)" "iam service-accounts delete ${email}" "the transport SA must actually be deleted"
}

test_teardown_transport_sa_removes_iap_binding_before_delete() {
  fresh_gcloud_state
  local email log binding_line binding_at delete_at
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false"
  log="$(gcloud_log)"
  binding_line="$(echo "$log" | grep 'iap web remove-iam-policy-binding' || true)"
  assert_contains "$binding_line" "--service=${INSTANCE_NAME_TEST}-iap-proxy" "the binding removal must target the hub's own Cloud Run service"
  assert_contains "$binding_line" "--member=serviceAccount:${email}" "the binding removal must name the transport SA"
  assert_contains "$binding_line" "--role=roles/iap.httpsResourceAccessor" "the binding removal must name the accessor role"
  assert_eq "removed" "$HYBRID_TRANSPORT_SA_BINDING_STATE" "a successful removal must be recorded"
  binding_at="$(line_number "iap web remove-iam-policy-binding" "$log")"
  delete_at="$(line_number "iam service-accounts delete" "$log")"
  assert_true "$([[ -n "$binding_at" && -n "$delete_at" && "$binding_at" -lt "$delete_at" ]] && echo true || echo false)" \
    "the binding must be removed before the SA is deleted"
}

# The stub answers `iap web remove-iam-policy-binding` as for a policy
# that already contains conditional bindings: a call without
# --condition=None fails.
test_gcloud_stub_iap_remove_binding_without_condition_fails() {
  fresh_gcloud_state
  local out rc
  out="$(gcloud iap web remove-iam-policy-binding --resource-type=cloud-run --service=svc \
    --region="$REGION" --project="$PROJECT" --member="serviceAccount:transport-sa@${PROJECT}.iam.gserviceaccount.com" \
    --role=roles/iap.httpsResourceAccessor --quiet 2>&1 >/dev/null)"
  rc=$?
  assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" \
    "an IAP web binding removal without --condition must fail on a policy with conditions"
  assert_contains "$out" "(gcloud.iap.web.remove-iam-policy-binding) Removing a binding without specifying a condition from a policy containing conditions is prohibited in non-interactive mode." \
    "the removal failure must carry gcloud's error text"
}

test_teardown_transport_sa_iap_binding_removal_passes_condition_none() {
  fresh_gcloud_state
  local email out out_file binding_line
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  out_file="$(mktemp)"
  hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false" >"$out_file" 2>&1
  out="$(cat "$out_file")"
  rm -f "$out_file"
  binding_line="$(gcloud_log | grep 'iap web remove-iam-policy-binding' | head -1 || true)"
  assert_contains "$binding_line" " --condition=None " \
    "the IAP binding removal must pass --condition=None as its own argument"
  assert_eq "removed" "$HYBRID_TRANSPORT_SA_BINDING_STATE" "the removal must be recorded as removed"
  assert_eq "true" "$HYBRID_TRANSPORT_SA_DELETED" "the SA must be deleted after the removal"
  assert_eq "false" "$HYBRID_TRANSPORT_SA_DELETE_FAILED" "the happy path must not be reported as failed"
  assert_not_contains "$out" "Removing a binding without specifying a condition" \
    "the removal must not hit the conditional-policy error"
}

test_teardown_transport_sa_skips_binding_when_service_gone() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "true"
  assert_eq "0" "$(gcloud_log | grep -c 'iap web remove-iam-policy-binding' || true)" \
    "with the Cloud Run service confirmed gone, its IAM policy (and this binding) went with it"
  assert_eq "absent" "$HYBRID_TRANSPORT_SA_BINDING_STATE" "the binding must be recorded as absent"
  assert_eq "true" "$HYBRID_TRANSPORT_SA_DELETED" "the SA must still be deleted"
}

test_teardown_transport_sa_binding_already_absent_is_fine() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  set_iap_web_remove_binding_error "ERROR: Policy binding with the specified principal, role, and condition not found!"
  hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false"
  assert_eq "absent" "$HYBRID_TRANSPORT_SA_BINDING_STATE" "an already-absent binding counts as removed"
  assert_eq "true" "$HYBRID_TRANSPORT_SA_DELETED" "the SA must still be deleted"
  assert_eq "false" "$HYBRID_TRANSPORT_SA_DELETE_FAILED" "an already-absent binding is not a failure"
}

test_teardown_transport_sa_binding_failure_keeps_sa() {
  fresh_gcloud_state
  local email out
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  set_iap_web_remove_binding_error ""
  out="$(hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false" 2>&1)"
  hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false" >/dev/null 2>&1
  assert_eq "failed" "$HYBRID_TRANSPORT_SA_BINDING_STATE" "a binding removal failure must be recorded"
  assert_eq "true" "$HYBRID_TRANSPORT_SA_DELETE_FAILED" "a binding removal failure must be reported as a failure"
  assert_eq "false" "$HYBRID_TRANSPORT_SA_DELETED" "the SA must not be reported deleted"
  assert_eq "0" "$(gcloud_log | grep -c 'iam service-accounts delete' || true)" \
    "the SA must be kept so a re-run can retry the binding removal and the delete together"
  assert_contains "$out" "PERMISSION_DENIED" "the removal error must be printed"
}

test_teardown_transport_sa_never_touches_unmarked() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "some-other-marker"
  hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false" 2>/dev/null
  assert_eq "false" "$HYBRID_TRANSPORT_SA_DELETED" "an unmarked transport SA must not be reported as deleted"
  assert_eq "true" "$HYBRID_TRANSPORT_SA_DELETE_FAILED" "an unmarked transport SA must be reported as a failure to resolve"
  assert_eq "0" "$(gcloud_log | grep -c 'iam service-accounts delete' || true)" \
    "an unmarked transport SA must never actually be deleted"
  assert_eq "0" "$(gcloud_log | grep -c 'iap web remove-iam-policy-binding' || true)" \
    "an unmarked transport SA's bindings must never be touched either"
}

test_teardown_transport_sa_not_found_is_a_no_op() {
  fresh_gcloud_state
  hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false"
  assert_eq "false" "$HYBRID_TRANSPORT_SA_DELETED" "nothing to delete, so nothing should be reported deleted"
  assert_eq "true" "$HYBRID_TRANSPORT_SA_NOT_FOUND" "a positive not-found must be recorded as not found"
  assert_eq "false" "$HYBRID_TRANSPORT_SA_DELETE_FAILED" "a positive not-found must not be reported as a failure"
  assert_eq "0" "$(gcloud_log | grep -c 'iam service-accounts delete' || true)" "nothing must be deleted"
}

test_teardown_transport_sa_describe_permission_denied_is_a_failure() {
  fresh_gcloud_state
  local email out
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  set_service_account_describe_error "$email"
  out="$(hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false" 2>&1)"
  hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false" >/dev/null 2>&1
  assert_eq "true" "$HYBRID_TRANSPORT_SA_DELETE_FAILED" "a describe error other than not-found leaves the state unknown: a failure"
  assert_eq "false" "$HYBRID_TRANSPORT_SA_NOT_FOUND" "a permission error must never read as not found"
  assert_eq "false" "$HYBRID_TRANSPORT_SA_DELETED" "nothing was deleted"
  assert_contains "$HYBRID_TRANSPORT_SA_KEPT_REASON" "state unknown" "the kept reason must say why"
  assert_contains "$out" "PERMISSION_DENIED" "the describe error must be printed"
}

test_teardown_transport_sa_gone_at_delete_reported_not_found() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  set_service_account_delete_not_found "$email"
  hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false"
  assert_eq "true" "$HYBRID_TRANSPORT_SA_NOT_FOUND" "an SA found by describe but not found by delete is reported not found"
  assert_eq "false" "$HYBRID_TRANSPORT_SA_DELETED" "it must not be reported as deleted by this teardown"
  assert_eq "false" "$HYBRID_TRANSPORT_SA_DELETE_FAILED" "a positive not-found on delete is not a failure"
}

test_teardown_transport_sa_delete_failure_reported() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  set_service_account_delete_will_fail "$email"
  hybrid_teardown_transport_sa "$HUB" "$PROJECT" "${INSTANCE_NAME_TEST}-iap-proxy" "us-central1" "false" 2>/dev/null
  assert_eq "false" "$HYBRID_TRANSPORT_SA_DELETED" "a failed delete must not be reported as deleted"
  assert_eq "true" "$HYBRID_TRANSPORT_SA_DELETE_FAILED" "a failed delete must be reported as failed"
  assert_eq "delete failed" "$HYBRID_TRANSPORT_SA_KEPT_REASON" "the kept reason must say the delete failed"
}

# =====================================================================
# Restricted user access (hybrid_resolve_user_access): the mode and
# domains the tier writes into settings.yaml, and what it refuses.
# =====================================================================

# _user_access_config JSON — points CONFIG_FILE at a config file holding
# JSON, inside the per-test stub state dir so the per-test cleanup
# removes it.
_user_access_config() {
  CONFIG_FILE="${GCLOUD_STUB_STATE_DIR}/user-access-config.json"
  printf '%s' "$1" > "$CONFIG_FILE"
}

# _expect_user_access_refused ADMIN_EMAIL EXPECTED_SUBSTRING DESCRIPTION
_expect_user_access_refused() {
  run_expect_fail hybrid_resolve_user_access "$1"
  assert_true "$([[ $RUN_EXIT_CODE -ne 0 ]] && echo true || echo false)" "$3 must be refused"
  assert_contains "$RUN_OUTPUT" "$2" "$3: the error should say why"
}

test_user_access_defaults_to_invite_only() {
  fresh_gcloud_state
  _user_access_config '{"hub_name": "demohub"}'
  hybrid_resolve_user_access "admin@example.com"
  assert_eq "invite_only" "$HYBRID_USER_ACCESS_MODE" "with no user_access_mode configured, the tier writes invite_only"
  assert_eq '    user_access_mode: "invite_only"' "$HYBRID_USER_ACCESS_YAML" \
    "the default block is the mode alone, nested under auth:"
}

test_user_access_defaults_to_invite_only_without_config_file() {
  fresh_gcloud_state
  CONFIG_FILE=""
  hybrid_resolve_user_access "admin@example.com"
  assert_eq "invite_only" "$HYBRID_USER_ACCESS_MODE" "with no config file at all, the tier writes invite_only"
}

test_user_access_domain_restricted_with_domains() {
  fresh_gcloud_state
  _user_access_config '{"user_access_mode": "domain_restricted", "authorized_domains": ["Example.com", "*.corp.example.org"]}'
  hybrid_resolve_user_access "admin@example.com"
  assert_eq "domain_restricted" "$HYBRID_USER_ACCESS_MODE" "a configured domain_restricted mode is respected"
  assert_eq "$(printf '    user_access_mode: "domain_restricted"\n    authorized_domains:\n      - "example.com"\n      - "*.corp.example.org"')" \
    "$HYBRID_USER_ACCESS_YAML" "the configured domains are written, lowercased, in order"
}

test_user_access_invite_only_with_domains() {
  fresh_gcloud_state
  _user_access_config '{"authorized_domains": ["example.com"]}'
  hybrid_resolve_user_access "admin@example.com"
  assert_eq "invite_only" "$HYBRID_USER_ACCESS_MODE" "domains alone keep the invite_only default"
  assert_contains "$HYBRID_USER_ACCESS_YAML" '      - "example.com"' "configured domains are written with invite_only too"
}

test_user_access_refuses_open_mode() {
  fresh_gcloud_state
  _user_access_config '{"user_access_mode": "open"}'
  _expect_user_access_refused "admin@example.com" "user_access_mode 'open' is not supported" "user_access_mode open"
}

test_user_access_refuses_empty_mode() {
  fresh_gcloud_state
  _user_access_config '{"user_access_mode": ""}'
  _expect_user_access_refused "admin@example.com" "user_access_mode '' is not supported" "an empty user_access_mode"
}

test_user_access_refuses_unknown_mode() {
  fresh_gcloud_state
  _user_access_config '{"user_access_mode": "everyone"}'
  _expect_user_access_refused "admin@example.com" "use invite_only (the default when unset) or domain_restricted" \
    "an unknown user_access_mode"
}

test_user_access_refuses_non_string_mode() {
  fresh_gcloud_state
  _user_access_config '{"user_access_mode": 5}'
  _expect_user_access_refused "admin@example.com" "user_access_mode must be a string" "a non-string user_access_mode"
}

test_user_access_refuses_domain_restricted_without_domains() {
  fresh_gcloud_state
  _user_access_config '{"user_access_mode": "domain_restricted"}'
  _expect_user_access_refused "admin@example.com" "needs at least one authorized_domains entry" \
    "domain_restricted with no authorized_domains"
  _user_access_config '{"user_access_mode": "domain_restricted", "authorized_domains": []}'
  _expect_user_access_refused "admin@example.com" "needs at least one authorized_domains entry" \
    "domain_restricted with an empty authorized_domains list"
}

test_user_access_refuses_service_account_domains() {
  fresh_gcloud_state
  local domain
  for domain in "gserviceaccount.com" "developer.gserviceaccount.com" "demo-project.iam.gserviceaccount.com" \
      "*.gserviceaccount.com" "*.iam.gserviceaccount.com" "*.com" "Demo-Project.IAM.GServiceAccount.com"; do
    _user_access_config "{\"user_access_mode\": \"domain_restricted\", \"authorized_domains\": [\"example.com\", \"${domain}\"]}"
    _expect_user_access_refused "admin@example.com" "matches service account email addresses" \
      "authorized_domains entry '${domain}'"
  done
}

test_user_access_allows_ordinary_wildcard_domain() {
  fresh_gcloud_state
  _user_access_config '{"user_access_mode": "domain_restricted", "authorized_domains": ["*.example.com"]}'
  hybrid_resolve_user_access "admin@example.com"
  assert_eq "domain_restricted" "$HYBRID_USER_ACCESS_MODE" "an ordinary *.domain wildcard is accepted"
}

test_user_access_refuses_malformed_domains() {
  fresh_gcloud_state
  local domain
  for domain in "user@example.com" "example" "*" "*.com." "exa mple.com" "-example.com"; do
    _user_access_config "{\"authorized_domains\": [\"${domain}\"]}"
    _expect_user_access_refused "admin@example.com" "is not a domain name" "authorized_domains entry '${domain}'"
  done
}

test_user_access_refuses_non_list_domains() {
  fresh_gcloud_state
  _user_access_config '{"authorized_domains": "example.com"}'
  _expect_user_access_refused "admin@example.com" "authorized_domains must be a list" "a string authorized_domains"
}

test_user_access_refuses_empty_admin_email() {
  fresh_gcloud_state
  _user_access_config '{}'
  _expect_user_access_refused "" "needs admin_email set" "an empty admin_email with the tier on"
}

test_user_access_refuses_service_account_admin_email() {
  fresh_gcloud_state
  _user_access_config '{}'
  local email
  for email in "robot@demo-project.iam.gserviceaccount.com" \
      "$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com" \
      "Robot@Demo-Project.IAM.GServiceAccount.COM" "123456789-compute@developer.gserviceaccount.com"; do
    _expect_user_access_refused "$email" "is a service account" "admin_email '${email}'"
  done
}

test_user_access_refuses_admin_email_with_whitespace_or_comma() {
  fresh_gcloud_state
  _user_access_config '{}'
  local sa email
  sa="$(hybrid_transport_sa_name "$HUB")@${PROJECT}.iam.gserviceaccount.com"
  _expect_user_access_refused "${sa} " "is a service account" "a service-account admin_email with a trailing space"
  _expect_user_access_refused "$(printf '%s\t' "$sa")" "is a service account" "a service-account admin_email with a trailing tab"
  _expect_user_access_refused "$(printf ' \t%s' "$sa")" "is a service account" "a service-account admin_email with leading whitespace"
  for email in "${sa},admin@example.com" "admin@example.com,${sa}" "admin@example.com, ${sa}" \
      "admin@example.com ${sa}" "$(printf 'admin@example.com\t%s' "$sa")"; do
    _expect_user_access_refused "$email" "must be a single email address" "admin_email '${email}'"
  done
  _expect_user_access_refused "   " "needs admin_email set" "a whitespace-only admin_email"
}

test_user_access_accepts_ordinary_admin_email() {
  fresh_gcloud_state
  _user_access_config '{}'
  hybrid_resolve_user_access "Admin.User@Example.com"
  assert_eq "invite_only" "$HYBRID_USER_ACCESS_MODE" "an ordinary user admin_email passes"
  hybrid_resolve_user_access " admin@example.com "
  assert_eq "invite_only" "$HYBRID_USER_ACCESS_MODE" "surrounding whitespace on an ordinary admin_email is trimmed and passes"
}

test_user_access_config_present_ignores_value_types() {
  fresh_gcloud_state
  _user_access_config '{"user_access_mode": 5}'
  assert_true "$(hybrid_user_access_config_present && echo true || echo false)" \
    "a wrongly typed user_access_mode still counts as present, without exiting"
  _user_access_config '{"authorized_domains": "example.com"}'
  assert_true "$(hybrid_user_access_config_present && echo true || echo false)" \
    "a wrongly typed authorized_domains still counts as present, without exiting"
}

test_user_access_config_present_detects_either_key() {
  fresh_gcloud_state
  _user_access_config '{"hub_name": "demohub"}'
  assert_false "$(hybrid_user_access_config_present && echo true || echo false)" "no user access keys: not present"
  _user_access_config '{"user_access_mode": "invite_only"}'
  assert_true "$(hybrid_user_access_config_present && echo true || echo false)" "user_access_mode set: present"
  _user_access_config '{"authorized_domains": []}'
  assert_true "$(hybrid_user_access_config_present && echo true || echo false)" "an empty authorized_domains list: present"
}
