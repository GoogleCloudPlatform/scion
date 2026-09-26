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

# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

# scripts/single-node-vm/hybrid-tier.sh — hybrid-tier (attach-only GKE
# target) additions for deploy.sh.
#
# This file is meant to be `source`d by deploy.sh, never executed directly.
# It depends on the caller already having defined `config_get`,
# `config_prompt`, `info`, `warn`, `err` and `PYTHON` (deploy.sh defines all
# of these before sourcing this file), and on `set -euo pipefail` already
# being in effect.
#
# Every function here is a plain shell function operating on explicit
# arguments and a small set of documented globals, so the test
# harness under tests/ can source this file on its own -- with its own stub
# `config_get`/`info`/`warn`/`err` and a stub `gcloud` on PATH -- without
# ever loading or running deploy.sh itself.
#
# The cluster itself is always an existing, attach-only target -- this
# file never creates or deletes one.
#
# Array expansions throughout this file use the
# `${arr[@]+"${arr[@]}"}` idiom instead of a bare `"${arr[@]}"` wherever
# the array in question can legitimately be empty. Under `set -u`, an
# empty array's bare `"${arr[@]}"` is an unbound-variable error on bash
# older than 4.4 (macOS ships 3.2), even though it is a documented no-op
# on 4.4+; the `+` form is the portable way to say "zero or more
# elements" on every bash version this script might run under.

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
# The dedicated NFS squash identity: a system account with no login shell
# and no home, whose primary group is the existing "scion" group (the
# broker's own group -- see docs/deploy/agent-runbook-single-node-vm.md).
# Deliberately distinct from the "scion" user itself: squashing every NFS
# client to the broker's own uid would mean any pod that can reach the
# export can act as the broker on the shared tree, which is exactly the
# upper-dir-symlink-plant vector the dedicated identity exists to close.
# shellcheck disable=SC2034 # read by deploy.sh after sourcing this file
readonly HYBRID_NFS_SQUASH_USER="scion-nfs"
# The NFS export root: the mount point of a dedicated, size-capped ext4
# filesystem, loop-mounted from a single image file that itself lives on
# the VM's boot disk (per the design's boot-disk provisioning choice).
# The export root is the ROOT of that filesystem, not a subdirectory of
# it: with no_subtree_check (required so NFSv4 file handles survive a
# restart), a subdirectory export lets a forged file handle reach any
# inode on the whole containing filesystem. Giving the export its own
# filesystem means there is nothing else on that filesystem for a forged
# handle to reach.
# shellcheck disable=SC2034 # read by deploy.sh after sourcing this file
readonly HYBRID_NFS_EXPORT_ROOT="/srv/scion-shared"
# The backing image file for the export filesystem above. Lives on the
# boot disk, outside the export root itself. Created once, at whatever
# size gke_target.shared_dir_image_size_gb (default 20) specifies; never
# re-created or shrunk on a later run. Growing it later is a manual,
# documented operation (grow the file, then resize2fs) -- see
# docs/deploy/hybrid-tier.md.
# shellcheck disable=SC2034 # read by deploy.sh after sourcing this file
readonly HYBRID_NFS_IMAGE_PATH="/var/lib/scion-nfs/export.img"

# Marker types, all the exact token "scion-deployment=<hub_name>", by
# resource kind (matches the base-resource marker convention deploy.sh
# itself already uses for the VM, Cloud Run proxy, service account, and
# Cloud Router):
#   firewall rule -> description       (see hybrid_ensure_firewall_rules)
#   static internal IP address -> description
#                                       (see hybrid_ensure_internal_ip_new_vm /
#                                       _existing_vm)
#   Kubernetes namespace/PV/PVC -> label
#                                       (see hybrid_k8s_preflight)
# This marker is purely informational for ownership checks: it is never
# checked or corrected on resources this tier doesn't itself create or
# adopt.

# _hybrid_gcloud_not_found TEXT
#
# True only for gcloud's own specific "genuinely absent" signal: a
# bounded NOT_FOUND status token, a "code=404" ResponseError, or an
# "HTTP 404" status line -- never a bare "404" substring, which shows up
# in plenty of text that has nothing to do with absence: a cluster
# literally named "hub-404", a project ID like "team-404-prod" inside a
# URL, "Timeout after 404 seconds", or a proxy's own unrelated status
# line mentioning a request id that happens to contain "404". Some
# permission-denied responses are deliberately worded to avoid
# confirming a resource's existence to a caller who can't see it (for
# example "...not found or permission denied"), and those must never be
# treated as "gone": anything mentioning permission or forbidden is
# excluded outright, checked first, before the not-found signal itself.
# A suggestion response ("Did you mean ...") means gcloud found
# something close enough to suggest, which is not the same as
# confirming the requested name is absent, so that's excluded too.
_hybrid_gcloud_not_found() {
  local text="$1"
  if echo "$text" | grep -qiE 'permission|forbidden'; then
    return 1
  fi
  if echo "$text" | grep -qiE 'did you mean'; then
    return 1
  fi
  echo "$text" | grep -qE 'code=404\b|\bHTTP 404\b|(^|[^A-Za-z0-9_])NOT_FOUND($|[^A-Za-z0-9_])'
}

# _hybrid_registry_is_loopback REGISTRY
#
# True if REGISTRY's host component is one of the common loopback
# spellings (localhost, 127.0.0.0/8, ::1, or 0.0.0.0), with or without a
# port. Takes the part of REGISTRY before its first '/'
# (the host[:port]), strips an IPv6 literal's brackets first (so a port
# after "]" is never confused with the host itself), then strips a
# ":port" suffix from whatever's left, so "localhost/...",
# "localhost:PORT/...", "127.0.0.1/...", "127.0.0.1:PORT/...",
# "[::1]/..." and "[::1]:PORT/..." are all refused identically, since
# they all reach the same place on a GKE node.
_hybrid_registry_is_loopback() {
  local registry="$1" host
  host="${registry%%/*}"
  if [[ "$host" == \[*\]* ]]; then
    host="${host#\[}"
    host="${host%%\]*}"
  else
    host="${host%%:*}"
  fi
  case "$host" in
    localhost|127.*|0.0.0.0|::1) return 0 ;;
    *) return 1 ;;
  esac
}

# _hybrid_validate_target_fields NAME LOCATION RESOLVED_PROJECT NAMESPACE \
#   PVC_NAME PROJECT_ID
#
# The gke_target.* field-syntax and project-match validation shared by
# both hybrid_read_config (create, below) and deploy.sh's --delete path:
# a malformed or foreign-project cluster reference must be refused the
# same way regardless of which path read it, rather than create-mode
# validating strictly while delete-mode reads the same fields with none
# of these checks. RESOLVED_PROJECT is the caller's already-defaulted
# value (gke_target.project if set, else PROJECT_ID), not the raw config
# value, so this only ever sees one project value, not two. Exits
# non-zero with an actionable, field-naming message on the first problem
# found. Never touches HYBRID_ENABLED or any GKE_* global itself -- the
# caller is responsible for those.
_hybrid_validate_target_fields() {
  local name="$1" location="$2" resolved_project="$3" namespace="$4" pvc_name="$5" project_id="$6"

  if [[ -z "$location" ]]; then
    err "gke_target.location is required when gke_target.name is set."
    exit 1
  fi
  if ! [[ "$name" =~ ^[a-z]([-a-z0-9]{0,38}[a-z0-9])?$ ]]; then
    err "gke_target.name '${name}' is not a valid GKE cluster name (lowercase letters, digits and hyphens; must start with a letter and not end with a hyphen)."
    exit 1
  fi
  if ! [[ "$location" =~ ^[a-z]+-[a-z]+[0-9]+(-[a-z])?$ ]]; then
    err "gke_target.location '${location}' is not a valid GCP zone or region."
    exit 1
  fi
  if ! [[ "$resolved_project" =~ ^[a-z][-a-z0-9]{4,28}[a-z0-9]$ ]]; then
    err "gke_target.project '${resolved_project}' is not a valid GCP project ID."
    exit 1
  fi
  if ! [[ "$namespace" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]]; then
    err "gke_target.namespace '${namespace}' is not a valid Kubernetes namespace name (lowercase alphanumeric and hyphens, must start and end with an alphanumeric, max 63 characters)."
    exit 1
  fi
  if ! [[ "$pvc_name" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]]; then
    err "gke_target.pvc_name '${pvc_name}' is not a valid Kubernetes object name (lowercase alphanumeric and hyphens, must start and end with an alphanumeric, max 63 characters)."
    exit 1
  fi
  if [[ "$resolved_project" != "$project_id" ]]; then
    err "gke_target.project ('${resolved_project}') must match this hub's project ('${project_id}'). Attaching a cluster in a different project is not supported yet."
    exit 1
  fi
}

