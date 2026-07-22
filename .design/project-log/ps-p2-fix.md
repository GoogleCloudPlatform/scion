# Phase 2 Security Fixes — Injected Skills

**Branch:** `scion/project-skills-phase2`  
**Commit:** `f294abb0`  
**Date:** 2026-07-22  
**PR:** #544

## Summary

Fixed 5 security and correctness findings identified in code review of the
Phase 2 Injected Skills REST API (PR #544). All fixes include new tests.

---

## C-1 [CRITICAL] — IDOR: removeUserInjectedSkill

**File:** `pkg/hub/handlers_skills_injection.go`

**Problem:** `store.RemoveSkillInjection` takes only an entry UUID. The handler
called it directly without checking ownership — user A could delete user B's
entry by guessing the UUID via `DELETE /api/v1/users/me/injected-skills/{id}`.

**Fix:** Fetch-then-verify pattern:
1. List caller's own entries via `ListSkillInjections(ctx, scopeUser, callerID)`
2. Scan for the requested `entryID` in the list
3. If found: call `RemoveSkillInjection` (safe — confirmed owner)
4. If not found: return 404 (doesn't exist or belongs to another user)

**Test:** `TestRemoveUserInjectedSkill_CrossUserIDORRejected` — Alice tries to
DELETE Bob's entry via her own `/users/me/` path; verifies 404 and that Bob's
entry is untouched.

---

## C-2 [CRITICAL] — IDOR: removeProjectInjectedSkill

**File:** `pkg/hub/handlers_skills_injection.go`

**Problem:** Same root cause as C-1. A project-A admin could delete a project-B
entry by supplying a cross-project UUID. The authz check only verified the
caller had access to project-A, not that the entry UUID belonged to project-A.

**Fix:** Fetch-then-verify pattern:
1. List project's own entries via `ListSkillInjections(ctx, scopeProject, projectID)`
2. Scan for `entryID` in the list
3. If found: `RemoveSkillInjection`; if not found: 404

**Test:** `TestRemoveProjectInjectedSkill_CrossProjectIDORRejected` — Alice
tries to DELETE project-B's entry via project-A's URL; verifies 404 and that
project-B's entry is untouched.

---

## H-1 [HIGH] — Hub PUT destroys system entries on corrupt blob

**File:** `pkg/hub/handlers_skills_injection.go` (`setHubInjectedSkills`)

**Problem:** When `json.Unmarshal` of the stored `hub_settings["injected_skills"]`
blob fails, the handler logged a warning, continued with `existing.System = nil`,
and upserted `{system: [], user_defined: [...]}` — silently destroying all system
skill entries. Response was HTTP 200.

**Fix:** Return HTTP 500 immediately when unmarshal fails, before any upsert:
```go
if jsonErr := json.Unmarshal(setting.Value, &existing); jsonErr != nil {
    slog.Error("failed to parse existing hub injected_skills setting", "error", jsonErr)
    writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
        "internal error: failed to parse current hub skill settings", nil)
    return
}
```
Also added `else if !errors.Is(err, store.ErrNotFound)` guard so non-NotFound
errors from `GetHubSetting` also abort the request properly.

**Test:** `TestSetHubInjectedSkills_CorruptBlobReturns500` — uses raw SQL to
bypass Ent's JSON validation and seed a corrupt blob, then verifies PUT returns
500 and the stored value is unchanged.

---

## M-1 [MEDIUM] — listProjectInjectedSkills missing else-Forbidden

**File:** `pkg/hub/handlers_skills_injection.go` (`listProjectInjectedSkills`)

**Problem:** The GET handler for project injected-skills checked if the caller is
a `UserIdentity` and ran `CheckAccess`, but had no `else` guard. Non-UserIdentity
callers (agent tokens, broker tokens) bypassed the access check and received the
full skill list. All write handlers correctly had an else-Forbidden guard.

**Fix:** Added `else { Forbidden(w); return }` matching the write handler pattern:
```go
if userIdent, ok := identity.(UserIdentity); ok {
    // CheckAccess...
} else {
    Forbidden(w)
    return
}
```

**Test:** `TestListProjectInjectedSkills_ForbiddenForAgentToken` — generates a
real agent token for the project and verifies GET returns 403 Forbidden.

---

## M-2 [MEDIUM] — PUT bulk-replace silently drops SortOrder

**Files:**
- `pkg/store/store.go` — interface
- `pkg/store/entadapter/skill_injection_store.go` — implementation
- `pkg/hub/handlers_skills_injection.go` — bulk-replace handlers

**Problem:** `SetSkillInjections` accepted `[]api.SkillReference`, which has no
`SortOrder` field. The entadapter used the array index `i` as SortOrder.
Client-supplied `SortOrder` values were silently discarded in bulk-replace.

**Fix:** Changed signature to `[]store.SkillInjection`:
```go
// Before:
SetSkillInjections(ctx, scope, scopeID string, refs []api.SkillReference, createdBy string) error
// After:
SetSkillInjections(ctx, scope, scopeID string, entries []SkillInjection, createdBy string) error
```

Handler bulk-replace code now constructs `[]store.SkillInjection` directly from
the incoming `SkillInjectionEntry` list, preserving `SortOrder`. Falls back to
request position index when `SortOrder == 0` (same as prior behavior for clients
that don't set it). Removed `skillInjectionEntriesToRefs` helper. Removed `api`
import from `store.go` and `entadapter/skill_injection_store.go`.

**Phase 3 note:** Phase 3 provisioner integration will call `SetSkillInjections`
with `[]store.SkillInjection` — compatible with the new signature.

**Test:** `TestSetProjectInjectedSkills_SortOrderPreserved` — PUTs entries with
explicit non-default SortOrder (10, 20, 30); verifies store returns them sorted
correctly by SortOrder.

---

## Test Results

```
go test -tags sqlite ./pkg/hub/ -run "InjectedSkill|HubInjected" -v
# 30 tests: PASS

go test -tags sqlite ./pkg/store/...
# ok pkg/store, ok pkg/store/entadapter, ok pkg/store/storetest

go build ./...
# clean
```

Pre-existing unrelated failures in full hub suite:
- `TestAddProvider`
- `TestUpdateAgent_AllowsGeminiMaxModelCalls`  
- `TestSystemInit_EmbedOnlyHarness`

These were failing before this change (verified via `git stash` check).
