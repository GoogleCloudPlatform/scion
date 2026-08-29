# DEF-26: Rename AC-DEF8-1 placeholder test

**Date:** 2026-08-27
**Author:** dev-def26-rename
**Branch:** scion/ca-msg-em6

## Summary

Renamed `TestAC_DEF8_1_ConvergenceTwoPathsSameConversation` to
`TestResolve_SamePathIdempotency_AgentDM` in `pkg/messaging/resolve_test.go`.

The test was carrying the AC-DEF8-1 badge but did not test cross-path
convergence — it only exercised the resolver path twice (same-path
idempotency). The real cross-path convergence test is
`TestAC_DEF8_1_CrossPath_DualWriteAndResolverConverge`, which calls both
`ResolveOrCreateDMConversation` and `Resolve`.

## Changes

- Renamed the function from `TestAC_DEF8_1_ConvergenceTwoPathsSameConversation`
  to `TestResolve_SamePathIdempotency_AgentDM`.
- Replaced the doc comment: removed the AC-DEF8-1 claim and described what the
  test actually asserts (same-path idempotency, not cross-path convergence).
- Updated the cross-path test's comment to reference the new name.
- Both tests pass: `go test ./pkg/messaging/ -run 'TestResolve_SamePathIdempotency_AgentDM|TestAC_DEF8_1' -v -count=1`.
