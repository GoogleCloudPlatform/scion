# B2/B1/B14 Architect Review Fixes

**Date:** 2026-08-28
**Branch:** scion/ca-msg-em6-b2b1b14
**Author:** dev-b2b1b14-fixes

## Summary

Three fixes requested by the architect after reviewing the B2/B1/B14 DM migration
changes. All are non-functional (tests and comments only); no production code changed.

## Fix 1: AST Enumeration Test for D-1 Guard Call Sites

**File:** `pkg/store/entadapter/dm_guard_enumeration_test.go`

Created a new AST-based enumeration test with no build tag, so it runs under
`-tags no_sqlite` (CI mode). The test parses `conversation_store.go` and verifies:

1. `AddParticipant` calls `checkDMParticipantKey`
2. `EnsureParticipant` calls `checkDMParticipantKey`
3. `checkDMParticipantKey` delegates to `messages.CheckDMParticipantKey`

### Mutation m4 Results

**m4 under `-tags no_sqlite` (CI mode) — KILLED:**
```
--- FAIL: TestDMGuardCallSites_Enumeration (0.00s)
    --- FAIL: TestDMGuardCallSites_Enumeration/checkDMParticipantKey_delegates_to_messages (0.00s)
        dm_guard_enumeration_test.go:158: checkDMParticipantKey does not call messages.CheckDMParticipantKey — the delegation to the shared predicate in pkg/messages has been severed. See conversation_store_test.go for the behavioural tests.
FAIL
FAIL	github.com/GoogleCloudPlatform/scion/pkg/store/entadapter	0.010s
FAIL
```

**m4 without tag (full mode) — KILLED:**
```
--- FAIL: TestDMGuardCallSites_Enumeration (0.00s)
    --- FAIL: TestDMGuardCallSites_Enumeration/checkDMParticipantKey_delegates_to_messages (0.00s)
        dm_guard_enumeration_test.go:158: checkDMParticipantKey does not call messages.CheckDMParticipantKey — the delegation to the shared predicate in pkg/messages has been severed. See conversation_store_test.go for the behavioural tests.
FAIL
FAIL	github.com/GoogleCloudPlatform/scion/pkg/store/entadapter	0.011s
FAIL
```

## Fix 2: Tautology Explanation at stepRebuildParticipants

**File:** `pkg/messaging/dm_migration.go` (~line 206)

Added a comment explaining why `AddParticipant` in `stepRebuildParticipants` has
no explicit `CheckDMParticipantKey` guard: the principals `{kindA, idA}` and
`{kindB, idB}` are derived from `ParseDMKey` on the row's own `external_ref`,
so the check would re-parse the same string and compare its output to itself
(a tautology). Points to `mergeConversation` (~line 439) as the site where
principals are foreign and the guard is load-bearing.

## Fix 3: DEF-29 in B14 Pinning Tests

**File:** `pkg/messaging/dm_migration_test.go`

Updated doc comments for all five B14 pinning tests to reference DEF-29 and
explain that they pin current-but-wrong behaviour:

1. `TestGuardA_Migration_EmptyRefDirectRowsSkipped`
2. `TestStep3a_EmptyRefRowSkipped`
3. `TestStep3a_EmptyRefNotRekeyed`
4. `TestStep3a_EmptyRefSkippedRegardlessOfParticipantCount`
5. `TestB14_EmptyRefRowLeftKeyless`

### DEF-29 Naming Rationale

A direct conversation's `external_ref` IS its access-control basis — the DM key
names who is entitled to see the messages. A keyless row has no ACL at all. The
migration currently leaves these rows keyless because deriving a key from the
listing index would fabricate an ACL (B14 ruling). This is correct behaviour
given the constraint, but the underlying problem (keyless rows exist) is tracked
as DEF-29. When DEF-29 closes, the test expectations will invert; the correct
resolution is operator review, not automated migration.
