# Phase 2: Hub API — Injected Skills

**Branch:** `scion/project-skills-phase2`  
**Issue:** ptone/scion #542 (Injected Skills)  
**Author:** ps-p2-dev  
**Date:** 2026-07-22

## Summary

Implemented Phase 2 of the Injected Skills feature: REST API endpoints for managing injected-skills lists at the project, user, and hub scopes.

## Files Created/Modified

### `pkg/api/types.go` — Wire types
Added three new types:
- `SkillInjectionEntry` — API representation of one injected-skills list entry, with enrichment fields (`SkillName`, `SkillSlug`)
- `SkillInjectionList` — list response wrapper `{ "entries": [...] }`
- `HubSkillInjectionSetting` — hub_settings["injected_skills"] JSON shape: `{ "system": [...], "user_defined": [...] }`

### `pkg/hub/handlers_skills_injection.go` — Handlers (new file)
All project, user, and hub scope handlers:

**Project endpoints:**
- `listProjectInjectedSkills` — GET, read-access authz
- `addProjectInjectedSkill` — POST, update-access authz
- `setProjectInjectedSkills` — PUT bulk replace, update-access authz
- `removeProjectInjectedSkill` — DELETE, update-access authz

**User /me endpoints:**
- `listUserInjectedSkills` — GET, authenticated user only
- `addUserInjectedSkill` — POST, authenticated user only
- `setUserInjectedSkills` — PUT bulk replace, authenticated user only
- `removeUserInjectedSkill` — DELETE, authenticated user only

**Hub endpoints:**
- `getHubInjectedSkills` — GET, any authenticated user; reads hub_settings["injected_skills"]
- `setHubInjectedSkills` — PUT, hub admin only; updates only user_defined, preserves system

**Helpers:**
- `enrichSkillInjections` — best-effort enrichment from skill bank using slug lookup
- `skillInjectionToEntry`, `skillInjectionEntriesToRefs` — conversions
- `skillBaseURI`, `skillSlugFromURI` — URI parsing for enrichment

### `pkg/hub/handlers_projects_core.go` — Route dispatch
Added `injected-skills` path handling in `handleProjectRoutes`.

### `pkg/hub/server.go` — Route registration
Registered:
- `/api/v1/users/me/injected-skills` and `/api/v1/users/me/injected-skills/`
- `/api/v1/hub/settings/injected-skills`

### `pkg/hub/handlers_skills_injection_test.go` — Tests (new file)
31 tests covering all handlers:
- List, add, set, delete for project scope
- List, add, set, delete for user scope
- Hub GET (empty default, stored setting, any-auth-user-read, unauthenticated)
- Hub PUT (admin success, preserves system entries, non-admin forbidden, unauthenticated)
- Authorization: 401 unauthenticated, 403 wrong-project, 404 missing project

## Decisions and Deviations

1. **ValidationError code**: `ValidationError()` returns 400 (not 422). Tests reflect this.
2. **Enrichment**: Best-effort skill bank lookup by slug. Tries global scope then core scope. Missing entries leave SkillName/SkillSlug empty — no failure path.
3. **Pre-existing failures**: `TestAddProvider`, `TestUpdateAgent_AllowsGeminiMaxModelCalls`, `TestSystemInit_EmbedOnlyHarness` were already failing on the phase1 base branch (unrelated to injected skills).
4. **Hub PUT acceptance**: Uses `expectedRevision=-1` (last-writer-wins) for the UpsertHubSetting call, consistent with the design spec and other admin-managed settings.

## Test Results

- `go build ./...`: clean ✓
- `go test -tags sqlite ./pkg/hub/ -run "InjectedSkill|InjectedSkills|HubInjected"`: 31/31 PASS ✓
- `go test -tags sqlite ./pkg/store/...`: all pass ✓
- Pre-existing failures (3 tests unrelated to this feature) remain unchanged ✓
