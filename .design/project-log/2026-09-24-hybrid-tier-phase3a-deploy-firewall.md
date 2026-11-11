# Hybrid Deployment Tier — Phase 3a: deploy.sh GKE attach and NFS firewall rules

Branch `scion/hybrid-tier-p3`, based on `main`. Fork PR on `ptone/scion`.

## Overview

`scripts/single-node-vm/deploy.sh` can now optionally attach an existing GKE cluster as a
second, Kubernetes-based runtime alongside the single-node VM, so agents can run in either
place while sharing project scratchpads over the NFS export the hub VM serves. This slice
covers the tier's config, cluster discovery, and the two firewall rules the VM needs to serve
NFS to that cluster's nodes, end to end through creation and teardown. Everything else the
tier will eventually need — the NFS export itself, the squash uid, PV/PVC wiring, and
settings.yaml changes — is out of scope here and follows in a later slice.

The tier is entirely opt-in: a config file (or wizard run) that never sets `gke_target.name`
gets byte-for-byte the same `deploy.sh` behavior as before this change.

## Config

A new optional `gke_target` block (`name`, `location`, `project`) in
`deploy-config.example.json`, read the same way as every other field (`config_get`, with a
wizard prompt when running fully interactively and no config file is given). Each field is
validated against GCP's own naming pattern for that resource type before anything else runs,
and enabling the tier also requires the same Python interpreter (`$PYTHON`) the rest of
`deploy.sh` uses, checked up front rather than failing later inside discovery. The cluster is
always an attach-only, pre-existing prerequisite — `deploy.sh` never creates or deletes one.
This slice supports only a cluster in the hub's own GCP project; a different `gke_target.project`
fails with an actionable message before anything else runs.

## Discovery

Before creating anything, `deploy.sh` confirms the cluster exists and is on the same VPC
network as the hub VM (`default`, today), then discovers the cluster's node network tag with
read-only `gcloud` calls bound to the cluster itself, not to guessing at instance names: it
reads the cluster's node pools' managed instance groups, then the network tags on each group's
instance template — present even when a pool currently has zero running instances, such as an
idle Autopilot pool. Among those tags it selects the one matching the pattern GKE itself uses
for a node's firewall-purpose tag, `gke-<suffix>-node`, which is the same pattern for both a
Standard and an Autopilot cluster. Zero or more than one distinct match refuses to guess and
fails, naming the cluster and listing every tag it found. If any instance group or its template
can't be read at all, discovery also fails outright, even when the readable ones already yield
exactly one candidate: a group it couldn't see could carry a second, different tag, and a
partial view is never treated as a complete one. Every discovery failure surfaces the underlying
`gcloud` error text rather than assuming "not found," since a permission or API error looks
nothing like a missing cluster and needs a different fix. Discovery runs immediately after the required APIs
are enabled, before any of the rest of the deployment's resources (service account, IAM
bindings, Cloud Router, Cloud NAT, the IAP SSH firewall rule) are created, so a bad cluster
name, a network mismatch, or an undiscoverable tag never leaves any of those behind.

## Firewall rules and VM tag

Two firewall rules, both named from the hub and both carrying the exact description token
`scion-deployment=<hub_name>` as an ownership marker, checked client-side as an exact string,
never a substring or prefix match:

- `scion-hub-<hub_name>-nfs-allow` — INGRESS, ALLOW, tcp:2049, source is the discovered node
  tag, target is `scion-hub-<hub_name>-nfs`, priority 900.
- `scion-hub-<hub_name>-nfs-deny` — INGRESS, DENY, tcp:2049, source `0.0.0.0/0`, same target
  tag, priority 950.

The deny rule is created first, then the allow rule, so an interrupted run can never leave the
allow rule in place without its paired deny. The hub VM gets the `scion-hub-<hub_name>-nfs`
target tag at creation time when the tier is enabled, or via an idempotent `add-tags` call on an
existing VM. If a rule already exists under one of these names without the exact marker,
`deploy.sh` refuses to touch it rather than adopting it — this applies only to these two rules;
the VM, Cloud Run proxy, router, NAT, and service account keep today's unmarked adopt-by-name
behavior unchanged. Re-running the script against a hub that predates the hybrid tier works: the
existing base resources are adopted as always, and the two firewall rules (plus the VM tag) are
created fresh, with markers.

