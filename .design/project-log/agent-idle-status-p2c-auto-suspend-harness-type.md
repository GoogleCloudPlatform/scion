# Auto-suspend resume support from the harness type (ptone/scion#7922)

**Date:** 2026-11-14
**Branch:** scion/agent-idle-status-p2c
**Parent:** ptone/scion#7826 (agent idle status), phase 2c

## Problem

Hub auto-suspend (`autoSuspendStalledAgent`, `pkg/hub/server.go`) decided
whether an agent's harness supports resume with
`harness.New(agent.AppliedConfig.HarnessConfig)`. That argument is the
harness-config *name*. `harness.New` only knows built-in harness names and
falls back to the generic harness, which declares resume unsupported. So an
agent created from a custom-named harness config (for example `team-reviewer`
of harness `claude`) was never auto-suspended, and the skip was logged only at
debug. `harnessSupportsResume`, which the HTTP suspend and membership-loss
paths use, had the same bug.

## Fix

`harnessSupportsResume(ctx, agent)` (`pkg/hub/handlers_agent_lifecycle.go`) is
the one resume check. It judges the underlying harness type: the harness
stamped on the agent at create (`storedAgentHarness`), else the type its
harness-config reference resolves to through the existing
`resolveAgentHarnessType` (project scope, then global, then name inference).
It returns `(supported, harnessType, reason)`. An agent whose applied config
names no harness is still treated as supported.

Auto-suspend now calls this helper and logs every skip at info:
`Scheduler: skipping auto-suspend; harness does not support resume` with
`harness_config`, `harness` and `reason`.

## Notes for phase 3

The idle sweep should call `harnessSupportsResume` too, not build its own
check. The only built-in harness without resume today is `generic`. A
harness-config reference that no longer resolves (deleted config) falls back to
`generic`, so the agent is skipped and the log names the reason.

## Tests

`pkg/hub/fix_7922_auto_suspend_custom_harness_config_test.go`: a custom-named
config of `claude` is suspended; a custom-named config of `generic` is skipped,
with the info log line and its reason.
