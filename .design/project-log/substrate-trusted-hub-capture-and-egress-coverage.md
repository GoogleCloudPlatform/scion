# Project Log: Trusted hub endpoint capture point, tenant egress coverage, and broker error diagnosability

**Date:** 2026-09-30

## Overview

Moves `RunConfig.TrustedHubEndpoint`'s capture point in `pkg/agent/run.go`
to the very top of `Start`, before any agent-level or template-level
override can reach `opts.Env`; restricts the broker-side value that feeds
that capture to operator-derived sources only; requires every tenant-derived
egress host (git-clone remote, OTEL endpoints) to be covered by an
operator's own `egress_allow` entry before it is added to an actor's
`EgressPolicy`; and fixes a broker code path where a `*startContextError`'s
underlying cause never reached the server's own log or span, only its
curated client-facing message.

## Trusted hub endpoint capture point

`pkg/agent/run.go`'s `Start` now reads `opts.Env["SCION_HUB_ENDPOINT"]`
into `callerHubEndpoint` as its first action, before project-settings
resolution, before any inline `req.Config.Hub` override, and before a
template's own environment can set the same key. Outside broker mode,
`RunConfig.TrustedHubEndpoint` is `callerHubEndpoint`, falling back to the
project's configured hub endpoint only when the caller supplied none. In
broker mode, it is `opts.TrustedHubEndpoint` instead — see the next section
for what feeds that field. An inline agent-level Hub config and a
template's own environment value are both applied to the final process
environment (so the agent still calls the endpoint it was configured to
call) but never reach `TrustedHubEndpoint`, so neither can widen the
actor's egress allowlist. With no operator-derived hub endpoint available,
no hub host is added to egress at all, and a warning names the gap.
`substrateEgressHostnames` and the surrounding comments in
`pkg/runtime/interface.go`, `pkg/runtime/substrate_egress.go`, and
`run.go` itself describe this capture point and its guarantees directly,
rather than by reference to any prior implementation.

## Egress trust restricted to operator-derived resolution tiers

`api.StartOptions` adds `TrustedHubEndpoint`, populated in
`runtimebroker.buildStartContext` from `resolveEffectiveHubEndpoint`'s own
trust bit: true only when the resolved hub endpoint came from the request's
`HubEndpoint` field, the hub connection endpoint, or this broker's own
configured `HubEndpoint` — every one of these an operator-controlled
source. It is false when the endpoint instead came from `ResolvedEnv` (the
hub-resolved `AppliedConfig.Env`, which a project or template creator
controls) or from project settings (itself hub-resolved for a hub-managed
project, the same tenant-reachable precondition the resolved-env tier is
excluded for), even though either value is still delivered into the
agent's own `SCION_HUB_ENDPOINT`/`SCION_HUB_URL` env unchanged.
`pkg/agent/run.go` reads `opts.TrustedHubEndpoint` for its broker-mode
egress trust instead of re-reading `opts.Env`, so a creator-controlled
value can never reach Substrate's egress allowlist even in the degenerate
configuration where every operator tier is empty — that case now resolves
to no trusted hub host at all, the same fail-closed behavior an empty
caller-supplied value already had.

## Tenant-derived egress host coverage

`pkg/runtime/substrate/egress.go` adds `EgressAllowCovers`, which checks a
normalized host against a list of normalized `egress_allow` entries using
the same matching an operator's own entries use: an exact match, or a
single-level wildcard (`*.example.com` covers `api.example.com`, not
`example.com` or `a.b.example.com`) — the same scope TLS wildcard
certificates use.

`pkg/runtime/substrate_egress.go`'s `addTenantHost` now drops a
well-formed, public tenant-derived host (git-clone URL, `SCION_OTEL_ENDPOINT`,
`OTEL_EXPORTER_OTLP_ENDPOINT`) unless the operator's own `egress_allow` list
covers it, logging the dropped host and how to allow it. Passing the
public-hostname grammar validator is necessary but insufficient on its own: an
attacker-controlled but syntactically valid public hostname (e.g. an
externally-routable `*.nip.io` name resolving to an internal address) is
refused unless an operator has explicitly opted a covering pattern into
`egress_allow`. Hardcoded model-host allowlisting and operator-configured
`egress_allow` entries themselves are unaffected — this check applies only
to the tenant-controllable sources. `deploy/substrate/README.md`,
`deploy/substrate/settings.example.yaml`, `deploy/substrate/OPERATIONS.md`,
and `.design/kubernetes/substrate-runtime.md` describe the coverage
requirement.

## Bootstrap file-secret containment is documented

`deploy/substrate/README.md`'s bootstrap-files section, alongside its
existing note on symlink-traversal refusal, now also states that a
file-secret or auth target must resolve under the agent home directory: an
absolute path outside it, or a `..`-relative escape, is refused with HTTP
422 (`bootstrap_path_outside_home`) before anything is written.

## Broker error diagnosability

`writeStartContextErrorOrOp` now logs and records `OriginalErr` (falling
back to the `*startContextError` itself when unset) rather than the
error's own `Error()` text, which returns only the curated `Message` a
`*startContextError` was constructed with — so a genuinely underlying
cause (e.g. an `os.UserHomeDir` or hub-endpoint-resolution failure) reaches
the server's own log and span even when the client-facing message is a
fixed, generic string. `createAgent`'s own `*startContextError` branch
routes through the same helper.

## Verification

`go build ./...`, `go vet ./...`, and `go test` across every package under
`cmd/`, `pkg/`, and `internal/` pass on linux/amd64 (native); `go build` and
`go vet` under `GOOS=darwin` for both `arm64` and `amd64` pass for every
package except `pkg/hub`, which has a pre-existing, unrelated
`unix.Renameat2` darwin build gap present at this stack's base commit.
`pkg/hub`'s own test suite passes under `-timeout 30m` (native linux/amd64);
it takes roughly 25 minutes to complete and is not otherwise affected by
this change. `internal/fixturegen`'s `TestFixtureCoverage` fails identically
at this stack's base commit, unrelated to this change.
