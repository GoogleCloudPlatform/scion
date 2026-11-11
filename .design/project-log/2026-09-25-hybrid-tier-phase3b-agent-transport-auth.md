# Hybrid Deployment Tier — Phase 3b: agent transport auth

Branch `scion/hybrid-tier-p3`, same fork PR as the earlier Phase 3a and 3b slices.

## Overview

GKE agent pods reach the hub through its existing public IAP URL — the same URL browser users
use — authenticating with a Google OIDC ID token the hub mints by impersonating a dedicated
transport service account. There is no private-IP hub endpoint: hub-deny (`INGRESS DENY` on all
protocols from the discovered pod CIDR, priority 950, the same scheme as `nfs-deny`) makes
explicit that the cluster's default pod range has no direct path to the hub VM. The static internal IP
reservation exists solely for the shared NFS PV's server field.

## Cloud Run egress / hub-deny overlap

Before hub-deny (or either of the other two hybrid firewall rules) is created, a check reads the
Cloud Run IAP proxy's own egress subnet (`gcloud compute networks subnets describe`, the same
"default" subnet its direct-VPC-egress configuration already hardcodes) and refuses to continue if
that subnet's primary range could overlap the discovered pod CIDR — hub-deny would otherwise also
deny the proxy's own traffic to the hub, breaking every deploy regardless of whether the hybrid
tier is in use. A describe failure fails the same way; an unknown range is never treated as safe.

## Agent transport auth

A dedicated transport service account, `scion-tp-<first 12 characters of the hub name>-<8-hex
checksum of the full hub name>`, is created or adopted (marked in its description — service accounts have no labels — the same convention as every other hybrid-tier
resource; a same-name SA without the marker is refused, never adopted). Setup:

1. The project's IAP OAuth client ID is read (`gcloud iap settings get --resource-type=iap_web`)
   and used verbatim as the transport token's audience. An API error or an empty client ID fails
   closed.
2. The hub's own runtime service account is granted `roles/iam.serviceAccountOpenIdTokenCreator`
   on the transport SA specifically, not project-wide, and not the broader
   `serviceAccountTokenCreator`, which this never needs: `pkg/hub/transport_token.go`'s
   `gcpTransportMinter` calls the IAM Credentials API's `GenerateIdToken` directly
   (`transport_token.go:106`), and that call needs only the ID-token permission this role grants.
3. Once the Cloud Run proxy exists and has IAP enabled (Phase 4), the transport SA is granted
   `roles/iap.httpsResourceAccessor` on it — the same call and role the human operator's own access
   already uses.
4. `server.auth.transport` (`mode: iap`, `oidc_audience`, `platform_auth_sa`) is written into both
   the dev-mode and proxy-mode `settings.yaml` writes, gated on the hybrid tier: only
   GKE-dispatched agents leave the hub VM to reach it over IAP, so only they need a transport
   token.

IAM changes here can take on the order of a minute to propagate. `deploy.sh` itself never mints or
uses a transport token, so nothing in it blocks on that propagation; the very first agent dispatch
right after a deploy may see a transient authentication failure that a retry resolves, and this is
documented in the runbook rather than covered with a blind sleep.

Teardown removes the transport SA's Cloud Run IAP binding (skipped when the Cloud Run service
has already been deleted, which removes it) and then the SA itself, marked only, never touching an
unmarked same-name SA — the same discipline as every other hybrid-tier resource. Only a positive
not-found counts as absent; an unreadable state, a failed binding removal or a failed delete keeps
the SA, is listed in the teardown summary, and makes teardown exit non-zero.

## Docs

`docs/deploy/agent-runbook-single-node-vm.md` describes hub-deny and the Cloud-Run-egress overlap
check under the firewall-rule bullet and has a dedicated step for agent transport auth; its
resource table lists hub-deny and the transport service account. `docs/deploy/hybrid-tier.md`
notes that GKE agents reach the hub through its IAP URL and points to the runbook.

## Tests

Function-level coverage in `test_hybrid_tier.sh`: hub-deny's creation shape and every field of its
drift check; the overlap check (an overlapping range, a subnet-describe failure, and a disjoint
range); a check that no resource, variable, or function named for the private-IP path this tier
doesn't have exists under `scripts/single-node-vm`, `docs` or `.design/project-log`; and the
transport-auth functions (client-ID discovery, the transport SA's name and
create/adopt/refuse-unmarked behavior, the token-creator grant's role and member, the Cloud Run
accessor grant's resource type/service/role/member, the rendered `settings.yaml` block, and
teardown's marked/unmarked/absent/failure cases). `test_deploy_wiring.sh` covers the firewall
rules' teardown delete ordering and the internal-IP guard's post-create re-describe call.