# hybrid_read_config PROJECT_ID HUB_NAME
#
# Reads gke_target.{name,location,project,namespace,pvc_name} via the
# caller's config_get. In interactive mode (no CONFIG_FILE), offers to
# enable the tier and prompts for all five fields via config_prompt when
# the operator opts in, showing each derived default (empty input
# accepts it); in config-file mode, an absent or empty gke_target.name
# means the tier is off, full stop, with no prompt.
#
# Sets HYBRID_ENABLED to "true" or "false".
# Exits non-zero with an actionable message on any invalid gke_target.*
# or shared_dir_image_size_gb value, or a missing $PYTHON.
hybrid_read_config() {
  local project_id="$1" hub_name="$2"
  local cfg_name cfg_location cfg_project cfg_namespace cfg_pvc_name cfg_image_size_gb
  local hybrid_choice
  local default_namespace default_pvc_name
  default_namespace="$(hybrid_k8s_default_namespace "$hub_name")"
  default_pvc_name="$(hybrid_k8s_default_pvc_name "$hub_name")"

  cfg_name="$(config_get 'gke_target.name' '')"
  cfg_location="$(config_get 'gke_target.location' '')"
  cfg_project="$(config_get 'gke_target.project' '')"
  cfg_namespace="$(config_get 'gke_target.namespace' "$default_namespace")"
  cfg_pvc_name="$(config_get 'gke_target.pvc_name' "$default_pvc_name")"
  cfg_image_size_gb="$(config_get 'gke_target.shared_dir_image_size_gb' '20')"

  if [[ -z "$cfg_name" && -z "${CONFIG_FILE:-}" ]]; then
    echo ""
    config_prompt hybrid_choice "Attach an existing GKE cluster as a second runtime (hybrid tier)? [y/N]: " "n"
    if [[ "$(echo "$hybrid_choice" | tr '[:upper:]' '[:lower:]')" == "y" ]]; then
      config_prompt cfg_name "GKE cluster name: " ""
      config_prompt cfg_location "GKE cluster location (zone or region): " ""
      config_prompt cfg_project "GKE cluster project [${project_id}]: " "${project_id}"
      config_prompt cfg_namespace "Kubernetes namespace for the shared tree [${default_namespace}]: " "${default_namespace}"
      config_prompt cfg_pvc_name "PersistentVolumeClaim name [${default_pvc_name}]: " "${default_pvc_name}"
    fi
  fi

  if [[ -z "$cfg_name" ]]; then
    HYBRID_ENABLED=false
    return 0
  fi

  GKE_NAME="$cfg_name"
  GKE_LOCATION="$cfg_location"
  GKE_PROJECT="${cfg_project:-$project_id}"
  _hybrid_validate_target_fields "$cfg_name" "$cfg_location" "$GKE_PROJECT" "$cfg_namespace" "$cfg_pvc_name" "$project_id"

  if ! [[ "$cfg_image_size_gb" =~ ^[1-9][0-9]*$ ]]; then
    err "gke_target.shared_dir_image_size_gb '${cfg_image_size_gb}' must be a positive integer (gigabytes)."
    exit 1
  fi

  if ! command -v "$PYTHON" &>/dev/null; then
    err "Python interpreter '${PYTHON}' is required for the hybrid tier (cluster discovery and firewall-rule checks) but was not found. Set PYTHON=/path/to/python3 or disable the tier."
    exit 1
  fi

  # shellcheck disable=SC2034 # read by deploy.sh after sourcing this file
  HYBRID_ENABLED=true
  # shellcheck disable=SC2034 # read by hybrid_k8s_ensure_objects/_teardown_check as an override
  GKE_NAMESPACE="$cfg_namespace"
  # shellcheck disable=SC2034 # read by hybrid_k8s_ensure_objects/_teardown_check as an override
  GKE_PVC_NAME="$cfg_pvc_name"
  # shellcheck disable=SC2034 # read by deploy.sh, passed to hybrid_nfs_export_script
  HYBRID_SHARED_DIR_IMAGE_SIZE_GB="$cfg_image_size_gb"
}

# _hybrid_cluster_ref — a "name (project: P, location: L)" string for
# actionable messages, built from the globals hybrid_read_config sets.
_hybrid_cluster_ref() {
  echo "${GKE_NAME} (project: ${GKE_PROJECT}, location: ${GKE_LOCATION})"
}

# _hybrid_region_from_location LOCATION
#
# A GKE cluster's location is either regional (e.g. us-central1, already
# a region) or zonal (e.g. us-central1-a, a region plus a single-letter
# zone suffix). Subnetworks are regional resources, so a zonal location
# needs that suffix stripped before it can be passed to
# `gcloud compute networks subnets describe --region`.
_hybrid_region_from_location() {
  local location="$1"
  if [[ "$location" =~ ^(.+)-[a-z]$ ]]; then
    echo "${BASH_REMATCH[1]}"
  else
    echo "$location"
  fi
}

# _hybrid_validate_node_subnet_cidr CIDR
#
# Validates CIDR as a syntactically valid IPv4 network in canonical form
# (no host bits set), and refuses anything broader than /8: 0.0.0.0/0
# would list every routable address as a trusted NFS client, and nothing
# a single GKE node subnet legitimately needs is ever wider than /8.
# Prints nothing and returns non-zero on any problem; the caller supplies
# the actionable error message.
_hybrid_validate_node_subnet_cidr() {
  local cidr="$1"
  "$PYTHON" -c "
import ipaddress, sys
try:
    net = ipaddress.ip_network(sys.argv[1], strict=True)
except ValueError:
    sys.exit(1)
sys.exit(0 if (net.version == 4 and net.prefixlen >= 8) else 1)
" "$cidr"
}