A pre-existing, already-marked rule is also checked against the tier's full security-relevant
spec before being reused: direction, action, every allow/deny entry (not just the first),
source tags and source ranges compared as separate fields (each one not expected to be set must
be empty, since GCP ORs the two when both are present), source and target service accounts,
destination ranges, whether the rule is disabled, target tags, priority, and network. Any
mismatch fails the run, lists exactly which fields differ, and prints the commands to fix it —
always a delete (so the next `deploy.sh` run recreates the rule correctly), plus a direct
`update` command, but only when applying it would converge to exactly the expected rule (every
other field already matches, and nothing needs to be cleared) rather than leaving some other
drifted field in place. When the drifted rule is the deny rule and only a delete is possible, the
remediation is the full order-preserving sequence — delete the allow rule, delete the deny rule,
re-run `deploy.sh` — never advice that would leave the allow rule in place with no deny, even
temporarily. Nothing is ever auto-corrected: a hand-edited rule under this deployment's marker is
exactly what an operator should see reported back, since the rule's meaning comes from those
fields.

## Teardown (`--delete`)

`--delete` now validates `hub_name` against the same pattern the create path already enforces,
since the value reaches a `firewall-rules list --filter` expression. The existing static
deletion-list block gains one more check, run before anything is printed or
deleted, and factored into its own function (`hybrid_teardown_preflight`) so it has direct test
coverage of its own: a single `firewall-rules list` call (rather than one `describe` per rule)
looks up both hybrid firewall rules by name and classifies each as **found, marked** (queued for
deletion, printed as `found (marked): <name>`) or **found, unmarked** (printed as `SKIPPED
(unmarked): <name>`); a name not present at all is simply not queued. A non-zero exit from the
list call itself is treated as a failure, not as "no rules exist" — an unknown ownership state
must never look identical to nothing needing protection. When nothing matched at all, the check
never even needs a JSON parser, so it doesn't require `$PYTHON`; when something did match, it
does, and a missing interpreter is reported as its own clear error rather than being misdiagnosed
as an unmarked-rule collision. Either an unmarked name match or a failed list call fails the whole
teardown run before anything is deleted, not just the two hybrid rules — a naming collision, or
an inability to even check, means the hub name can no longer be trusted to identify only
resources this deployment owns, so nothing else proceeds safely from that point either.

With no hybrid rules queued for deletion, a VM delete failure warns and continues exactly as it
always has, with no change to the exit code. With hybrid rules queued, "the VM is gone" is a
positive, project-wide answer (an `instances list` by name, not scoped to any particular zone)
rather than an inference from a `describe` or `delete` call that merely failed, which could just
as easily mean a guessed-wrong zone or a transient error as an actually-absent VM; anything short
of a positive answer keeps both rules and fails the run, reporting what's known. That list call
also forces a partial result to be a hard error rather than a silent success: without `--zones`
it's a project-wide AggregatedList, and the SDK's default behavior downgrades an unreachable zone
to a warning and an empty-but-successful result, which would otherwise misread the exact zone
outage that could have caused the VM delete to fail in the first place as "the VM is gone".
Deletion order is the reverse of creation — the allow rule first, then the deny rule — and
deletion stops at the first rule that isn't confirmed gone (a failed `delete` re-checked with a
fresh `list`, so only a positive not-found counts), so the deny rule can never be deleted after
the allow rule's own delete failed or came back uncertain; every rule left queued behind a stopped
delete is printed as not attempted, not just silently omitted from the summary. A rule whose
delete call fails is reported as a failure, and the final summary only lists rules actually
confirmed gone, never one that failed to delete. The cluster itself is never deleted, under any
circumstance.

## Tests

`scripts/single-node-vm/hybrid-tier.sh` is a self-contained module (functions:
`hybrid_read_config`, `hybrid_discover`, `hybrid_ensure_firewall_rules`, `hybrid_vm_tag`,
`hybrid_apply_vm_tag`, `hybrid_teardown_check`, `hybrid_teardown_preflight`,
`hybrid_teardown_delete`), sourced by `deploy.sh` but independently testable.
`scripts/single-node-vm/tests/run.sh` runs two layers: function-level tests against the
hybrid-tier.sh functions directly, and wiring tests that run `deploy.sh` itself as a real
subprocess against the same stub, to cover the parts of the integration a function-level test
can't reach (whether `deploy.sh` actually gates discovery, `--tags`, and `add-tags` on the tier
being enabled, and the exact ordering of its teardown calls). Both layers use a stubbed `gcloud`
on `PATH` that records every invocation and serves fixtures — no test ever contacts GCP. Each
test runs in its own subshell so one unexpected early exit can't end the rest of the suite. A
create-mode `deploy.sh` subprocess only needs to run through its VM-exists check, not a full
simulated deploy (which would have to get past an SSH-readiness retry loop with real sleeps), so
those tests run it in the background and stop it as soon as the stub signals that check happened,
rather than waiting out a fixed timeout.

