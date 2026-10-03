# Project Log: Substrate egress trust boundary, secret-path containment, and broker diagnosability

**Date:** 2026-09-30

## Overview

Closes a gap where an actor's egress allowlist could be widened by an
agent- or template-controlled hub endpoint, adds path containment for
bootstrap file-secret targets in enforced mode, fixes a broker
diagnosability regression where runtime-op failures reached neither logs
nor telemetry on several handlers, adds bounded read/idle timeouts to the
in-actor control server, removes orphaned root-owned-file helpers with no
production caller, and closes a darwin build gap in test helpers that read
a stat structure's platform-specific ctime field.

## Egress allowlist trust boundary

`substrateEgressHostnames` (`pkg/runtime/substrate_egress.go`) now
separates every host it can add into two explicit trust classes:

- Operator/broker-config: `RunConfig.TrustedHubEndpoint` (new field) and a
  fixed set of hardcoded harness-model hosts. Neither passes through the
  public-hostname validator, since an in-cluster hub endpoint
  (`*.svc.cluster.local`) is a legitimate deployment shape that validator
  would otherwise reject.
- Tenant-controllable: the git-clone remote and any OTEL exporter endpoint
  an agent or template config can set. Both go through the same hostname
  validator operator-configured allow entries use, which rejects IP
  literals, CIDR ranges, and cluster-internal suffixes.

`RunConfig.TrustedHubEndpoint` is captured in `pkg/agent/run.go` from the
broker-resolved hub endpoint value in effect immediately before an
agent/template config's own environment override can change it. The
actor's egress allowlist is built from this captured value, never from the
final (possibly overridden) environment map — so an agent- or
template-supplied hub endpoint override still changes which endpoint the
agent itself calls, but never widens which host the actor's sandboxed
egress path may reach. A mismatch between the two is logged (hostnames
only) rather than silently allowed. `OPERATIONS.md`'s known-limits section
now states this behavior explicitly.

## Bootstrap file-secret containment

`writeBootstrapFile` (`pkg/sciontool/substrate`) refuses a file-secret
target whose resolved path falls outside the agent's home directory when
running in enforced mode, via a new component-wise containment check.
Non-enforced mode is unaffected.

## Broker runtime-op diagnosability

`pkg/runtimebroker` now routes every runtime-op handler's failure response
through one helper that always logs the underlying error and records it on
the request's span before writing the client a fixed, identity-free
message — closing several handlers that previously wrote only the fixed
message with no corresponding log line or span status. A curated
config/template error assembled inside `buildStartContext` reaches the
client with its own actionable message on all three of its callers now,
not just one of them as before, while an error whose message happened to
embed raw internal error text is rewritten to a fixed string first so that
widening does not turn into a new disclosure path; a template/harness-config
hydration failure's own error text remains visible only through the one
caller that has always shown it.

## Control-server timeouts

The in-actor control server (`sciontool substrate-serve`) now bounds how
long it holds a connection open waiting on a slow or stalled client
(header, body, and idle timeouts), without bounding how long it may take to
write a response — a command executed through the exec endpoint can
legitimately run for several minutes before any response is written.

## Actor-template reuse

Confirmed the actor-template name's content hash already covers both the
image digest and the full container security context (a single
capabilities list), and added the test that proves the image half of that,
which the existing stability test never exercised.

## Dead-code removal and darwin build fix

Two root-owned-directory helpers and a package-level private-temp-directory
constant with no caller anywhere in the stack are removed; a third helper
in the same family is relocated to the package that actually calls it. A
platform-specific stat field access in several test helpers is now routed
through a small per-platform accessor so the affected test files compile
on darwin as well as linux.
