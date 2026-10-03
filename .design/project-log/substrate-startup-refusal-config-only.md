# Substrate: broker startup refusal limited to deterministic config failures

**Date:** 2026-10-03
**Branch:** scion/substrate-pr3-fix2 (on top of scion/substrate-pr3-restack-wip)

## What changed

- **Startup refusal covers config failures only.** Before this change, `runtime.GetRuntime`
  tagged every `NewSubstrateRuntime` error with `ErrSubstrateProfileInvalid`. That included
  construct-time dependency failures: building the Kubernetes client, and `substrate.Dial`'s
  trust-bundle/CA load and API dial. So an API-server blip at boot made the broker refuse to
  start. Now only deterministic config failures carry the sentinel:
  - `ValidateOperatorOnlySubstrateProfile`, tagged in `factory.go`;
  - the required `api_endpoint` / `router_endpoint` checks and `substrate.Validate`, tagged in
    `NewSubstrateRuntime`.

  Errors from `substrateRuntimeBuilder` are returned untagged. The broker starts degraded on a
  plain `ErrorRuntime`, and a pod restart is the retry. The docs on the sentinel,
  `NewSubstrateRuntime`, and `refuseErrorRuntimeAtStartup` now describe this split. The refusal
  message reads "failed config validation".
- **Tests** (`cmd/server_foreground_broker_test.go`), both driven from operator-level settings
  through `resolveBrokerDefaultRuntime`:
  - `..._SubstrateConfigValidationFailureRefusesStart`: an unsupported `egress_trust_bundle`
    still refuses startup, and the builder is never reached.
  - `..._SubstrateBuildFailureStartsDegraded`: a stubbed builder error does not refuse. The
    broker logs `Runtime broker using runtime: error`.

  Each test fails when its half of the fix is reverted: tagging all errors in the factory again
  breaks the second; dropping the tag on validation failures breaks the first.
- **Docs-only:**
  - The `managerRuntimeProvider` test-seam comment.
  - The `substrateEgressHostnames` doc now names `EgressAllowCovers` as the primary control
    for tenant-derived hosts, with IP-literal/CIDR rejection as defense-in-depth.
  - `deploy/substrate/OPERATIONS.md` gains a timeout invariant note. The hub→broker create
    timeout must cover the substrate start budget, but the hub side is a fixed 120s with no
    setting. The broker's RUNNING wait (`defaultActorRunningTimeout`) is also a constant; only
    `template_ready_timeout` can be configured.

## Learnings

- Pre-existing, environment-only failures in `cmd` (`TestHubAllOrOneActions`,
  `TestReincarnateHandoffTemplate_WorksAnywhere`): inside an agent container they reach the
  container's real hub and get 401. They fail the same way on the unmodified base and are
  unrelated to this change.

## Follow-ups (raised, not implemented)

- The hub's create timeout (control-channel `RequestTimeout` and the direct HTTP broker client)
  is hard-coded to 120s, shorter than the substrate worst-case start budget. Making it
  configurable, or deriving it per runtime, would close the orphaned-actor gap that
  OPERATIONS.md currently only documents.
