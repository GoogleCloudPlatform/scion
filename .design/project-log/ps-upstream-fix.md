# Project Log: ps-upstream-fix

**Branch:** scion/project-skills-phase2  
**Upstream PR:** GoogleCloudPlatform/scion/pull/846 (fork PR #844)  
**Date:** 2026-07-23  
**Commit:** ed9ec99c

---

## Task 0 — False Positive Verdict

**Claim:** `skillBaseURI` is "redeclared" in handlers_skills_injection.go vs handlers_agent_create_helpers.go.

**Verdict: CONFIRMED FALSE POSITIVE — no code change made.**

Investigation:
```
grep -rn "func skillBaseURI\|func baseSkillURI" pkg/hub/
```
Result: exactly ONE function definition at `handlers_skills_injection.go:719`.

The test file `handlers_agent_create_helpers_test.go` contains
`TestSkillBaseURI_StripVersion` — a *test function* that exercises the
`skillBaseURI` helper, not a duplicate definition. The reviewer confused a
test function name with a redeclaration. The function is called from both
`handlers_skills_injection.go` and `handlers_agent_create_helpers.go` (the
latter imports it by being in the same package), which is correct Go. CI
build+vet+lint passed, which would have caught a real redeclaration.

---

## Fix 1 — N+1 Query: Batch GetSkillBySlug

**File:** `pkg/hub/handlers_skills_injection.go`

**Problem:** `enrichSkillInjections` called `GetSkillBySlug` once per
injection entry (potentially twice: global scope, then core scope fallback),
resulting in up to 2N DB calls for N entries.

**Fix:** Replaced per-entry DB calls with two `ListSkills` batch queries (one
for `SkillScopeCore`, one for `SkillScopeGlobal`, limit 1000 each) executed
before the loop. A `slug→*store.Skill` in-memory map is built from the
results. Per-entry enrichment then does O(1) map lookups. Global entries
overwrite core entries on slug collision (same priority as the original
try-global-first logic). Total DB calls: 2 (constant), down from 2N.

---

## Fix 2 — sortOrder POST Appends at End

**File:** `pkg/hub/handlers_skills_injection.go`

**Problem (2 instances):**
- `addProjectInjectedSkill` (project scope POST): stored `sortOrder = entry.SortOrder` which defaults to 0, placing every new entry at the beginning.
- `addUserInjectedSkill` (user scope POST): same.

**Fix (both instances):** Added pre-insert logic: if `entry.SortOrder == 0`
(client didn't supply an explicit position), query `ListSkillInjections` for
the current scope, find the max `SortOrder`, and set the new entry's
`SortOrder = maxOrder + 1`. Explicit non-zero client values are preserved as-is.

---

## Fix 3 — Unique URI Validation Before DB Hit

**File:** `pkg/hub/handlers_skills_injection.go`

**Problem (2 instances):**
- `setProjectInjectedSkills` (project scope PUT): validated that entries were non-empty but did not check for duplicate `skillUri` values. A duplicate URI in the input hit the DB unique constraint and returned a raw constraint error.
- `setUserInjectedSkills` (user scope PUT): same.

**Fix (both instances):** Added pre-validation loop that builds a `seen` map
over trimmed `skillUri` values. If a duplicate is detected, returns HTTP 400
with `ErrCodeInvalidRequest` and a human-readable message:
`"duplicate skillUri in request: <uri>"`. Applied before any DB write.

---

## Fix 4 — Web UI: Clear Stale Rows on Empty scopeId

**File:** `web/src/components/shared/injected-skills-panel.ts`

**Problem:** `updated()` lifecycle hook correctly prevented loading when
`scopeId` became empty/falsy, but did not clear `this.rows`. When a panel
was reused with a new empty `scopeId`, rows from the previous scope remained
visible.

**Fix:** Added `else` branch to the `updated()` guard:
```typescript
} else {
  this.rows = [];
  this.error = null;
}
```
This clears stale state when `scope`/`scopeId` becomes invalid. The
`connectedCallback` has no corresponding else — on fresh connect the
component initializes with `rows = []` so there is no stale state to clear.

---

## Quality Check Results

| Check | Result |
|-------|--------|
| `go build ./...` | PASS |
| `go test ./pkg/hub/...` | 3 pre-existing failures (TestAddProvider, TestUpdateAgent_AllowsGeminiMaxModelCalls, TestSystemInit_EmbedOnlyHarness) — confirmed pre-exist on branch before this change, unrelated to skill-injection code |
| `go vet ./...` | PASS |
| `tsc --noEmit` | PASS |

---

## Commit

```
ed9ec99c fix(skill-injection): address upstream PR review findings
```

Pushed to `origin/scion/project-skills-phase2`.
