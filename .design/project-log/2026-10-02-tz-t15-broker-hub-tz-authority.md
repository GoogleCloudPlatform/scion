# tz-refactor task 15: broker hub authority for TZ

Issue: ptone/scion#2508 (refs ptone/scion#2457). Design: tz-refactor design §3 A (c), rung 4 and invariant I4.

## What changed

For a broker-mode start (`opts.BrokerMode`, which every hub dispatch sets), the hub is now the only source of the container `TZ`:

- `buildAgentEnv` (`pkg/agent/run.go`) takes a `brokerMode` parameter. In broker mode it skips the post-expansion key `TZ` from the config layer (persisted `scion-agent.json`, broker-local templates, harness-config directory env). The skip runs before the empty-value host passthrough, so a `TZ: ""` marker no longer leaks the broker host's `TZ`. An empty hub `TZ` is omitted without being reported missing.
- `resolveAuthEnvOverlay` skips `TZ` when merging the broker settings' harness-config entry env.
- `extractRequiredEnvKeys` (`pkg/runtimebroker/handlers.go`) never lists `TZ` as a required key, secret or alternative, in either mode.
- Each dropped non-empty `TZ` is logged with `slog.Warn` (agent ID, value, layer `config` or `harness-config entry`) and added to the `AgentInfo.Warnings` that `Start` returns. For hub-dispatched agents the operator surface is the broker `slog.Warn` line; `AgentInfo.Warnings` shows only in solo/CLI output. A dropped value that equals the hub-supplied value is not reported, because the container gets that value anyway.
- `finalScionCfg` is never mutated and nothing is scrubbed from disk. Solo mode is unchanged.

The seam for tz-refactor task 16 is `pkg/agent/broker_hub_env_authority.go`: the `hubOnlyEnvKeys` set, `IsHubOnlyEnvKey` (used by the broker preflight), the `droppedBrokerEnv` record and `warnDroppedBrokerEnv`.

## Evidence

- New tests: `pkg/agent/broker_hub_env_authority_test.go` (buildAgentEnv in both modes, harness-config entry merge, warnings, and `Start` tests for rung tests (i) and (ii), including the `HarnessAuth` on-disk check and solo passthrough), and `pkg/runtimebroker/handlers_hub_only_env_test.go` (required keys, auth alternatives, and an env-gather create that is not blocked by an empty `TZ`). The broker-mode tests fail with the rule disabled.
- `pkg/agent/...` and `pkg/runtimebroker/...` were run under `TZ=UTC`, `Asia/Tokyo` and `Asia/Kathmandu`. golangci-lint on the changed packages found 0 issues. The PR has the details.
- `image-build/` contains no `TZ` at all, so no core image sets `ENV TZ` (prerequisite from tz-refactor task 7).

## Follow-ups

- Carrying the drop warnings to the hub over the wire: moved to task 16 (ptone/scion#2509) by tz-lead ruling. There, an `AgentResponse` warnings field carries only the hub-only-env drop warnings, and the hub dispatcher appends them.