# hybrid_discover HUB_NETWORK
#
# Verifies the configured GKE cluster exists and is on the hub's own
# network, then discovers its node network tag, its node subnet's
# primary IP range, and its pod CIDR. Every gcloud call here is
# read-only (describe/list); nothing is created. Sets GKE_NODE_TAG,
# GKE_NODE_SUBNET_CIDR, and GKE_POD_CIDR. Exits non-zero with an
# actionable message, naming the cluster and (where relevant) what was
# found, if the cluster can't be described, is on a different network
# than the hub VM, its node subnet or pod CIDR can't be determined or
# has an invalid or dangerously broad IP range, or no single node tag
# can be discovered -- always before any resource is created. Every
# failed gcloud call's message includes gcloud's own stderr rather than
# assuming "not found": a permission or API-disabled error looks nothing
# like a missing cluster, and reporting it as one would send an operator
# chasing the wrong fix.
#
# The node subnet -- not the pod CIDR or any secondary range -- is what
# the NFS export's client list is built from: nodes, not pods, originate
# the NFS mount traffic that reaches the VM. The NFS firewall allow
# rule's source is unrelated to this and continues to use the node
# network tag discovered above, not an IP range. The pod CIDR is used
# only by the separate hub-allow firewall rule (tcp:8080), whose traffic
# genuinely does originate from pod IPs.
#
# Node-tag discovery reads the node tag GKE assigns, from the cluster's
# own GKE-managed firewall rules, the same source for Standard and
# Autopilot clusters alike: it lists the firewall rules on the cluster's
# network, and looks among them for the one rule matching ^gke-.+-all$,
# direction INGRESS, whose source ranges include this cluster's own pod
# CIDR -- pod CIDRs are unique within a VPC, so this ties the rule to
# this specific cluster. That rule must carry exactly one target tag,
# matching ^gke-.+-node$, and the matching <same-prefix>-vms rule must
# exist and carry the same single target tag. Anything else -- no
# matching rule, more than one, the wrong shape of tags, or the -vms
# rule missing or disagreeing -- refuses to guess and fails instead,
# listing whatever it found.
hybrid_discover() {
  local hub_network="$1"
  local cluster_ref
  cluster_ref="$(_hybrid_cluster_ref)"

  local describe_json describe_err
  describe_err="$(mktemp)"
  if ! describe_json="$(gcloud container clusters describe "${GKE_NAME}" \
      --project="${GKE_PROJECT}" --location="${GKE_LOCATION}" \
      --format=json 2>"${describe_err}")"; then
    err "Could not describe GKE cluster ${cluster_ref}:"
    err "  $(cat "${describe_err}")"
    rm -f "${describe_err}"
    exit 1
  fi
  rm -f "${describe_err}"

  local network
  network="$(echo "$describe_json" | "$PYTHON" -c "
import json, sys
d = json.load(sys.stdin)
print(d.get('network') or '')
")"
  if [[ -z "$network" ]]; then
    err "Could not determine the network for GKE cluster ${cluster_ref} from its description."
    exit 1
  fi
  if [[ "$network" != "$hub_network" ]]; then
    err "GKE cluster ${cluster_ref} is on network '${network}', but this hub uses network '${hub_network}'. Attaching a cluster on a different network is not supported."
    exit 1
  fi

  # The NFS export's client list is the cluster's node subnet, never the
  # pod CIDR or any secondary range: nodes are what actually originate
  # NFS traffic, and the node subnet is the narrowest range that's still
  # guaranteed to cover every node regardless of how pod/service ranges
  # are laid out.
  local subnetwork
  subnetwork="$(echo "$describe_json" | "$PYTHON" -c "
import json, sys
d = json.load(sys.stdin)
print(d.get('subnetwork') or '')
")"
  if [[ -z "$subnetwork" ]]; then
    err "Could not determine the node subnetwork for GKE cluster ${cluster_ref} from its description."
    exit 1
  fi

  local subnet_region
  subnet_region="$(_hybrid_region_from_location "$GKE_LOCATION")"

  local subnet_err subnet_json
  subnet_err="$(mktemp)"
  if ! subnet_json="$(gcloud compute networks subnets describe "$subnetwork" \
      --region="$subnet_region" --project="${GKE_PROJECT}" --format=json 2>"${subnet_err}")"; then
    err "Could not describe node subnetwork '${subnetwork}' (region: ${subnet_region}) for GKE cluster ${cluster_ref}:"
    err "  $(cat "${subnet_err}")"
    rm -f "${subnet_err}"
    exit 1
  fi
  rm -f "${subnet_err}"

  local node_cidr
  node_cidr="$(echo "$subnet_json" | "$PYTHON" -c "
import json, sys
d = json.load(sys.stdin)
print(d.get('ipCidrRange') or '')
")"
  if [[ -z "$node_cidr" ]] || ! _hybrid_validate_node_subnet_cidr "$node_cidr"; then
    err "GKE cluster ${cluster_ref}'s node subnetwork '${subnetwork}' has an invalid or dangerously broad primary IP range ('${node_cidr:-empty}'). Refusing to build an NFS export client list from it."
    exit 1
  fi
  # shellcheck disable=SC2034 # consumed by the NFS export function
  GKE_NODE_SUBNET_CIDR="$node_cidr"

  # The pod CIDR is what the hub-allow firewall rule (tcp:8080, GKE agent
  # pods reaching the hub directly) sources from -- unlike the NFS export,
  # which sources from the node subnet, because kubelet (not the pod)
  # originates NFS mount traffic, but a pod's own HTTP request to the hub
  # genuinely comes from its pod IP. Read from two places in the same
  # describe JSON and require them to agree: `clusterIpv4Cidr` is the
  # cluster-wide value, `ipAllocationPolicy.clusterIpv4CidrBlock` is the
  # allocation-policy's own record of it; a real disagreement between the
  # two would mean this code is looking at the wrong field for this
  # cluster's provisioning mode, which is safer to fail on than to guess.
  local pod_cidr pod_cidr_alt
  pod_cidr="$(echo "$describe_json" | "$PYTHON" -c "
import json, sys
d = json.load(sys.stdin)
print(d.get('clusterIpv4Cidr') or '')
")"
  pod_cidr_alt="$(echo "$describe_json" | "$PYTHON" -c "
import json, sys
d = json.load(sys.stdin)
print((d.get('ipAllocationPolicy') or {}).get('clusterIpv4CidrBlock') or '')
")"
  if [[ -z "$pod_cidr" || -z "$pod_cidr_alt" ]]; then
    err "Could not determine the pod CIDR for GKE cluster ${cluster_ref} from its description (clusterIpv4Cidr or ipAllocationPolicy.clusterIpv4CidrBlock is missing)."
    exit 1
  fi
  if [[ "$pod_cidr" != "$pod_cidr_alt" ]]; then
    err "GKE cluster ${cluster_ref} reports two different pod CIDRs (clusterIpv4Cidr='${pod_cidr}', ipAllocationPolicy.clusterIpv4CidrBlock='${pod_cidr_alt}'). Refusing to guess which one is right."
    exit 1
  fi
  if ! _hybrid_validate_node_subnet_cidr "$pod_cidr"; then
    err "GKE cluster ${cluster_ref}'s pod CIDR '${pod_cidr}' is invalid or dangerously broad (refusing anything broader than /8). Refusing to build the hub-allow firewall rule's source range from it."
    exit 1
  fi
  # shellcheck disable=SC2034 # consumed by the hub-allow firewall rule
  GKE_POD_CIDR="$pod_cidr"

  # Autopilot does not expose node instance groups or templates as
  # Compute resources in the project -- every instance-group describe
  # call 404s, even for a cluster with running nodes -- so the tag is
  # read from GKE's own auto-created firewall rules instead, which exist
  # identically for every cluster, of either type. The cluster's pod CIDR
  # (unique within a VPC) ties the right pair of rules to this cluster
  # without needing to reconstruct GKE's own truncated-name/hash scheme.
  # The list carries no --filter: it returns every rule in the project,
  # and the Python below keeps only the rules on this network, by exact
  # match on the network URL's last path segment.
  local fw_list_json fw_list_err
  fw_list_err="$(mktemp)"
  if ! fw_list_json="$(gcloud compute firewall-rules list --project="${GKE_PROJECT}" \
      --format=json 2>"${fw_list_err}")"; then
    err "Could not list firewall rules in project ${GKE_PROJECT} to discover the GKE node tag for cluster ${cluster_ref} (network '${network}'):"
    err "  $(cat "${fw_list_err}")"
    rm -f "${fw_list_err}"
    exit 1
  fi
  rm -f "${fw_list_err}"

  local disc_result disc_status disc_detail
  disc_result="$(echo "$fw_list_json" | "$PYTHON" -c "
import json, sys, re

raw = sys.stdin.read()
rules = json.loads(raw) if raw.strip() else []
pod_cidr = sys.argv[1]
network = sys.argv[2]
all_re = re.compile(r'^gke-[a-z0-9-]+-all\$')
node_re = re.compile(r'^gke-[a-z0-9-]+-node\$')


def describe(r):
    ranges = ','.join(r.get('sourceRanges') or []) or 'none'
    return '%s (direction=%s, sourceRanges=%s)' % (r.get('name') or '', r.get('direction') or '', ranges)


# The list is unfiltered, so this is the only network check: a rule
# counts only if its network URL ends in exactly /networks/<network>. A
# same-named, same-pod-CIDR rule on any other network, including one
# whose name merely starts or ends with this one, is never mistaken for
# this cluster's own.
on_network = [r for r in rules if (r.get('network') or '').endswith('/networks/' + network)]

name_matches = [r for r in on_network if all_re.fullmatch(r.get('name') or '')]
if not name_matches:
    print('NONE::no firewall rule matching ^gke-[a-z0-9-]+-all\$ was found on network %s' % network)
    sys.exit(0)

candidates = [r for r in name_matches
              if (r.get('direction') or '') == 'INGRESS'
              and pod_cidr in (r.get('sourceRanges') or [])]
if not candidates:
    found = '; '.join(describe(r) for r in name_matches)
    print('NONE::found %d rule(s) named like a GKE-managed rule, but none had direction INGRESS with source range %s: %s'
          % (len(name_matches), pod_cidr, found))
    sys.exit(0)

if len(candidates) > 1:
    found = ', '.join(sorted((r.get('name') or '') for r in candidates))
    print('AMBIGUOUS::%d candidate rules matched: %s' % (len(candidates), found))
    sys.exit(0)

rule = candidates[0]
tags = rule.get('targetTags') or []
if len(tags) != 1:
    print('BADTAGS::%s has %d target tag(s): %s' % (rule.get('name') or '', len(tags), ', '.join(tags) or 'none'))
    sys.exit(0)

tag = tags[0]
if not node_re.fullmatch(tag):
    print(\"BADTAGSHAPE::%s's target tag '%s' does not match ^gke-[a-z0-9-]+-node\$\" % (rule.get('name') or '', tag))
    sys.exit(0)

prefix = (rule.get('name') or '')[:-len('-all')]
vms_name = prefix + '-vms'
vms_rule = next((r for r in on_network if (r.get('name') or '') == vms_name), None)
if vms_rule is None:
    print('NOVMS::%s was not found' % vms_name)
    sys.exit(0)

vms_tags = vms_rule.get('targetTags') or []
if vms_tags != [tag]:
    print(\"VMSMISMATCH::%s's target tag(s) (%s) do not match %s's (%s)\"
          % (vms_name, ', '.join(vms_tags) or 'none', rule.get('name') or '', tag))
    sys.exit(0)

print('OK::%s' % tag)
" "$pod_cidr" "$network")"

  disc_status="${disc_result%%::*}"
  disc_detail="${disc_result#*::}"

  if [[ "$disc_status" != "OK" ]]; then
    err "Could not discover a GKE node network tag for cluster ${cluster_ref}: its GKE-managed firewall rules are missing or ambiguous (${disc_detail}). This is fixed on the cluster's own firewall rules, not in this script."
    exit 1
  fi

  GKE_NODE_TAG="$disc_detail"
}

# hybrid_nfs_fsid HUB_NAME
#
# A stable, deterministic NFS export fsid for this hub: a UUID5 derived
# from the hub name, so it's the same across every re-run without needing
# a separate value generated once and recorded somewhere. nfs-utils
# accepts a UUID for `fsid=` (the documented way to keep file handles
# stable and avoid ESTALE) just as readily as a small integer, and a UUID
# needs no coordination between hubs the way hand-assigned integers would.
hybrid_nfs_fsid() {
  local hub_name="$1"
  "$PYTHON" -c "
import uuid, sys
print(uuid.uuid5(uuid.NAMESPACE_DNS, 'scion-hub-' + sys.argv[1] + '.nfs-export'))
" "$hub_name"
}

# hybrid_nfs_export_line EXPORT_ROOT CIDR ANONUID ANONGID FSID
#
# Renders one `/etc/exports.d` line for the hybrid tier's NFS export:
# `sync` (required so both Docker and GKE writers see consistent state),
# `no_subtree_check`, `all_squash` to the given anonuid/anongid, `sec=sys`
# (this tier's authorization is by source IP -- see the docs, not by NFS
# auth), `mp` (mountpoint-only: knfsd itself refuses to serve EXPORT_ROOT
# unless it's currently a mountpoint), and the given fsid. `mp` is the
# export-side half of the fail-closed guarantee the caller's mount check
# provides at provisioning time -- it also covers a mount that fails on a
# later reboot, which a one-time provisioning check can't. There is no
# separate squash group: anongid is the "scion" group's own gid, since
# the Phase 2 leaf ACL grants that group access, and a different anongid
# would make GKE's writes invisible to it. Pure string rendering -- no
# gcloud or SSH calls -- so it's directly unit-testable; the caller is
# responsible for actually reading the anonuid/anongid off the VM and
# writing the result to a file.
hybrid_nfs_export_line() {
  local export_root="$1" cidr="$2" anonuid="$3" anongid="$4" fsid="$5"
  echo "${export_root} ${cidr}(rw,sync,no_subtree_check,all_squash,anonuid=${anonuid},anongid=${anongid},sec=sys,mp,fsid=${fsid})"
}

# hybrid_nfs_squash_identity_script SQUASH_USER
#
# Renders the remote script that idempotently creates the dedicated NFS
# squash identity (a system account, no home, no login shell, primary
# group "scion") if it doesn't already exist, then validates it --
# whether freshly created just now or pre-existing from an earlier run --
# against every property the squash identity's security purpose depends
# on: uid not 0, uid in the system range (at or below /etc/login.defs'
# SYS_UID_MAX, defaulting to 999 when that file or key is missing, same
# as useradd's own default), primary group exactly "scion", login shell
# /usr/sbin/nologin, and (as before) a uid distinct from the "scion"
# (broker) user's own -- squashing every NFS client to the broker's own
# identity would let any pod that can reach the export act as the broker
# on the shared tree. A pre-existing account that fails any of these was
# not created by this script and is refused outright rather than reused,
# since silently squashing to it could be squashing to something with
# far more privilege than intended. Only on success does it print
# "SQUASH_UID:SCION_GID" for the caller to capture. Pure string
# rendering -- no gcloud or SSH calls -- so it's directly unit-testable;
# the caller (deploy.sh) is responsible for actually running the result
# over SSH.
hybrid_nfs_squash_identity_script() {
  local squash_user="$1"
  cat <<SCRIPT
set -euo pipefail
id ${squash_user} >/dev/null 2>&1 || sudo useradd -r -M -N -g scion -s /usr/sbin/nologin ${squash_user}
SQUASH_UID=\$(id -u ${squash_user})
SQUASH_GROUP=\$(id -gn ${squash_user})
SQUASH_SHELL=\$(getent passwd ${squash_user} | cut -d: -f7)
SYS_UID_MAX=\$(awk -F'[ \\t]+' '\$1 == "SYS_UID_MAX" {print \$2}' /etc/login.defs 2>/dev/null | tail -1 || true)
case "\$SYS_UID_MAX" in ''|*[!0-9]*) SYS_UID_MAX=999 ;; esac
SCION_UID=\$(id -u scion)
SCION_GID=\$(getent group scion | cut -d: -f3)
if [ "\$SQUASH_UID" -eq 0 ]; then
  echo "The NFS squash user (${squash_user}) must not be uid 0." >&2
  exit 1
fi
if [ "\$SQUASH_UID" -gt "\$SYS_UID_MAX" ]; then
  echo "The NFS squash user (${squash_user})'s uid (\$SQUASH_UID) must be a system uid (<= \$SYS_UID_MAX)." >&2
  exit 1
fi
if [ "\$SQUASH_GROUP" != "scion" ]; then
  echo "The NFS squash user (${squash_user})'s primary group must be scion, found '\$SQUASH_GROUP'." >&2
  exit 1
fi
if [ "\$SQUASH_SHELL" != "/usr/sbin/nologin" ]; then
  echo "The NFS squash user (${squash_user})'s login shell must be /usr/sbin/nologin, found '\$SQUASH_SHELL'." >&2
  exit 1
fi
if [ "\$SQUASH_UID" = "\$SCION_UID" ]; then
  echo 'The NFS squash uid must not equal the scion (broker) uid.' >&2
  exit 1
fi
echo "\${SQUASH_UID}:\${SCION_GID}"
SCRIPT
}

