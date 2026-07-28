# Phase 3 — Route `gh://` skill resolution through the Hub

**Branch:** `scion/ps-cache-p3-dev` (based on `scion/ps-cache-em-b`, which carries Phase 1 + Phase 2)
**Date:** 2026-07-28

## Goal

Phase 1 gave the broker a singleton in-process resolution cache; Phase 2 gave the Hub a
DB-backed `gh://` resolver. Phase 3 flips the routing so `gh://` skill URIs are resolved by
the Hub (durable cache + GitHub App token minting) instead of by the broker-local GitHub
resolver, while keeping the local resolver as a backstop so a Hub outage cannot break skill
provisioning.

## What changed

### `pkg/agent/routing_skill_resolver.go`

- Added a `fallbackResolvers map[string]SkillResolver` field to `RoutingSkillResolver`
  (scheme → resolver used only when the Hub path fails).
- Added `RegisterFallback(scheme, resolver)`. Registering a scheme here has two effects:
  1. URIs with that scheme are routed to the primary fallback (Hub) *first*, so Hub-side
     caching applies;
  2. the registered resolver becomes the backstop if Hub fails.
  It panics on an empty scheme or a duplicate registration, mirroring `Register`.
- Reworked resolver selection in `Resolve()`. For each scheme group:
  - an explicitly `Register`ed resolver still wins (unchanged behaviour);
  - otherwise, if the scheme has a fallback resolver, Hub becomes the primary and the
    registered resolver becomes the backstop;
  - if no Hub is configured at all, the fallback resolver is used directly as primary
    (avoids a nil-Hub regression for brokers without a Hub connection);
  - otherwise the pre-existing `skill`/bare-name behaviour applies.
- Added two failure paths:
  - **Transport error** — Hub returns a hard error: log at `warn`, retry the *entire* scheme
    group against the fallback. If the fallback also hard-errors, the error is surfaced as
    `fallback resolver for scheme %q failed: ...`.
  - **Per-URI errors** — Hub returns `ResolveError`s: log at `info`, retry only the affected
    URIs against the fallback (`retryErrorsWithFallback`). Retried errors are dropped and
    replaced by the fallback's resolved skills / errors; errors for URIs the fallback was not
    asked about are preserved. If the fallback hard-errors during retry, Hub's original
    errors are kept unchanged.
- Added `resolverNameOf()` for nil-safe log field values. Note `SkillResolver` does **not**
  declare `ResolverName()`, so this uses an interface type assertion and falls back to `%T`.

Logging uses `log/slog`, consistent with the rest of `pkg/agent`.

### `pkg/runtimebroker/handlers.go`

Single-line routing change (plus a comment):

```go
- router.Register("gh", ghResolver)
+ router.RegisterFallback("gh", ghResolver)
```

`hub_skill_resolver.go` was intentionally left untouched — it already forwards
`opts.ProjectID`, which the Hub needs for GitHub App token minting.

## Tests added — `pkg/agent/routing_skill_resolver_test.go`

Nine new cases, all using the existing `mockSchemeResolver`:

| Test | Covers |
| --- | --- |
| `RegisterFallback_HubSuccess` | `gh://` goes to Hub; local resolver never called |
| `RegisterFallback_HubTransportError` | Hub hard error → all group refs retried locally |
| `RegisterFallback_BothFail` | Hub *and* fallback hard error → wrapped error surfaced |
| `RegisterFallback_HubPerURIError` | only the errored URI retried; results merged, error cleared |
| `RegisterFallback_FallbackAlsoErrors` | fallback's per-URI error replaces Hub's |
| `RegisterFallback_NoHubUsesFallbackDirectly` | nil Hub → fallback used as primary |
| `RegisterFallback_PrimaryTakesPrecedence` | `Register` beats `RegisterFallback` for a scheme |
| `RegisterFallback_MixedBatch` | `skill://` + `gh://` both reach Hub in separate calls |
| `RegisterFallbackPanics` | empty scheme and duplicate scheme both panic |

## Build & test results

| Check | Result |
| --- | --- |
| `gofmt -l pkg/agent pkg/runtimebroker` | clean |
| `go build ./...` | pass |
| `go vet ./pkg/agent/... ./pkg/runtimebroker/...` | pass |
| `go test ./pkg/agent/ -run 'TestRoutingSkillResolver\|TestDetectScheme'` | 20/20 pass |
| `go test ./pkg/runtimebroker/...` | pass |
| `go test ./pkg/agent/...` | one **pre-existing** failure (see below) |

### Pre-existing failure (not caused by Phase 3)

`TestProvisionWritesTaskToPromptMd` (`with_task` and `without_task`) fails on the base
commit `a42cb8ed` as well — verified by stashing the Phase 3 diff and re-running:

```
provision_test.go:252: Provision failed: stage project pre-start hook:
  remove stale pre-start hook: .../hooks/pre-start.d/30-project-custom: not a directory
```

This comes from the pre-start hook staging work merged in #888, not from skill routing.
Flagging it for the EM — it likely needs a separate fix on `main`.

## Notes for review

- The workspace was originally branched from `main`, which does **not** contain Phase 1 or
  Phase 2. The branch was reset onto `origin/scion/ps-cache-em-b` before starting so Phase 3
  builds on the correct base. `origin/main` is an ancestor of that branch (11 commits behind).
- Not pushed — the EM will push after code review.
