# Phase 5 — Web UI: Injected Skills Panel

**Agent:** ps-p5-dev  
**Branch:** scion/project-skills-phase2  
**Commit:** 97addc4  
**Date:** 2026-07-22

## Summary

Implemented the Web UI for the Injected Skills feature (GitHub issue #542, Phase 5).

## Files Created

### `web/src/components/shared/injected-skills-panel.ts`
Single shared `<scion-injected-skills-panel>` LitElement component parameterized by:
- `scope: 'project' | 'user' | 'hub'` — determines which API endpoint to call
- `scopeId: string` — project or user UUID (empty for hub scope)
- `readonly: boolean` — makes entire panel read-only (not currently used externally, but supported)

**Component features:**
- Table of injected skill entries: URI/name, alias (as), optional flag
- Enriched skill display: shows `skillName` when the URI resolves to a skill bank entry
- Add dialog with two modes:
  - **Skill Bank** — searches `/api/v1/skills` with debounce, pick-to-select
  - **External URI** — free-form input for GCS/GitHub/external URIs
- Alias (as) field + optional checkbox in dialog
- Per-row delete button (with confirmation prompt)
- Drag-handle reorder via HTML5 drag & drop → PUT bulk-set
- Hub-scope: system entries shown with lock icon + "System" badge, non-deletable/non-draggable; user_defined entries fully editable

**API mapping:**
- project scope → `GET/POST/PUT/DELETE /api/v1/projects/{scopeId}/injected-skills[/{id}]`
- user scope → `GET/POST/PUT/DELETE /api/v1/users/me/injected-skills[/{id}]`
- hub scope → `GET/PUT /api/v1/hub/settings/injected-skills` (PUT sends only `user_defined` list)

### `web/src/components/pages/profile-skills.ts`
Thin wrapper page for the user scope, analogous to `profile-env-vars.ts`. Renders `<scion-injected-skills-panel scope="user">`.

## Files Modified

### `web/src/components/pages/project-settings.ts`
- Imported `injected-skills-panel.js`
- Added "Skills" tab to the Resources `sl-tab-group`
- Tab panel renders `<scion-injected-skills-panel scope="project" scopeId=${this.projectId}>`

### `web/src/components/pages/settings.ts` (Hub Resources)
- Imported `injected-skills-panel.js`
- Added "Skills" tab to hub Resources `sl-tab-group`
- Tab panel renders `<scion-injected-skills-panel scope="hub">` with explanatory description about system vs. user_defined entries

### `web/src/client/main.ts`
- Added route: `{ pattern: /^\/profile\/skills$/, tag: 'scion-page-profile-skills', ... }`
- Added `scion-page-profile-skills` to `PROFILE_ROUTES` set so it renders in profile shell

### `web/src/components/profile/profile-shell.ts`
- Added `'/profile/skills': 'Skills'` to `PROFILE_TITLES`

### `web/src/components/profile/profile-nav.ts`
- Added `{ path: '/profile/skills', label: 'Skills', icon: 'puzzle' }` under Configuration section

## Design Decisions

1. **Single component, three API shapes**: The hub API uses `SkillReference` (`uri`/`as`/`optional`) while project/user use `SkillInjectionEntry` (`skillUri`/`skillAs`/`id`/`sortOrder`/`skillName`/`skillSlug`). The component normalizes both into a single internal `SkillRow` type and handles serialization back to wire format per scope.

2. **Hub system entries**: Hub GET returns `{system: [], user_defined: []}`. System entries are marked `readonly: true` in the normalized row — they render with a lock icon + "System" badge and are excluded from delete/drag actions. The PUT only sends `user_defined`.

3. **Skill bank URI format**: When a skill is selected from the search picker, the component uses `skill://<slug>` as the URI. The back-end enriches this at read time to populate `skillName`/`skillSlug`.

4. **Drag reorder**: Uses HTML5 DragEvent. On drop, the component optimistically reorders locally and fires a PUT bulk-set. On failure it calls `load()` to revert. Hub scope drag only applies to user_defined rows; system rows are excluded from drag.

5. **exactOptionalPropertyTypes**: The project uses strict TypeScript. Used a `buildSkillRef()` helper to conditionally assign optional fields rather than spreading `{as: undefined}`, which TS rejects.

## Acceptance Criteria Status

- ✅ Project admin can add a skill via Skills tab in Project Settings → Resources
- ✅ Skill bank skills display name/slug; external URIs display raw URI
- ✅ User can add/remove from their own Skills tab in User Settings (/profile/skills)
- ✅ Hub admin sees system entries (read-only) and user_defined entries (editable) in Hub Settings → Skills
- ✅ Same component code used in all three locations — no duplicate implementations
- ✅ `go build ./...` clean
- ✅ Web build passes (`npm run build`)
- ✅ TypeScript type check clean (`tsc --noEmit`)

## Build Results

```
go build ./...   → clean (no output)
gofmt -l .       → clean (no output; no Go files modified)
npm run build    → ✓ built in 6.76s
tsc --noEmit     → clean (no output)
```