# hybrid_nfs_export_script EXPORT_ROOT CIDR ANONUID ANONGID FSID HUB_NAME \
#   IMAGE_PATH IMAGE_SIZE_GB
#
# IMAGE_PATH is a parameter (rather than reading the HYBRID_NFS_IMAGE_PATH
# constant directly) purely so tests can point it at a throwaway temp
# path when executing the rendered script for real -- the real caller
# (deploy.sh) always passes $HYBRID_NFS_IMAGE_PATH.
#
# Renders the remote script that idempotently:
#   - creates the backing image file (IMAGE_PATH) at IMAGE_SIZE_GB
#      gigabytes with fallocate (which actually reserves the space,
#      unlike a sparse truncate, and fails clearly -- removing the
#      partial file -- if the disk can't hold it) and formats it ext4,
#      but ONLY the first time -- an existing image file is never
#      re-created or re-mkfs'd, since doing so would destroy whatever
#      the export already holds;
#   - adds an /etc/fstab entry loop-mounting that image at EXPORT_ROOT,
#      with BOTH x-systemd.before=nfs-server.service (orders the mount
#      before the NFS server unit) AND x-systemd.required-by=
#      nfs-server.service (makes it an actual dependency, not just an
#      ordering) -- before= alone only orders the units; on a failed
#      mount it would let nfs-server start anyway and export whatever's
#      really at EXPORT_ROOT (the boot disk's root filesystem), exactly
#      the exposure this dedicated filesystem exists to close. Because
#      required-by= is set, systemd-fstab-generator does not also
#      attach this mount to local-fs.target (systemd.mount(5)), so a
#      failed mount blocks only nfs-server, never boot; nofail is kept
#      anyway, defensively, to keep that same "boot never blocks on
#      this mount" property true even if required-by= is ever removed
#      from this line. The fsck pass is 0: systemd-fstab-generator only
#      ever schedules fsck for device paths, so a nonzero pass on this
#      loop-mounted regular file is a no-op that just logs a boot-time
#      warning.
#      Then mounts it if it isn't already, and verifies (findmnt +
#      losetup) that whatever ends up mounted at EXPORT_ROOT is
#      actually the loop device backing IMAGE_PATH, not a stray tmpfs
#      or bind mount left over from something else;
#   - fails closed -- before writing or activating anything below --
#      if EXPORT_ROOT is not actually a mountpoint after that: this is
#      the deploy-time half of the guarantee; the exports line's own
#      `mp` option (see hybrid_nfs_export_line) is the export-side half,
#      and additionally covers a mount that fails on a later reboot,
#      which this one-time check can't. A
#      /etc/systemd/system/scion-hub.service.d/10-scion-shared.conf
#      drop-in adds RequiresMountsFor=EXPORT_ROOT to scion-hub.service
#      (tier-on only; the base service install is unaffected when the
#      tier is off), so the hub itself -- which creates shared-dir
#      paths under EXPORT_ROOT for the co-located Docker broker -- can
#      never start against an unmounted export and write to the boot
#      disk's root filesystem instead, the same split this whole layout
#      exists to prevent on the NFS side;
#   - sets ownership/mode on the now-mounted export root (scion:scion,
#      mode 2755 so the squash uid can't write it), installs
#      nfs-kernel-server if it isn't already, disables NFSv2/v3/4.0 and
#      UDP via /etc/nfs.conf.d (this tier is NFSv4.1+/TCP-only; the PV
#      pins nfsvers=4.1) and masks rpcbind (unneeded once v2/v3
#      are off), verifying the socket mask took,
#      writes this hub's own file under /etc/exports.d/ (using
#      hybrid_nfs_export_line for the rendered line, so the two stay in
#      sync), re-exports, and enables the server under its canonical
#      unit name (nfs-server; nfs-kernel-server is only the Debian/
#      Ubuntu package name). A restart -- not just enable --now, which
#      would be a no-op against an already-running unit -- only happens
#      when it's actually needed: either this is the first time
#      /etc/nfs.conf.d/scion-hub.conf has this exact content (apt's
#      postinst already started the server with the stock v2/v3/UDP-
#      enabled config before this script's write, so the very first
#      run must restart to pick up v4.1/TCP-only), or nfs-server isn't
#      confirmed already active, or /proc/fs/nfsd/versions shows the
#      running kernel still has v3 or v4.0 enabled (a version the
#      config write alone can't retroactively disable for an already-
#      running server). Every later re-run with unchanged content, a
#      confirmed-active server, and v3/v4.0 both confirmed off leaves
#      it running untouched, avoiding an NFSv4 grace-period stall
#      (during which GKE clients can't reclaim or open new state) on
#      every redeploy. Fails closed toward restarting: an inconclusive
#      "is it active" or "which versions are on" check is treated the
#      same as "needs a restart".
# Always rewrites the exports file and re-exports, which is how the
# export picks up a changed CIDR on re-run with no separate drift
# detection needed. Pure string rendering -- no gcloud or SSH calls --
# so it's directly unit-testable; the caller is responsible for actually
# running the result over SSH, after the squash identity script above
# has already run.
hybrid_nfs_export_script() {
  local export_root="$1" cidr="$2" anonuid="$3" anongid="$4" fsid="$5" hub_name="$6" \
    image_path="$7" image_size_gb="$8" fstab_path="${9:-/etc/fstab}"
  local export_line image_dir
  export_line="$(hybrid_nfs_export_line "$export_root" "$cidr" "$anonuid" "$anongid" "$fsid")"
  image_dir="$(dirname "$image_path")"
  local fstab_line="${image_path} ${export_root} ext4 loop,nofail,x-systemd.before=nfs-server.service,x-systemd.required-by=nfs-server.service 0 0"
  cat <<SCRIPT
set -euo pipefail
sudo mkdir -p ${image_dir}
# Refuse before touching anything -- no fallocate, no mkfs, no fstab
# write -- if EXPORT_ROOT is already in use by a layout this script
# doesn't manage: a pre-existing mount from a different source (for
# example a manually provisioned image at another path), or an existing
# fstab line for EXPORT_ROOT that isn't exactly this one. Checking this
# first means a manually created layout is refused outright instead of
# silently getting a second image allocated and a second fstab line
# appended alongside it, only to fail later at the mount-source check.
if mountpoint -q ${export_root}; then
  existing_src="\$(findmnt -n -o SOURCE --mountpoint ${export_root} 2>/dev/null || true)"
  existing_back="\$(losetup -n -O BACK-FILE "\$existing_src" 2>/dev/null || true)"
  if [ "\$existing_back" != "${image_path}" ]; then
    echo "${export_root} is already mounted (source: \${existing_src:-<unknown>}, backing file: \${existing_back:-<none>}), not from the image this script manages (${image_path}); refusing to continue on top of an existing layout it doesn't recognize." >&2
    exit 1
  fi
fi
if awk -v mp="${export_root}" -v exact="${fstab_line}" '
  /^[[:space:]]*#/ { next }
  \$0 == exact { next }
  { m2 = \$2; sub(/\/\$/, "", m2); if (m2 == mp) found=1 }
  END { exit !found }
' ${fstab_path} 2>/dev/null; then
  echo "${fstab_path} already has a line for ${export_root} that does not match the expected entry; refusing to continue. Expected: ${fstab_line}" >&2
  exit 1
fi
if grep -qxF "${fstab_line}" ${fstab_path} 2>/dev/null; then
  : # fstab already has exactly the expected line; nothing to add.
else
  echo "${fstab_line}" | sudo tee -a ${fstab_path} > /dev/null
  sudo systemctl daemon-reload
fi
if [ ! -e ${image_path} ]; then
  sudo fallocate -l ${image_size_gb}G ${image_path} || { sudo rm -f ${image_path}; echo "could not reserve ${image_size_gb}G for ${image_path} (insufficient disk space?); refusing to continue" >&2; exit 1; }
  sudo mkfs.ext4 -F -q ${image_path}
fi
sudo mkdir -p ${export_root}
if ! mountpoint -q ${export_root}; then
  sudo mount ${export_root}
fi
if ! mountpoint -q ${export_root}; then
  echo "${export_root} is not a mountpoint after attempting to mount ${image_path}; refusing to write or activate the NFS export on the boot disk's root filesystem instead." >&2
  exit 1
fi
mount_src="\$(findmnt -n -o SOURCE --mountpoint ${export_root})"
mount_back="\$(losetup -n -O BACK-FILE "\$mount_src" 2>/dev/null || true)"
if [ "\$mount_back" != "${image_path}" ]; then
  echo "${export_root} is mounted, but not from the loop device backing ${image_path} (found: \${mount_back:-<none>}); refusing to write or activate the NFS export against the wrong filesystem" >&2
  exit 1
fi
sudo chown scion:scion ${export_root}
sudo chmod 2755 ${export_root}
sudo install -d -m 0755 /etc/systemd/system/scion-hub.service.d
cat <<HUBDROPIN | sudo tee /etc/systemd/system/scion-hub.service.d/10-scion-shared.conf > /dev/null
[Unit]
RequiresMountsFor=${export_root}
HUBDROPIN
sudo systemctl daemon-reload
sudo install -d -m 0755 /etc/exports.d
if ! dpkg -s nfs-kernel-server >/dev/null 2>&1; then
  sudo apt-get update -y
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y nfs-kernel-server
fi
sudo install -d -m 0755 /etc/nfs.conf.d
NFS_CONF_CONTENT="\$(cat <<'NFSCONF'
[nfsd]
vers2=n
vers3=n
vers4.0=n
udp=n
NFSCONF
)"
NFS_CONF_EXISTING="\$(cat /etc/nfs.conf.d/scion-hub.conf 2>/dev/null || true)"
NFS_CONF_NEEDS_RESTART=true
if [ "\$NFS_CONF_EXISTING" = "\$NFS_CONF_CONTENT" ]; then
  NFS_CONF_NEEDS_RESTART=false
