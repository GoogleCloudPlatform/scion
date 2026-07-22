# Phase 6 Fix: Platform Skills Migration Review Findings

**Branch:** scion/project-skills-phase2  
**Date:** 2026-07-22  
**Agent:** scion/ps-p6-fix

## Summary

All review and test findings from Phase 6 (Platform Skills Migration) have been addressed.
Changes are confined to `pkg/hub/platform_skills_seed.go` and its test files.

---

## Findings Addressed

### M1 — "seeded" → "seed" (updatedBy arg)
**File:** `pkg/hub/platform_skills_seed.go`, line ~99  
**Fix:** Changed the `updatedBy` argument to `UpsertHubSetting` from `"seeded"` to `"seed"`.
BackfillOrigin exempts only `updated_by="seed"` rows; using `"seeded"` caused the row's
origin to oscillate seeded→managed on every restart.

### M2 — Error paths silently wipe user_defined
**File:** `pkg/hub/platform_skills_seed.go`, lines ~75-86  
**Fix:** Both error paths now return an error instead of logging a warning and proceeding:
- `GetHubSetting` returning a non-`ErrNotFound` error → `return fmt.Errorf(...)`
- `json.Unmarshal` failing on existing value → `return fmt.Errorf(...)`
The `ent.IsNotFound` / `store.ErrNotFound` path (first run) still proceeds with `userDefined=nil`.
The caller in `server.go` already logs `WARN` and continues, so this is safe.

### L1 — inject_when comment
**File:** `pkg/hub/platform_skills_seed.go`  
**Fix:** Added comment near the skill iteration loop explaining that `inject_when` conditions
are not evaluated during seeding; the provisioner's `injectPlatformSkills` (Step 3a2) handles
`inject_when` filtering.

### L2 — Equality check before upsert
**File:** `pkg/hub/platform_skills_seed.go`  
**Fix:** Added a `jsonEqualSeed` helper and an equality check before calling `UpsertHubSetting`.
If the existing DB value semantically matches the desired value, the upsert is skipped to avoid
spurious revision bumps and event churn on every restart.

### L3 — Dead code: newSeedTestServer
**File:** `pkg/hub/platform_skills_seed_test.go`  
**Fix:** Removed the unused `newSeedTestServer` function. All tests use `testServer(t)` directly.

### L4 — Test: verify scion-platform:// URI prefix
**File:** `pkg/hub/handlers_skills_injection_test.go`  
**Fix:** Added assertion in `TestGetHubInjectedSkills_DefaultState` that each system entry URI
starts with `platformSkillURIPrefix` (`"scion-platform://"`).

### Test LOW-1 — UpsertHubSetting error path test
**File:** `pkg/hub/platform_skills_seed_test.go`  
**Fix:** Added `TestSeedPlatformSkillInsertions_UpsertError` test. Uses an `errUpsertStore`
wrapper (embedded `store.Store`, overrides `UpsertHubSetting` to return an error) to verify
that `seedPlatformSkillInsertions` returns an error rather than panicking or silently succeeding.

### Test LOW-2 — PUT body 'system' field ignored
**File:** `pkg/hub/handlers_skills_injection_test.go`  
**Fix:** Added `TestSetHubInjectedSkills_SystemBodyFieldIgnored` test. Verifies that a PUT
request to `/api/v1/hub/settings/injected-skills` that includes a `system` array in the body
does NOT modify the stored system entries (they are always preserved from the DB read).

### Test MEDIUM — empty-FS testability
**Action:** Opened GitHub issue **#548** on `ptone/scion`:
*"Phase 6 follow-up: make PlatformSkillsFS injectable for test coverage of empty-FS path"*  
Added `// TODO: make PlatformSkillsFS injectable for testability; see GitHub issue #548`
comment in `seedPlatformSkillInsertions`.

---

## GitHub Issue

- **#548**: Phase 6 follow-up: make PlatformSkillsFS injectable for test coverage of empty-FS path  
  https://github.com/ptone/scion/issues/548

---

## Quality Checks

- `go build ./...`: **PASS**
- `go vet ./pkg/hub/...`: **PASS**
- `go test ./pkg/hub/... -run "TestSeedPlatformSkillInsertions|TestGetHubInjectedSkills_DefaultState|TestSetHubInjectedSkills_SystemBodyFieldIgnored"`: **PASS** (all 6 targeted tests pass)
- Full `pkg/hub` suite: **3 pre-existing failures** (`TestAddProvider`, `TestUpdateAgent_AllowsGeminiMaxModelCalls`, `TestSystemInit_EmbedOnlyHarness`) — confirmed pre-existing on the branch before these changes; all newly added tests pass.

---

## Commit Hash

`4ca80fcc` — fix(p6): address all review findings for platform skills seed
