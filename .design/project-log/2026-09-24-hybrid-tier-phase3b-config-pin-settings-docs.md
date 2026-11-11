# Hybrid Deployment Tier — Phase 3b, slice 3, part A: wizard prompts, image pull refusal, image pin, settings.yaml, docs, not-found hardening

Branch `scion/hybrid-tier-p3`, same fork PR as the earlier Phase 3a and 3b slices (`ptone/scion`,
based on `main`). The internal IP guard and the pod CIDR/hub-deny/static internal IP work land in a
later slice of this same PR.

## Overview

Rounds out the hybrid tier's configuration surface and settings.yaml integration, closes the
image-pin question raised for this tier, hardens the not-found detection introduced in the
previous slice against a realistic ambiguous-error shape, and does a full documentation pass for
everything except the hub reachability pieces (pod CIDR, hub-deny rule, static internal IP).

## Wizard prompts

`hybrid_read_config` now prompts for `gke_target.namespace` and `gke_target.pvc_name` alongside
the three existing fields, showing each field's derived default so empty input accepts it --
every `gke_target` key now has a prompt, matching the interactive-wizard convention the rest of
the config already follows. The values it determines (via config or interactively) are exposed
as globals the Kubernetes object-management functions check first, falling back to their own
direct config lookup (computing the same defaults) for callers -- namely `--delete` -- that
never call `hybrid_read_config` at all, since a teardown run must never prompt to enable the
tier.

## Registry-only images when the tier is on

GKE nodes cannot pull from the VM's local Docker image store, which is exactly what
`container_images.source: build` uses. With the tier on, `build` (and any `localhost/` registry)
is now refused right after the hybrid config is read, before any create, with a message naming
the fix: `source: registry` with a registry the cluster's node service account can read.

## Image pin

Traced how the agent's container image is actually resolved for a Docker vs. a GKE agent: both
runtimes resolve through one shared function, with a fixed precedence chain (on-disk
harness-config, then settings harness-config overrides, then the agent/template's own image,
then a CLI `--image` override, then a final registry-prefix rewrite from `image_registry`). A
template's own image setting beats a settings profile's `harness_overrides` pin -- confirming the
tracked-issue's claim -- and the only mechanism that (a) applies identically to both runtimes and
(b) beats every other layer is the CLI `--image` flag at agent start. `deploy.sh` has no
agent-level image flag of its own to set this, and the `image_registry` field it already writes
is the lowest-precedence, last-applied piece (a registry-prefix rewrite on bare image names, not
a name/tag selector) -- so there's no settings write that would actually pin anything more than
it already does. This is documented rather than implemented as a new settings write; the
existing known-limits page already carried the right guidance and needed no correction.

## settings.yaml: shared_dir_storage

Both settings.yaml writes (the initial dev-mode one and the later proxy-mode update) now add a
`server.shared_dir_storage` block when the tier is on, using the schema already defined for it in
the runtime's own settings package: backend `nfs`, with `mount_root` and the one share's `export`
both the VM's own export root (the broker reads it directly as a local path on this same VM,
while GKE pods reach it over NFS at that same server path), `subpath_root` fixed to `projects`,
and the share's `pv_name` the PV this hub's pods bind to. Rendered once by a pure function, right
after the VM's internal IP is read -- moved earlier in the script, since the dev-mode write now
needs it too, not just the Kubernetes objects and the later proxy-mode write that already did --
and spliced into both heredocs with a conditional pattern that produces zero bytes of difference
in the tier-off case, not even a blank line.

## Not-found matching hardened

The three places that treat a lookup failure as "genuinely absent" (the Cloud Run label
decision, the generic Kubernetes object getter, and teardown's cluster-gone check) must not read
a bare "not found" substring in the error text as confirmation. Some permission-denied responses
are deliberately worded to avoid confirming a resource's existence to an unauthorized caller --
"...not found or permission denied" is a realistic shape -- and that must never be read as
"gone." Two shared functions gate every such check: for gcloud, a NOT_FOUND status token, an
HTTP 404 code, or the literal "Requested entity was not found" message; for kubectl, its own
"(NotFound)" reason token. Both exclude outright anything mentioning permission or forbidden,
checked before the not-found signal itself. Fixture tests with realistic permission-masked text
cover all three call sites.

## Documentation

A full pass over the runbook's hybrid-tier section (except the hub reachability pieces, documented in a
later slice): prerequisites up front (existing cluster in-project and on-network, a registry
image source with node-SA read access, the required APIs); the settings.yaml write as the tier's
one of the tier's additive pieces; an explicit statement that base adoption and teardown are unchanged by this
tier, always; the two changes that apply regardless of the tier (base resource markers, the
enable-only-what's-missing API check) called out together; the export's lack of a separate
teardown; the hybrid-tier resources and their conditional-on-marker deletion added to the
Cleanup section; and a pointer to `docs/deploy/hybrid-tier.md`'s existing known-limits section
and manual NFS-tree fix-up recipe rather than duplicating either. That page's own two references
to the dedicated squash identity as future `deploy.sh` work are updated to reflect that this
slice now provisions it for new deployments.

## Tests

Extends the harness with: config-level tests for the namespace/pvc_name default, override, and
validation, and that `hybrid_k8s_ensure_objects` actually uses the value `hybrid_read_config`
determined (not just its own fallback default); wiring tests for the image-source refusal in
both tier states, requiring the tier-on create-mode fixtures to use a registry-based image source
rather than the default `source: build`, since the refusal blocks that default whenever the tier
is on; unit tests for the settings.yaml block's fields and the exact
conditional-splice mechanism, proving the tier-off case is byte-identical; and unit tests for the
tightened not-found matching, both directly (each signal recognized, each permission-masked
message rejected) and through the three call sites' own fixtures.

Verified against a real bash 3.2.57 build: the source=build refusal firing fast (not hanging
until a create-mode timeout) and a registry-sourced tier-on create still reaching VM creation,
alongside the full scenario set carried over from the earlier slices.