fi
echo "\$NFS_CONF_CONTENT" | sudo tee /etc/nfs.conf.d/scion-hub.conf > /dev/null
sudo systemctl mask --now rpcbind.service rpcbind.socket
[ "\$(systemctl is-enabled rpcbind.socket 2>/dev/null || true)" = masked ] || { echo "rpcbind.socket did not mask; refusing to continue" >&2; exit 1; }
echo '${export_line}' | sudo tee /etc/exports.d/scion-hub-${hub_name}.exports > /dev/null
sudo exportfs -ra
sudo systemctl enable nfs-server
# Even with an unchanged config and an already-active server, the kernel's
# own enabled-version set is what the config drop-in actually controls --
# an interrupted first run can write the drop-in, die before ever
# restarting, and leave nfs-server serving whatever it started with
# (v2/v3/v4.0 all still on). Restart unless the kernel confirms v3 and
# v4.0 are both off; an unreadable versions file is treated the same as
# "not confirmed", not as "fine".
NFS_VERSIONS_OK=false
if NFS_VERSIONS_CONTENT="\$(cat /proc/fs/nfsd/versions 2>/dev/null)"; then
  case " \$NFS_VERSIONS_CONTENT " in
    *" -3 "*)
      case " \$NFS_VERSIONS_CONTENT " in
        *" -4.0 "*) NFS_VERSIONS_OK=true ;;
      esac
      ;;
  esac
