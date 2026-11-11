# Substrate: endpoint-refusal test coverage, gid-0 exec guard, redaction restart note

**Date:** 2026-10-03
**Branch:** scion/substrate-pr3-fix3 (on top of scion/substrate-pr3-fix2)

## Changes

1. **Boot-refusal test coverage** (`cmd/server_foreground_broker_test.go`).
   `TestResolveBrokerDefaultRuntime_SubstrateConfigValidationFailureRefusesStart`
   is now a table with one row per deterministic config-validation tagging
   site in `NewSubstrateRuntime`: missing `api_endpoint`, missing
   `router_endpoint`, and `substrate.Validate` rejecting the settings. Each
   row asserts the broker refuses to start (`ErrSubstrateProfileInvalid`, nil
   runtime, nothing logged, builder never called). New helper
   `operatorSubstrateSettingsWithBlock` lets a row omit a field.
   Mutation check: dropping the `substrateProfileInvalid` tag on either
   endpoint check fails only that endpoint's row.

2. **Refuse primary gid 0 for `/exec`** (`pkg/sciontool/substrate/execuser.go`).
   `execUserCredential` already refused uid 0. It now also refuses a passwd
   entry whose primary gid is 0, placed before the same-identity shortcut
   just like the uid-0 check, so the exec child never runs with the root
   group as its primary group. Tests: `TestExecUserCredential_RootGIDIsRefused`
   (fails when the guard is removed) and the positive control
   `TestExecUserCredential_NonZeroGIDIsAccepted`. The two tests that use a
   real identity (`SameIdentitySkipsCredential`, `RealScionUser`) now skip
   when that identity's primary gid is 0, the same way they already skip
   for uid 0.

3. **Ops note** (`deploy/substrate/OPERATIONS.md`, "Exec error redaction
   after a restart"). The exec-secret redaction cache lives in memory only.
   For agents this broker process didn't bootstrap (for example after a
   restart), exec error text reaches the broker log unredacted. Client
   responses stay opaque, and the exec child gets no agent secrets.

## Notes

- `./cmd/` has two tests that fail in the agent sandbox
  (`TestHubAllOrOneActions`, `TestReincarnateHandoffTemplate_WorksAnywhere`).
  They fail the same way on the base commit, and the cause is the sandbox
  agent home, not these changes.
