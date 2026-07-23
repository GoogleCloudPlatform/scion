# Phase 6 — Seed Platform Skills into Hub Injected Skills

**Agent:** ps-p6-dev  
**Branch:** scion/ps-p6-dev → target: scion/project-skills-phase2  
**Commit:** 0a0fe0a7  
**Date:** 2026-07-22

## Summary

Implemented Phase 6 of the Injected Skills feature: platform (binary-embedded) skills are now
seeded into `hub_settings["injected_skills"].system` on every hub startup, surfacing them in the
Hub Settings → Skills tab as read-only "System" entries.

## Files Created

### `pkg/hub/platform_skills_seed.go`

New file containing `seedPlatformSkillInsertions()` — a method on `*Server` that:

1. Iterates `resources.PlatformSkillsFS()` to discover all platform skill directories.
2. Skips directories that lack a `SKILL.md` (mirrors `injectPlatformSkills()` behaviour).
3. Creates one `api.SkillReference{URI: "scion-platform://<name>", Optional: true}` per skill.
4. Reads the existing `hub_settings["injected_skills"]` value to preserve `user_defined` entries.
5. Marshals a new `api.HubSkillInjectionSetting{System: refs, UserDefined: preserved}` and calls
   `s.store.UpsertHubSetting(ctx, "injected_skills", value, "seeded", -1, "seeded")`.

URI scheme chosen: `scion-platform://` — clearly identifies built-in skills, distinct from
`skill://` (skill bank URIs). With `Optional: true`, any attempt to resolve these via the
skill resolvers is silently skipped (unsupported scheme → optional skip). The actual skill
files are still injected by `injectPlatformSkills()` at provision time (Step 3a2, unchanged).

### `pkg/hub/platform_skills_seed_test.go`

Three test cases using the existing `testServer(t)` helper (real SQLite in-memory store):

- **`TestSeedPlatformSkillInsertions_SetsSystemEntries`** — verifies ≥1 system entry is written,
  all with `Optional=true` and a URI containing the `scion-platform://` prefix.
- **`TestSeedPlatformSkillInsertions_Idempotent`** — calls seed twice, verifies no error and
  stable system entry count.
- **`TestSeedPlatformSkillInsertions_PreservesUserDefined`** — pre-seeds a `user_defined` list,
  calls seed, verifies system is populated and user_defined is unchanged.

## Files Modified

### `pkg/hub/server.go`

Added call to `srv.seedPlatformSkillInsertions(ctx)` in `New()`, after the existing
`seedDefaultPoliciesAndGroups` and `seedDevUser` calls. Failure is non-fatal: logged at WARN
so a broken embedded FS doesn't prevent hub startup.

### `pkg/hub/handlers_skills_injection_test.go`

Renamed `TestGetHubInjectedSkills_EmptyByDefault` → `TestGetHubInjectedSkills_DefaultState` and
updated assertions: `system` is now expected non-empty (seeded by startup), `user_defined`
remains empty. The old "empty by default" invariant is no longer true after Phase 6.

## Key Decisions

### URI Format: `scion-platform://<name>`

Several schemes were considered:
- `skill://scion/core/<name>` — canonical but implies skill bank registration (not true yet)
- `builtin://scion/<version>/skill/<name>` — matches other bundled resource pattern but longer
- `scion-platform://<name>` — **chosen**: unambiguous, compact, clearly identifiable

With `Optional: true`, an unrecognized scheme simply results in "unsupported_scheme" error that
is silently dropped at provision time. No provisioning breakage.

### Startup Wiring: Non-Fatal

`seedPlatformSkillInsertions` failure logs WARN and continues. The admin UI would show an empty
system list rather than crashing the hub. This matches the approach used for other seed operations.

### Hub Settings UI: Already Integrated

Phase 5 already added `<scion-injected-skills-panel scope="hub">` to `web/src/components/pages/settings.ts`.
No UI changes were needed for Phase 6.

### Test Strategy

Used `testServer(t)` (full server + real SQLite) rather than a stub/fake, so tests exercise the
actual startup flow including the seeded state. This also confirmed that `testServer` now
auto-seeds platform skills on creation (visible in the log output).

## Issues Found and Resolved

**`TestGetHubInjectedSkills_EmptyByDefault` broke** because the test used `testServer(t)` which
now runs `seedPlatformSkillInsertions()` during initialization, populating the system list. The
test expected an empty system list. Fixed by updating the test to expect the seeded state.

The three other pre-existing test failures (`TestAddProvider`, `TestUpdateAgent_AllowsGeminiMaxModelCalls`,
`TestSystemInit_EmbedOnlyHarness`) were already failing on the base branch (`scion/project-skills-phase2`)
and are unrelated to Phase 6.

## Build Results

```
go build ./...              → clean
go vet ./...                → clean
go test ./pkg/hub/ -run TestSeedPlatformSkill  → PASS (3/3)
go test ./pkg/hub/ -run TestGetHubInjectedSkills → PASS (4/4)
```
