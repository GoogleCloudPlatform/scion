# tz-refactor task 16 (P2a-1), part (a): `ExplicitTimezone` fields and `resolveAgentTZ`

**Date:** 2026-10-02
**Branch:** `scion/tz-t16a` (part (b), the wiring, is stacked on it as `scion/tz-t16b`)
**Fork issue:** ptone/scion#2509 (part of ptone/scion#2457, design Option A)

## What changed

- `store.AgentAppliedConfig` gains three `omitempty` fields:
  `ExplicitTimezone` (the pinned IANA zone), `ExplicitTimezoneLegacy`
  (provenance: adopted from a `TZ` an older hub persisted in `Env`) and
  `ExplicitTimezoneUnpinned` (tombstone for an explicit unpin, so the create
  pipeline and reincarnate do not re-pin).
- `pkg/hub/agent_tz.go` adds the agent `TZ` chain:
  - `chooseAgentTZ` (pure): explicit pin (`explicit`/`legacy`) > storage
    env > hub `default_timezone` (`hub-default`) > nothing (`none`);
    `forGatherAnswer` turns the last rung into `UTC` for answering an old
    broker's env-gather need.
  - `resolveStorageTZ`: walks `envScopePrecedence` with a `Key: "TZ"` filter
    (always-mode only, empty values never win), then progeny; reports
    `user|project|hub|broker|progeny`.
  - `(*HTTPAgentDispatcher).resolveAgentTZ`: combines them, reading
    `hubAgentDefaultsProvider` live.
- No runtime-profile rung (decision D3). No caller yet: part (b) wires it
  into `buildCreateRequest`, `buildStartEnv`, the writer table and the PATCH.

## Deviation from design

Design §3 A (c) specifies `resolveAgentTZ(ctx, agent, storageEnv) (tz,
source string)`. The implementation is `resolveAgentTZ(ctx, agent,
forGatherAnswer bool) agentTZ{TZ, Source}`:

- `forGatherAnswer` is needed by I4 (answering an old broker's `TZ` gather
  need with `UTC` when no rung supplies one).
- The `storageEnv` parameter was dropped. The map `resolveEnvFromStorage`
  builds carries no scope, so it cannot yield the `user|project|hub|broker|
  progeny` source label, and it lets an empty higher-scope value win, which
  the TZ chain must not do (first non-empty wins).
- Cost: the resolver reads storage itself, at most 5 indexed, `Key`-filtered
  queries per dispatch (4 scopes plus progeny), and none when the agent is
  pinned.

`resolveAgentTZ` also logs a warning when it meets an unadopted legacy `TZ`
in the agent's `Env`/`InlineConfig.Env`; the result is unchanged. Part (b)
must run `adoptLegacyTZ` at every entry point before resolving.

## Why

Design §3 A (c): one resolver is the only source of the agent container
`TZ`, so create, start and restart agree and the reported source matches
what the container runs.

## Tests

`agent_tz_test.go` (chain table, JSON omitempty and round trip) and
`agent_tz_storage_test.go` (scope order, explicit vs storage, as_needed and
empty skipping, progeny, live hub default, SQLite round trip). Run under
`TZ=UTC`, `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu`, all green.

## Follow-ups

- Part (b) on `scion/tz-t16b`.
- The winning progeny `TZ` across two ancestors is not deterministic
  (`ListProgenyEnvVars` orders by key only). This is pre-existing and shared
  with `resolveEnvFromStorage`; tracked in ptone/scion#2637.
