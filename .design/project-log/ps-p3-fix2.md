# ps-p3-fix2: Phase 3 Second-Pass LOW Findings

**Branch:** scion/project-skills-phase2  
**File:** pkg/hub/handlers_agent_create_helpers.go  
**Date:** 2026-07-22

## Summary

Fixed three LOW-severity findings from the Phase 3 second-pass code review on PR #544.

---

## LOW-1: ErrNotFound not suppressed for user/project ListSkillInjections

**Location:** `mergeInjectedSkills`, lines 332 and 345

**Issue:** The hub-scope error path at line 317 correctly silences `store.ErrNotFound`
(the normal case when no hub skills are configured). The user-scope and project-scope
`ListSkillInjections` error paths logged `slog.Warn` for *all* errors including
`ErrNotFound`, creating an inconsistency that could become a real bug if the store
implementation changes.

**Fix:** Wrapped each `slog.Warn` in both branches with an `!errors.Is(err, store.ErrNotFound)`
guard, matching the hub-scope pattern exactly:

```go
if !errors.Is(err, store.ErrNotFound) {
    slog.Warn("mergeInjectedSkills: failed to fetch user injected skills", "error", err)
}
// continue, best-effort
```

Applied identically for user-scope (~line 332) and project-scope (~line 345).

---

## LOW-2: Version-conflict warning message implies cross-scope conflict

**Location:** `mergeSkillRefs`, line 394

**Issue:** The `slog.Warn` message "skill injection version conflict resolved" implied
the conflict was definitely cross-scope. However, if the same base URI appeared twice
within one scope (a configuration error), it would also trigger this path — producing
a misleading diagnostic.

**Fix:** Updated the warning message to "possible skill injection version conflict or
duplicate URI", which accurately covers both cross-scope version conflicts and
within-scope duplicate base URI scenarios.

---

## LOW-3: Dead `ok` guard on `first[base]` map lookup

**Location:** `mergeSkillRefs`, line 386 (in the post-loop warn pass)

**Issue:** The map lookup `if orig, ok := first[base]; ok && ...` had a dead `ok`
guard. Because `first[base]` is populated in the same loop body as `seen[base]`
(every key in `seen` was put there at the same iteration that wrote `first[base]`),
the `ok` check is always true when ranging over `seen`. The guard was redundant dead
code.

**Fix:** Replaced the guarded lookup with a direct assignment:

```go
orig := first[base]
if orig.URI != winner.URI {
```

This eliminates the dead variable and makes the invariant explicit.

---

## Verification

- `go build ./...` — clean
- `gofmt -l .` — no output (no formatting issues)
- `go test -tags sqlite ./pkg/hub/...` — all 14 Phase 3 tests + existing tests pass