fi
if [ "\$NFS_CONF_NEEDS_RESTART" = "true" ] || ! systemctl is-active --quiet nfs-server || [ "\$NFS_VERSIONS_OK" != "true" ]; then
  sudo systemctl restart nfs-server
fi
echo 'NFS export configured.'
SCRIPT
}

# hybrid_cloud_run_label_args SERVICE_NAME PROJECT_ID REGION HUB_NAME
#
# Echoes the --labels=... argument to pass to `gcloud beta run deploy`, or
# nothing, based on whether the service already exists: the base-marker
# convention is additive and create-only, so the label is only added
# when this is the first create. `describe` failing only means "not
# found by that exact call"; it is not itself a positive absence signal
# (a permissions error looks the same from the exit code alone, and
# real Cloud Run 404 text -- "Cannot find service [X]" -- carries none
# of the NOT_FOUND/404 tokens gcloud's other APIs use, so text-matching
# describe's error here would silently never fire). Absence is instead
# confirmed the same way `_hybrid_firewall_rule_absent` does: a `list`
# call that both succeeds and comes back empty. Any other outcome --
# the service is listed, or the list call itself fails -- fails safe
# toward "assume it exists" (no label), on the reasoning that a missing
# marker is corrected by nothing, while a wrong marker on an existing,
# unrelated service is not easily undone.
hybrid_cloud_run_label_args() {
  local service_name="$1" project_id="$2" region="$3" hub_name="$4"
  if gcloud run services describe "$service_name" \
      --project="$project_id" --region="$region" >/dev/null 2>/dev/null; then
    return 0
  fi
  local list_output
  if list_output="$(gcloud run services list --project="$project_id" --region="$region" \
      --filter="metadata.name=${service_name}" --format="value(metadata.name)" 2>/dev/null)"; then
    if [[ -z "$list_output" ]]; then
      echo "--labels=scion-deployment=${hub_name}"
    fi
  fi
}

# _hybrid_firewall_rule_fields
#
# Reads the JSON body of `gcloud compute firewall-rules describe
# --format=json` from stdin and echoes every field able to widen,
# narrow or disable the rule, normalized for exact comparison and joined
# by ASCII unit separator (0x1f, not tab -- see below):
#   network  direction  action  allowed/denied (all entries)  sourceTags
#   sourceRanges  sourceServiceAccounts  targetTags
#   targetServiceAccounts  destinationRanges  disabled  priority
# Every other field is ignored, including non-scoping settable fields
# (description, logConfig) and output-only metadata (id, name, kind,
# selfLink, creationTimestamp) -- none of them can change what the rule
# does. network is reduced to its short name (the self-link's last path
# segment). action is ALLOW, DENY, or MALFORMED if both allowed[] and
# denied[] are present (GCP never returns that shape; treating it as a
# guaranteed mismatch is simpler than picking one). Every allowed/denied
# entry is included, each rendered "proto:port,port" (ports sorted) or
# bare "proto", the whole set sorted and joined with ';', so a second
# entry changes the field regardless of order. sourceTags and
# sourceRanges are compared as separate fields, since GCP ORs them when
# both are set. The remaining list fields are comma-joined and sorted.
#
# 0x1f, not tab, because bash's `read` collapses runs of IFS
# *whitespace* -- which includes tab -- silently merging adjacent empty
# fields and shifting every field after them, which is wrong here: "this
# field is empty" is a meaningful, distinct result, not a fencepost to
# skip.
_hybrid_firewall_rule_fields() {
  "$PYTHON" -c "
import json, sys
d = json.load(sys.stdin)
network = (d.get('network') or '').rstrip('/').rsplit('/', 1)[-1]
direction = d.get('direction') or ''
allowed = d.get('allowed') or []
denied = d.get('denied') or []
if allowed and denied:
    action = 'MALFORMED'
    rules = allowed + denied
elif allowed:
    action = 'ALLOW'
    rules = allowed
elif denied:
    action = 'DENY'
    rules = denied
else:
    action = ''
    rules = []

def rule_sig(r):
    proto = r.get('IPProtocol', '')
    ports = ','.join(sorted(r.get('ports') or []))
    return (proto + ':' + ports) if ports else proto

rules_sig = ';'.join(sorted(rule_sig(r) for r in rules))
source_tags = ','.join(sorted(d.get('sourceTags') or []))
source_ranges = ','.join(sorted(d.get('sourceRanges') or []))
source_sas = ','.join(sorted(d.get('sourceServiceAccounts') or []))
target_tags = ','.join(sorted(d.get('targetTags') or []))
target_sas = ','.join(sorted(d.get('targetServiceAccounts') or []))
dest_ranges = ','.join(sorted(d.get('destinationRanges') or []))
disabled = 'true' if d.get('disabled') else 'false'
priority = str(d['priority']) if d.get('priority') is not None else ''
sep = chr(0x1f)  # ASCII unit separator -- see the bash side for why not tab
print(sep.join([network, direction, action, rules_sig, source_tags,
                source_ranges, source_sas, target_tags, target_sas,
                dest_ranges, disabled, priority]))
"
}

