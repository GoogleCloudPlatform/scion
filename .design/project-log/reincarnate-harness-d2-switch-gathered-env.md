# Reincarnate --harness: the new harness's first start lacked its as_needed env (D2)

**Date:** 2026-11-14
**Branch:** scion/reincarnate-harness-d2
**Issue:** ptone/scion#7757 (e2e defect D2; the e2e report calls it D1)

## Symptom

On the community hub (docker broker), `scion reincarnate X --harness claude` on an agent created
with codex ended with phase error, container `Exited (1)`, and "pre-start hook failed: ...
20-harness-provision: ... provisioner output omitted: it contains a staged value too short to
mask". codex -> antigravity failed the same way with a readable message: "auth type ... selected
but no credentials found; set GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_LOCATION". claude -> codex,
claude -> claude (reset) and a fresh claude agent all passed. The agent API redacts
`appliedConfig.env`, so the tester's field-by-field config comparison could not show the
difference.

## Root cause

1. A hub start does not resolve `as_needed` env: `buildStartEnv` -> `selectDeliveryEnv` skips
   those rows (`pkg/hub/httpdispatcher.go`, `pkg/hub/material_delivery.go`). Only the env-gather
   second pass of a create, provision or reprovision resolves them. The Vertex AI project and
   location are such values here.
2. Restarts of a create-time agent still work because the broker records the auth secrets the
   creating start staged (`recordSecrets`, `agentDir/harness-secrets`, outside the home) and
   restores them on later starts, but only when the record's harness-config identity (name,
   source, hub record ID, revision) matches (`restoreSecretsRecord`,
   `pkg/agent/harness_config_policy.go`).
3. On a switch, `dispatchProvision` (reprovision) does gather and resolve the target's
   `as_needed` keys, but only for the provision-only request: Reprovision stages no auth secrets,
   and the persist-back into AppliedConfig is skipped on reprovision by design. The start that
   follows had no Vertex env, and the record had the codex identity, so nothing was restored
   (broker log: "recorded secrets are restored only for the harness-config revision that staged
   them; none restored"). The broker auto-detected vertex-ai from the assigned GCP service
   account, the hub credential preflight passed for the same reason (`isAuthTypeSatisfied` skips
   the env check when an SA is assigned), and the in-container provisioner failed.
4. claude's message was omitted, unlike antigravity's, because `ContainerScriptHarness.ResolveAuth`
   adds `CLAUDE_CODE_USE_VERTEX=1` for vertex-ai, which is staged as a secret file, and
   `scrubSecrets` replaces the whole output when a staged value shorter than 8 characters occurs
   in it ("1").

## Fix

`pkg/hub/reprovision_gathered_env.go`: for a home-reset reincarnation the worker passes a ctx with
a holder (`startCtx`) to `DispatchAgentReprovision` and `DispatchAgentStart` only. After a
confirmed reprovision, `dispatchProvision` records in it the names of the `as_needed` keys its
gather resolved (the broker's own verdict of what the target needs), each alternative name mapped
to its canonical key. Values are never kept. `buildStartEnv` then admits the `as_needed` env vars
and environment secrets stored under those names through the same `deliverySelector` as all other
launch material (`selectDeliveryEnvWith`, `selectDeliverySecretsWith` in
`pkg/hub/material_delivery.go`), so an item the delivery decision refuses is withheld. They fill
only keys nothing else set, under the canonical key, classified secret-fetchable. Nothing is logged
or persisted; the broker's record of that start keeps the values for later restarts, as after a
create. A plain reincarnation gets no holder and is unchanged. No wire or capability change.

A first version carried the gathered values themselves; review pass 1 (R1) found that this
bypassed the start's per-item delivery authorization, so the holder now carries names only.

## Adjacent, not fixed

- `scrubSecrets` (`cmd/sciontool/commands/harness.go`) discards the whole provisioner output when
  any staged value shorter than 8 characters occurs in it; `CLAUDE_CODE_USE_VERTEX=1` is staged as
  a secret, so every claude vertex-ai provisioner failure is unreadable.
- A hub-side harness-config revision change has the same failure mode for restarts of agents whose
  credentials come from `as_needed` env: the record's revision no longer matches, and a start
  resolves no `as_needed` env.
- A crash of the new harness right after a successful start (pre-start hook) counts as a
  completed reincarnation: `reincarnationState` is not set to failed and no fail-forward hint is
  sent. See the PR for the proposal.

## Verification

See the PR body for the gates that ran.
