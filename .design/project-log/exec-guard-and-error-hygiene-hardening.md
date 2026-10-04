# Project Log: exec-site fail-closed guards and generic error hygiene

**Date:** 2026-09-30

## Overview

Closed the remaining fail-open exec sites in the generic enforced
privilege-drop mode: `services.Manager`'s service-start path and
`gitCloneWorkspace`'s git-command path now refuse outright, mirroring
`supervisor.Supervisor.Run`'s existing guard, instead of relying solely on
the caller-side `requirePrivilegeDropOrFail` clamp. `hooks.LifecycleManager`'s
`buildEnforcedCmd` gained the same guard on its dropped branch, and its
pre-start root branch now clears its working directory exactly like the
post-start root branch already did.

## Enforcement

- `services.Manager`'s `managedService.start` returns the new
  `services.ErrPrivilegeDropRequired` instead of starting a service with no
  `Credential` when `requirePrivilegeDrop` is set but uid/gid do not both
  pass the credential predicate.
- `gitCloneWorkspace` takes a `requirePrivilegeDrop` parameter and refuses
  before running any git command under the same condition.
- `hooks.LifecycleManager.buildEnforcedCmd`'s dropped branch refuses to
  build a runnable command when `WorkloadUID`/`WorkloadGID` are not both
  valid, mirroring the harness-provision wrapper's existing guard. Its
  pre-start root branch now sets `Dir` to `/`, matching the post-start root
  branch.
- `pkg/sciontool/rootexec/guard_test.go`'s exec-site allowlist
  justifications for `supervisor.go`, `services/manager.go` and the
  substrate scan directory now state the actual, conditional guarantee (a
  Credential drop, or a fail-closed refusal) rather than an unconditional
  claim.

## Error hygiene

- `runtimebroker`'s `stopAgent` and `resolveDeleteTarget` log the
  underlying runtime-list and record-less-actor-probe errors server-side
  and return a fixed, scope-free message to the caller, instead of
  including the raw error (which can carry a runtime-scope identifier or
  an internal address) in the HTTP body.
- The substrate bootstrap environment strips `SCION_KEEPID_UID`, which has
  no meaning on this runtime and could otherwise cause the host-uid drop
  to be skipped if a workload-supplied environment happened to carry it
  forward.

## Text-only cleanup

Reworded internal comments and test descriptions that described their own
development history relative to a prior state, so each now states the
invariant or the behavior directly instead. Neutralized two leftover
fixture names that named an internal namespace concept out of scope for
the generic hardening they annotate.