# _hybrid_check_rule_drift NAME PROJECT_ID JSON NETWORK DIRECTION ACTION \
#   RULES SOURCE_TYPE SOURCE_VALUE TARGET_TAG PRIORITY
#
# Compares an already-marked, pre-existing rule against every field able
# to widen, narrow or disable it: direction, action, disabled, priority,
# network, allowed/denied (all entries), sourceTags, sourceRanges,
# sourceServiceAccounts, targetServiceAccounts, targetTags,
# destinationRanges. Every other field is ignored, including
# non-scoping settable fields (description, logConfig) and output-only
# metadata (id, name, kind, selfLink, creationTimestamp). SOURCE_TYPE is
# "tag" or "range"; the *other* source field is expected empty, since a
# rule this tier creates only ever sets one of them, and GCP ORs the two
# when both are present. Service accounts and destination ranges are
# always expected empty, and disabled is always expected false.
#
# On a full match, returns 0 silently. On any mismatch, prints every
# differing field, then the delete remediation, then an `update`
# remediation too, but only when `update` can converge to *exactly* the
# expected rule -- every other field already matches, and the only drift
# left is something it can set directly (the expected source type,
# target tags, priority). If a field would need to be cleared, or it is
# not one of the fields this function offers an update for, only delete
# is offered. For the nfs-deny rule, the delete remediation is the
# two-rule, order-preserving sequence
# (delete allow, delete deny, re-run deploy.sh) rather than a bare
# delete, since deleting the deny alone would leave tcp:2049 reachable
# through the allow rule with nothing to deny it. Never corrects
# anything itself.
_hybrid_check_rule_drift() {
  local name="$1" project_id="$2" json="$3" exp_network="$4" exp_direction="$5" \
    exp_action="$6" exp_rules="$7" source_type="$8" exp_source_value="$9" \
    exp_target_tag="${10}" exp_priority="${11}"

  local exp_source_tags="" exp_source_ranges=""
  if [[ "$source_type" == "tag" ]]; then
    exp_source_tags="$exp_source_value"
  else
    exp_source_ranges="$exp_source_value"
  fi

  local fields
  fields="$(echo "$json" | _hybrid_firewall_rule_fields)"
  local act_network act_direction act_action act_rules act_source_tags act_source_ranges \
    act_source_sas act_target_tags act_target_sas act_dest_ranges act_disabled act_priority
  IFS=$'\x1f' read -r act_network act_direction act_action act_rules act_source_tags \
    act_source_ranges act_source_sas act_target_tags act_target_sas act_dest_ranges \
    act_disabled act_priority <<< "$fields"

  local -a mismatches=()
  local update_converges=true

  if [[ "$act_network" != "$exp_network" ]]; then
    mismatches+=("network: expected '${exp_network}', found '${act_network}'")
    update_converges=false
  fi
  if [[ "$act_direction" != "$exp_direction" ]]; then
    mismatches+=("direction: expected '${exp_direction}', found '${act_direction}'")
    update_converges=false
  fi
  if [[ "$act_action" != "$exp_action" ]]; then
    mismatches+=("action: expected '${exp_action}', found '${act_action}'")
    update_converges=false
  fi
  if [[ "$act_rules" != "$exp_rules" ]]; then
    mismatches+=("ports: expected '${exp_rules}', found '${act_rules}'")
    update_converges=false
  fi
  if [[ "$act_source_tags" != "$exp_source_tags" ]]; then
    mismatches+=("source tags: expected '${exp_source_tags:-(empty)}', found '${act_source_tags:-(empty)}'")
    [[ "$source_type" != "tag" ]] && update_converges=false
  fi
  if [[ "$act_source_ranges" != "$exp_source_ranges" ]]; then
    mismatches+=("source ranges: expected '${exp_source_ranges:-(empty)}', found '${act_source_ranges:-(empty)}'")
    [[ "$source_type" != "range" ]] && update_converges=false
  fi
  if [[ "$act_source_sas" != "" ]]; then
    mismatches+=("source service accounts: expected '(empty)', found '${act_source_sas}'")
    update_converges=false
  fi
  if [[ "$act_target_tags" != "$exp_target_tag" ]]; then
    mismatches+=("target tags: expected '${exp_target_tag}', found '${act_target_tags}'")
  fi
  if [[ "$act_target_sas" != "" ]]; then
    mismatches+=("target service accounts: expected '(empty)', found '${act_target_sas}'")
    update_converges=false
  fi
  if [[ "$act_dest_ranges" != "" ]]; then
    mismatches+=("destination ranges: expected '(empty)', found '${act_dest_ranges}'")
    update_converges=false
  fi
  if [[ "$act_disabled" != "false" ]]; then
    mismatches+=("disabled: expected 'false', found '${act_disabled}'")
    update_converges=false
  fi
  if [[ "$act_priority" != "$exp_priority" ]]; then
    mismatches+=("priority: expected '${exp_priority}', found '${act_priority}'")
  fi

  if [[ ${#mismatches[@]} -eq 0 ]]; then
    return 0
  fi

  err "Firewall rule '${name}' carries this deployment's marker but its spec has drifted from what this tier expects:"
  local m
  for m in "${mismatches[@]}"; do
    err "  ${m}"
  done
  err "Refusing to auto-correct a marked rule. To restore the expected spec:"
  if [[ "$name" == *-nfs-deny ]]; then
    local allow_name="${name%-nfs-deny}-nfs-allow"
    err "  Delete both rules, in this order, then re-run deploy.sh (deleting only the deny rule would leave tcp:2049 reachable through the allow rule with nothing to deny it):"
    err "    gcloud compute firewall-rules delete ${allow_name} --project=${project_id} --quiet"
    err "    gcloud compute firewall-rules delete ${name} --project=${project_id} --quiet"
  else
    err "  gcloud compute firewall-rules delete ${name} --project=${project_id} --quiet"
    err "  Then re-run deploy.sh to recreate it with the expected spec."
  fi

  if [[ "$update_converges" == "true" ]]; then
    local source_flag
    if [[ "$source_type" == "tag" ]]; then
      source_flag="--source-tags=${exp_source_value}"
    else
      source_flag="--source-ranges=${exp_source_value}"
    fi
    err "Or update it in place:"
    err "  gcloud compute firewall-rules update ${name} --project=${project_id} --priority=${exp_priority} ${source_flag} --target-tags=${exp_target_tag}"
  fi

  return 1
}

# _hybrid_ensure_firewall_rule NAME PROJECT_ID MARKER NETWORK DIRECTION \
#   ACTION RULES SOURCE_TYPE SOURCE_VALUE TARGET_TAG PRIORITY
#
# Shared by hybrid_ensure_firewall_rules below. If a rule named NAME
# already exists: fails outright if its description isn't exactly MARKER
# (deploy.sh never adopts a same-named rule it does not already own);
# otherwise verifies its full security-relevant spec against the expected
# values via _hybrid_check_rule_drift, and fails (never auto-corrects) on
# any mismatch. Only a rule that both carries the marker and matches the
# expected spec is left alone. Otherwise creates it fresh, with the marker
# and expected spec.
_hybrid_ensure_firewall_rule() {
  local name="$1" project_id="$2" marker="$3" network="$4" direction="$5" action="$6" \
    rules="$7" source_type="$8" source_value="$9" target_tag="${10}" priority="${11}"

  local json
  if json="$(gcloud compute firewall-rules describe "${name}" \
      --project="${project_id}" --format=json 2>/dev/null)"; then
    local existing_desc
    existing_desc="$(echo "$json" | "$PYTHON" -c "import json,sys; print(json.load(sys.stdin).get('description') or '')")"
    if [[ "$existing_desc" != "$marker" ]]; then
      err "Firewall rule '${name}' already exists but does not carry this deployment's marker ('${marker}'). Refusing to modify it."
      exit 1
    fi
    if ! _hybrid_check_rule_drift "$name" "$project_id" "$json" "$network" "$direction" \
        "$action" "$rules" "$source_type" "$source_value" "$target_tag" "$priority"; then
      exit 1
    fi
    echo "  Firewall rule already exists and matches the expected spec: ${name}"
    return 0
  fi

  local source_flag
  if [[ "$source_type" == "tag" ]]; then
    source_flag="--source-tags=${source_value}"
  else
    source_flag="--source-ranges=${source_value}"
  fi

  gcloud compute firewall-rules create "${name}" \
    --project="${project_id}" \
    --description="${marker}" \
    --network="${network}" \
    --direction="${direction}" \
    --action="${action}" \
    --rules="${rules}" \
    "${source_flag}" \
    --target-tags="${target_tag}" \
    --priority="${priority}" \
    --quiet
  echo "  Created firewall rule: ${name}"
}

# hybrid_ensure_firewall_rules HUB_NAME PROJECT_ID NETWORK
#
# Creates (or verifies the marker and full spec of) the three firewall
# rules this tier needs, all targeting scion-hub-<hub>-nfs and carrying
# the exact description token scion-deployment=<hub>:
#   scion-hub-<hub>-nfs-allow  INGRESS ALLOW tcp:2049 from the discovered
#                              node tag (GKE_NODE_TAG; set by
#                              hybrid_discover), priority 900.
#   scion-hub-<hub>-nfs-deny   INGRESS DENY  tcp:2049 from 0.0.0.0/0,
#                              priority 950.
#   scion-hub-<hub>-hub-allow  INGRESS ALLOW tcp:8080 from the discovered
#                              pod CIDR (GKE_POD_CIDR; set by
#                              hybrid_discover), priority 900 -- lets GKE
#                              agent pods reach the hub directly over the
#                              VPC. No paired deny for hub-allow, by
#                              design: other VPC-internal reachability to
#                              this port comes from the network's own
#                              pre-existing rules, not something this
#                              tier provisions.
# The two NFS rules are created deny first, then allow, so an interrupted
# run can never leave an allow rule in place without its paired deny
# (teardown deletes in the opposite order: allow first, then deny, for
# the same reason in reverse). Sets HYBRID_ALLOW_NAME, HYBRID_DENY_NAME,
# and HYBRID_HUB_ALLOW_NAME. Call only after hybrid_discover has set
# GKE_NODE_TAG and GKE_POD_CIDR.
hybrid_ensure_firewall_rules() {
  local hub_name="$1" project_id="$2" network="$3"
  local marker="scion-deployment=${hub_name}"
  local target_tag
  target_tag="$(hybrid_vm_tag "${hub_name}")"

  HYBRID_ALLOW_NAME="scion-hub-${hub_name}-nfs-allow"
  HYBRID_DENY_NAME="scion-hub-${hub_name}-nfs-deny"
  HYBRID_HUB_ALLOW_NAME="scion-hub-${hub_name}-hub-allow"

  _hybrid_ensure_firewall_rule "${HYBRID_DENY_NAME}" "${project_id}" "${marker}" \
    "${network}" "INGRESS" "DENY" "tcp:2049" "range" "0.0.0.0/0" "${target_tag}" "950"

  _hybrid_ensure_firewall_rule "${HYBRID_ALLOW_NAME}" "${project_id}" "${marker}" \
    "${network}" "INGRESS" "ALLOW" "tcp:2049" "tag" "${GKE_NODE_TAG}" "${target_tag}" "900"

  _hybrid_ensure_firewall_rule "${HYBRID_HUB_ALLOW_NAME}" "${project_id}" "${marker}" \
    "${network}" "INGRESS" "ALLOW" "tcp:8080" "range" "${GKE_POD_CIDR}" "${target_tag}" "900"
}

# hybrid_vm_tag HUB_NAME
#
# Echoes the network tag a VM needs for the firewall rules above to take
# effect on it.
hybrid_vm_tag() {
  echo "scion-hub-$1-nfs"
}

# hybrid_apply_vm_tag INSTANCE_NAME ZONE PROJECT_ID HUB_NAME
#
# Idempotently adds the hybrid-tier network tag to an EXISTING VM. A newly
# created VM gets the same tag at creation time instead, via --tags.
# add-tags is a set union on GCP's side, so calling it again when the tag
# is already present is a harmless no-op -- no separate existence check is
# needed first.
hybrid_apply_vm_tag() {
  local instance="$1" zone="$2" project_id="$3" hub_name="$4"
  gcloud compute instances add-tags "${instance}" \
    --zone="${zone}" --project="${project_id}" \
    --tags="$(hybrid_vm_tag "${hub_name}")" \
    --quiet
}

# hybrid_teardown_check HUB_NAME PROJECT_ID
#
# Looks up all three hybrid-tier firewall rules (the two NFS rules and
# the hub-allow rule) with a single `firewall-rules list` call and
# classifies each: marked -> HYBRID_TEARDOWN_DELETE (both allow rules
# before the deny); found but unmarked -> HYBRID_TEARDOWN_SKIP, and
# HYBRID_TEARDOWN_FAILED=true; not found -> ignored. A failed list call
# also sets HYBRID_TEARDOWN_FAILED, without populating either array --
# "unknown" must never look like "nothing to protect". $PYTHON is needed,
# and preflighted with a clear error, only when something was actually
# found to classify. Never deletes anything itself; the caller must
# treat HYBRID_TEARDOWN_FAILED=true as reason to abort the entire
# teardown (hybrid_teardown_preflight below does this).
hybrid_teardown_check() {
  local hub_name="$1" project_id="$2"
  local marker="scion-deployment=${hub_name}"
  local name_allow="scion-hub-${hub_name}-nfs-allow"
  local name_deny="scion-hub-${hub_name}-nfs-deny"
  local name_hub_allow="scion-hub-${hub_name}-hub-allow"

  HYBRID_TEARDOWN_FAILED=false
  HYBRID_TEARDOWN_DELETE=()
  HYBRID_TEARDOWN_SKIP=()

  local list_json list_err
  list_err="$(mktemp)"
  if ! list_json="$(gcloud compute firewall-rules list --project="${project_id}" \
      --filter="name=(${name_allow} ${name_deny} ${name_hub_allow})" --format=json 2>"${list_err}")"; then
    err "Could not list firewall rules to check hybrid-tier ownership; aborting teardown rather than assuming none exist:"
    err "  $(cat "${list_err}")"
    rm -f "${list_err}"
    # shellcheck disable=SC2034 # read by callers after this call returns
    HYBRID_TEARDOWN_FAILED=true
    return 0
  fi
  rm -f "${list_err}"

  # Nothing to classify, so nothing needs $PYTHON either -- this keeps
  # the common tier-off/no-hybrid-rules case working even without a
  # Python interpreter on PATH, since it never has anything to parse.
  if [[ "$list_json" == "[]" ]]; then
    return 0
  fi

  if ! command -v "$PYTHON" &>/dev/null; then
    err "Python interpreter '${PYTHON}' is required to check hybrid-tier firewall rule ownership during teardown, but was not found."
    # shellcheck disable=SC2034 # read by callers after this call returns
    HYBRID_TEARDOWN_FAILED=true
    return 0
  fi

  local name desc
  for name in "$name_allow" "$name_hub_allow" "$name_deny"; do
    desc="$(echo "$list_json" | "$PYTHON" -c "
import json, sys
name = sys.argv[1]
for r in json.load(sys.stdin):
    if r.get('name') == name:
        print(r.get('description') or '')
        break
else:
    print('__NOT_FOUND__')
" "$name")"
    if [[ "$desc" == "__NOT_FOUND__" ]]; then
      continue
    fi
    if [[ "$desc" == "$marker" ]]; then
      HYBRID_TEARDOWN_DELETE+=("${name}")
    else
      HYBRID_TEARDOWN_SKIP+=("${name}")
      # shellcheck disable=SC2034 # read by callers after this call returns
      HYBRID_TEARDOWN_FAILED=true
    fi
  done
}

# hybrid_teardown_preflight HUB_NAME PROJECT_ID
#
# The full teardown-safety check deploy.sh runs before deleting anything:
# calls hybrid_teardown_check, prints the classification block ("  found
# (marked): <name>" / "  SKIPPED (unmarked): <name>"), and returns
# non-zero whenever hybrid_teardown_check sets HYBRID_TEARDOWN_FAILED.
# Never deletes anything. deploy.sh calls this unconditionally in
# --delete mode, even when the hybrid tier is off in the current config:
# teardown has no other way to know whether the tier was ever turned on
# for this hub, so it always checks for these rule names.
hybrid_teardown_preflight() {
  local hub_name="$1" project_id="$2"
  hybrid_teardown_check "$hub_name" "$project_id"

  local name
  for name in ${HYBRID_TEARDOWN_DELETE[@]+"${HYBRID_TEARDOWN_DELETE[@]}"}; do
    echo "  found (marked): ${name}"
  done
  for name in ${HYBRID_TEARDOWN_SKIP[@]+"${HYBRID_TEARDOWN_SKIP[@]}"}; do
    echo "  SKIPPED (unmarked): ${name}"
  done

  if [[ "$HYBRID_TEARDOWN_FAILED" != "true" ]]; then
    return 0
  fi
  if [[ ${#HYBRID_TEARDOWN_SKIP[@]} -gt 0 ]]; then
    err "Refusing to tear down: the SKIPPED rule(s) above do not carry this deployment's marker, so ownership can't be confirmed. Resolve the naming collision manually, then re-run teardown."
  fi
  return 1
}

# _hybrid_firewall_rule_absent NAME PROJECT_ID
#
# Positively confirms a rule doesn't exist via a `list` call, the same
# fail-closed pattern as the ownership check: returns 0 only when the
# list call succeeds AND comes back empty. Any other outcome (the rule is
# listed, or the list call itself fails) returns 1 -- "unknown" is never
# treated as "gone".
_hybrid_firewall_rule_absent() {
  local name="$1" project_id="$2"
  local list_json
  if ! list_json="$(gcloud compute firewall-rules list --project="${project_id}" \
      --filter="name=(${name})" --format=json 2>/dev/null)"; then
    return 1
  fi
  [[ "$list_json" == "[]" ]]
}

# hybrid_teardown_delete PROJECT_ID
#
# Deletes the rules hybrid_teardown_check queued in HYBRID_TEARDOWN_DELETE,
# allow before deny. Call only after confirming HYBRID_TEARDOWN_FAILED=
# false, and only once the hub VM is confirmed gone (the caller's
# responsibility -- see docs/deploy/agent-runbook-single-node-vm.md).
# Stops at the first delete that isn't confirmed gone -- via `delete`
# succeeding, or, on a `delete` failure, a positive not-found from
# `_hybrid_firewall_rule_absent` -- and leaves every rule from that point
# on untouched, so the deny is never deleted after the allow delete
# failed or came back uncertain. Sets HYBRID_TEARDOWN_DELETED to the
# rules actually gone afterward and HYBRID_TEARDOWN_DELETE_FAILED to the
# first rule that wasn't (and, transitively, every rule still queued
# behind it). The caller must treat a non-empty
# HYBRID_TEARDOWN_DELETE_FAILED as a failed teardown, and must only
# report HYBRID_TEARDOWN_DELETED as deleted. Never deletes the cluster;
# this tier never creates or deletes a cluster in the first place.
hybrid_teardown_delete() {
  local project_id="$1"
  HYBRID_TEARDOWN_DELETED=()
  HYBRID_TEARDOWN_DELETE_FAILED=()
  local name delete_err stopped=false
  for name in ${HYBRID_TEARDOWN_DELETE[@]+"${HYBRID_TEARDOWN_DELETE[@]}"}; do
    if [[ "$stopped" == "true" ]]; then
      err "Not attempted (kept so tcp:2049 stays denied): ${name}"
      HYBRID_TEARDOWN_DELETE_FAILED+=("${name}")
      continue
    fi
    delete_err="$(mktemp)"
    if gcloud compute firewall-rules delete "${name}" \
        --project="${project_id}" --quiet 2>"${delete_err}"; then
      echo "  Deleted: ${name}"
      HYBRID_TEARDOWN_DELETED+=("${name}")
      rm -f "${delete_err}"
      continue
    fi
    if _hybrid_firewall_rule_absent "${name}" "${project_id}"; then
      warn "Firewall rule ${name} was already gone before this teardown deleted it."
      HYBRID_TEARDOWN_DELETED+=("${name}")
    else
      err "Failed to delete firewall rule ${name}:"
      err "  $(cat "${delete_err}")"
      HYBRID_TEARDOWN_DELETE_FAILED+=("${name}")
      stopped=true
    fi
    rm -f "${delete_err}"
  done
}
