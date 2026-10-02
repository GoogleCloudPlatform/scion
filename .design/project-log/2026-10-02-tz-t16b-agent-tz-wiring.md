# tz-refactor task 16 (P2a-1), part (b): agent TZ wiring, writers and the PATCH response

**Date:** 2026-10-02
**Branch:** `scion/tz-t16b`, stacked on `scion/tz-t16a` (part (a): fields and resolver)
**Fork issue:** ptone/scion#2509 (part of ptone/scion#2457, design Option A)

## What changed

- **Callers.** `buildCreateRequest` and `buildStartEnv` set `TZ` only from
  `resolveAgentTZ` (`setResolvedAgentTZ`). Create, start and restart all run
  the chain, so a hub default edit reaches unpinned agents at their next
  start.
- **Writers of `ExplicitTimezone`** (design §3 A writer table):
  - (a) create: `captureCreateTimezone` runs last in `resolveDerivedConfig`
    (request env > template env > harness config env, then the env copies are
    stripped). `CreateInputs` keeps the requested `TZ` so reincarnate can
    replay it.
  - (b) the agent PATCH's top-level `explicitTimezone`, in any phase (`""`
    unpins and records the unpin tombstone).
  - (c) `adoptLegacyTZ`, run lazily by `buildCreateRequest`, `buildStartEnv`,
    the PATCH (before the `config.env` strip), the agent GET (in memory only)
    and reincarnate (before `legacyCreateInputsFromAppliedConfig`).
  - (d) reincarnate carries the pin, its legacy label and the unpin tombstone
    forward.
- **I2-I4.** The PATCH strips `config.env.TZ` (warning when non-empty);
  the storage merge, env secrets (`dropTZTargetedSecrets`), as_needed
  gather and finalize-env (`withoutCallerTZ`) skip `TZ`; `takeTZGatherNeed`
  removes `TZ` from broker needs and answers it from the resolver
  (`UTC` when no rung applies) for old brokers; `shouldPersistResolvedEnvKey`
  rejects `TZ`.
- **Labels.** `buildEnvSources` and `buildEnvGatherResponse` report the
  resolver's source for `TZ`.
- **PATCH response.** Every agent PATCH returns `resolvedTimezone`,
  `timezoneSource` and `warnings` (the `config.env` ignore warning, and the
  next-start warning when a running agent's pin changes).
- **Broker warnings.** `api.AgentInfo.HubOnlyEnvWarnings` (the hub-only env
  `TZ` drop warnings) is copied to `runtimebroker.AgentResponse.Warnings`
  (omitempty) and relayed in the hub create and start responses.

## Removed (runtime-profile timezone)

In `HTTPAgentDispatcher.buildCreateRequest`, the profile-timezone injection
and the separate hub-default fallback were removed. The resolver replaces both.
The deleted test is `TestHTTPAgentDispatcher_TZInjection_ProfileTimezone`.
`profileTimezoneProvider`, `SetProfileTimezoneProvider` and the `server.go`
wiring stay in place for tz-refactor task 13 to remove.

## Merge order

The runtime-profile timezone stops affecting agents with this change.
Merge it together with or after tz-refactor task 13 (ptone/scion#2506,
profile retirement). Otherwise the profile timezone card has no effect for a
short time.

## Known limitations

- Restart discards the broker response, so broker `Warnings` are relayed on
  create and start only.
- Version skew: the unpin assertion (no `TZ` reaches the container when no
  rung applies) needs tz-refactor task 15 (ptone/scion#2508) on the broker.
  An older broker fills `TZ` itself.

## Tests

`agent_tz_dispatch_test.go` covers the chain on create, start and restart,
the live hub default, legacy adoption, TZ secrets, persistence, labels, the
old-broker no-launder case and the relayed warnings.
`agent_tz_http_test.go` covers create capture (HTTP and scheduled spawn), the
PATCH, reincarnate and the GET.
Tests were updated in `applied_config_env_write_path_test.go`,
`pkg/runtimebroker/types_test.go` and
`pkg/agent/broker_hub_env_authority_test.go`.
All of them were run under `TZ=UTC`, `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu`.
