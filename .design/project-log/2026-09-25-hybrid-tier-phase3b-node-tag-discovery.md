# Hybrid Deployment Tier — Phase 3b: node-tag discovery

Branch `scion/hybrid-tier-p3`, same fork PR as the earlier Phase 3a and 3b slices.

## Overview

`hybrid_discover`'s node-tag lookup reads the tag directly off the cluster's own GKE-managed
firewall rules, because Autopilot clusters do not expose node instance groups, templates or
instances as Compute resources in the project at all -- a managed-instance-group describe call
returns not-found even for a cluster with running nodes -- despite Autopilot being a documented,
supported attach target. This source exists identically for both cluster types.

## Discovery

From the cluster's own describe call: its network, and its pod CIDR (already read for the
hub-deny rule). A `firewall-rules list` with no filter returns every rule in the project; the
code keeps only the rules whose network URL ends in exactly `/networks/<network>`, and among
them finds the candidate rule: name matching `gke-<suffix>-all`, direction INGRESS, source
ranges including the cluster's pod CIDR (pod CIDRs
are unique within a VPC, which is what ties the rule to this specific cluster without needing to
reconstruct GKE's own name-truncation and hashing scheme). That rule must carry exactly one target
tag, matching `gke-<suffix>-node`, and the matching `gke-<suffix>-vms` rule must exist with the
same single target tag. No rule, more than one, the wrong tag shape, or the two rules disagreeing
all refuse to guess and fail, listing what was found and naming the cluster's own firewall rules
(not this script) as where to fix it. Nothing is created before this check passes.

Unchanged: the NFS allow rule still sources from the discovered tag (kubelet mounts from the
node's own primary IP, so a tag-based source is still correct for it); the hub-deny rule keeps
its pod-CIDR source range; the NFS export's client list stays the node subnet.

## Tests

The Autopilot fixture now models the real shape: a managed-instance-group describe call is set up
to fail as it would in reality, and the test asserts that call is never made at all, since
discovery reads the node tag from firewall rules instead. Standard clusters exercise the identical
path. Coverage added
for every way the firewall-rule check can fail: no candidate rule, the pod CIDR matching no rule,
a candidate whose direction isn't INGRESS, more than one candidate, a candidate with more than one
target tag, the `-all` and `-vms` rules disagreeing, a missing `-vms` rule, and the name/tag
patterns' own anchoring. The stub's `firewall-rules list` handler gained a network-scoped fixture
pool alongside its existing name-keyed one, since this call needs to see every rule on a network,
not a fixed set of names.

## Docs

The runbook's discovery section and its "Standard or Autopilot" claim now describe the firewall-
rule-based mechanism, which is what makes that claim actually true.
