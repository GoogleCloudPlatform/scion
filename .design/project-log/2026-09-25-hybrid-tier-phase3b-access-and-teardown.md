# Hybrid Deployment Tier — Phase 3b: hub-deny scope, transport SA id, restricted user access

Branch `scion/hybrid-tier-p3`, the same fork PR as Phase 3a and the other Phase 3b entries.

## Overview

hub-deny denies all protocols and ports from the pod CIDR; the transport service account id is a
hub-name prefix plus a hash; transport SA create and teardown treat only a positive not-found as
absent; the settings the tier writes include restricted user access; and tests parse both
settings.yaml writes as YAML and cover `deploy.sh`'s transport wiring end to end against the stubs.

## hub-deny on all protocols

`scion-hub-<hub>-hub-deny` denies all protocols and ports (`--rules=all`) from the discovered
pod CIDR to the hub VM's tag, priority 950; the drift check expects `all`. Nothing legitimate in
the tier needs traffic from a pod address to the hub VM: GKE agents reach the hub through its IAP
URL; the Cloud Run proxy's egress comes from the default subnet's primary range, which the
existing overlap check keeps disjoint from the pod CIDR; and NFS mounts come from the kubelet on
the node's primary address, which `nfs-allow` (source tag, priority 900) admits before either deny
rule. Firewall source tags match only a VM's primary address, so `nfs-allow` does not cover pod
addresses. Only the default pod range is covered; additional pod ranges (node-pool or
cluster-level) and pod traffic that leaves with the node's address are not, and the runbook says
so, with the remedy. Additional pod ranges are not detected: ranges added after a deploy would
still be uncovered until the next one, and the remedy is a single equivalent rule per range.

## Transport service account id

`hybrid_transport_sa_name` returns `scion-tp-<prefix>-<hash>`: the first 12 characters of the hub
name with trailing hyphens trimmed, then the 8-hex-digit POSIX `cksum` of the full hub name. The
id is 19 to 30 characters, stable across runs and machines (`cksum` output is fixed by POSIX, so
teardown needs no Python), distinct for hubs whose names share a long prefix, and never equal to
a hub's `scion-hub-*` base id. A hash collision would surface as the existing refusal to adopt a
same-name SA without this hub's marker.

## Transport SA create and teardown

Create refuses on a describe error other than not-found; only a positive not-found reads as absent.
Teardown takes a fifth argument, whether the Cloud Run service is gone. It removes the SA's IAP
access binding first unless the service is gone, treats an already-absent binding as fine, and
keeps the SA if the removal fails. Any describe error other than not-found keeps the SA as
"state unknown". Every kept case sets `HYBRID_TRANSPORT_SA_DELETE_FAILED`, which makes `--delete`
exit non-zero, and the teardown summary lists the SA (`Deleted`, `Not found`, or `Kept` with the
reason) and its IAP access binding. The k8s-failure early exit happens before this step, so no
transport SA line is printed in that case.

## Restricted user access

With the tier on, both settings.yaml writes carry `server.auth.user_access_mode` (`invite_only`
by default, or `domain_restricted` from the config file) and any configured
`server.auth.authorized_domains`, both new optional top-level config keys.
`hybrid_resolve_user_access` runs before anything is created and refuses an empty or
service-account `admin_email`, any other mode including `open` and empty, `domain_restricted`
without domains, and domain entries that are malformed or match `gserviceaccount.com` addresses
(including wildcards such as `*.com`, since the hub matches `*.suffix` entries by plain suffix).
The admin account signs in whatever the mode (the hub checks `admin_emails` first) and invites
other users from the web UI or with `scion hub invite create`. With the tier off, the keys are
ignored with a warning and both writes are byte-identical to a render
without the user access splice. The runbook gains two
tier-on checks: the admin can sign in, and a request through IAP carrying only a transport ID
token is refused.

## Tests

- `tests/lib/settings-yaml-to-json.go` (`//go:build ignore`, run with `go run` from the repository
  root) parses a settings document with `github.com/knadh/koanf/parsers/yaml`, the parser the hub
  loads settings with. The wiring tests parse both captured writes, tier on and off, and assert
  `server.auth.transport.{mode,oidc_audience,platform_auth_sa}`, `user_access_mode`,
  `authorized_domains`, `admin_emails`, `shared_dir_storage` and `listen_port`. PyYAML was not
  used because it is not installed in this environment, while the Go module already carries the
  hub's own parser.
- Wiring tests run a tier-on create through Phase 5 and assert the OpenIdTokenCreator binding on
  the transport SA (hub runtime SA as member), the Phase 4 IAP accessor grant for the transport
  SA, the client ID written verbatim, no transport calls with the tier off, deploy-level user
  access refusals and domains, and transport SA teardown exit status and summary lines.
- Unit tests cover the SA id, create and teardown on describe errors, binding removal and its
  failure, both overlap shapes plus an empty or unparsable subnet range, and every user access
  refusal. The `iap settings get` stub requires `--resource-type=iap_web`. The absence check scans
  extensionless files and `.design/project-log`, case-insensitively, for every spelling.
- Suite: 1182 assertions across 437 tests at the time of this entry, under bash 5 and bash 3.2.