Coverage includes: rule names, the marker on every created rule (including that the marker
check is an exact match, not a substring or prefix, so a prefix-colliding hub name or a marker
with trailing text can't be mistaken for a match), refusal on an existing unmarked rule (even
one whose spec otherwise fully matches, proving the run stops at the ownership check rather than
happening to also fail a later one), the target tag and `--project` on every created rule, the
allow/deny priorities/ports/source-tag/deny-all shape and creation order, discovery for both a
Standard and an Autopilot node-pool/instance-group/instance-template fixture shape (including a
pair of prefix-colliding cluster names, a multi-tag template, a tag that only contains the
expected pattern as a substring, a zero-instance pool, one unreadable instance group alongside a
readable one, all instance groups unreadable, and the zero- and multiple-candidate refusals),
the network-mismatch refusal, the pre-existing non-hybrid-hub re-run case at both the function
and the `deploy.sh` level, an idempotent create-then-reuse round trip, and at least one drift
test for every field the spec check compares (direction, action, every allow/deny entry,
disabled, priority, network, source tags, source ranges, source and target service accounts, and
destination ranges, on both the allow and the deny rule where the two sides differ), each
asserting the field is named, the delete remediation is always present, and the update
remediation is present only where it would converge — including the deny-specific
allow-then-deny-then-re-run sequence when only a delete is possible. Teardown coverage includes
its found/SKIPPED/fail-the-run classification, needing `$PYTHON` only when something matched,
and its ordering relative to `deploy.sh`'s deletes: the ownership check before any delete, the
hybrid rules only after the VM is positively confirmed gone (not merely inferred from a failed
call), deletion stopping at the first rule that isn't confirmed gone so the deny is never deleted
after the allow's delete failed, a VM delete failure keeping both rules only when hybrid rules are
queued (a tier-off VM delete failure still warns and continues unchanged), and a delete failure
reported as a failure rather than "deleted." The tier-off case is covered at both levels for both
a fresh deploy and a redeploy against an already-existing VM: no `gke_target`, no cluster calls,
no `add-tags`, no hybrid firewall-rule calls, and (for a fresh VM) an unchanged `instances create`
invocation. Re-running teardown after the VM is already gone -- the recovery path deploy.sh's own
"Keeping..." message points an operator to -- is covered directly, as is an unreachable zone
during the VM-gone check never being misread as "gone." The interactive path (no `--config`,
answering prompts on stdin) is also covered at the `deploy.sh` level for the no-Python,
no-hybrid-rules case, alongside the existing function-level coverage.

Both `deploy.sh` and `hybrid-tier.sh` use `${arr[@]+"${arr[@]}"}` rather than a bare
`"${arr[@]}"` for every array that can legitimately be empty, since the bare form is an
unbound-variable error under `set -u` on bash older than 4.4 (macOS ships 3.2) even though it is
a no-op on newer bash. This was verified against a real bash 3.2.57, built from source and
checksum-verified against the recipe already recorded for this repository's macOS compatibility
job: the tier-off create path, the array-guard's most exposed use, reaches VM creation without
error, and `--delete` runs cleanly through all three of the tier-off/no-rules, marked-rules, and
unmarked-collision cases.

No CI workflow currently runs anything under `scripts/single-node-vm/`; the repository's
blanket `shellcheck` job lints any `*.sh` file with no path filter, so the new scripts are
covered by that but not by any dedicated test-execution job. Every new or changed script here
(including `tests/lib/gcloud`, which has no `.sh` extension) is shellcheck-clean.

## Docs

A new "Hybrid Tier (Optional)" section in `docs/deploy/agent-runbook-single-node-vm.md`
documents the config fields, the discovery method, the firewall rules and VM tag, and the
teardown behavior, and states plainly that base-resource adoption and teardown are unchanged by
this tier. The full docs pass, including `docs/deploy/single-node-vm.md`, is a later slice.

## Verifying this locally

1. `bash -n scripts/single-node-vm/deploy.sh scripts/single-node-vm/hybrid-tier.sh`
2. `scripts/single-node-vm/tests/run.sh`
3. Manually: run `deploy.sh --config <file>` with a `gke_target` block naming a real cluster in
   the same project as the hub, and confirm the two firewall rules and the VM tag appear; then
   `deploy.sh --delete` and confirm both rules are deleted and reported.
