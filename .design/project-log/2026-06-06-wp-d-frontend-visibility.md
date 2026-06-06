# WP-D: Frontend Visibility UI Changes

**Date:** 2026-06-06
**Branch:** design/project-visibility-membership
**Commit:** fa2fb68

## Summary

Implemented WP-D from the project-visibility implementation plan — the frontend
portion of the membership-based visibility model.

## Changes

### 1. `web/src/components/pages/project-create.ts`
- Removed the `visibility` `@state` property (was `'private'` default)
- Removed `visibility` from the POST request body
- Removed the entire visibility `<sl-select>` markup (Private/Team/Public options)
- New projects now default to creator-only; visibility is emergent from membership

### 2. `web/src/components/shared/group-member-editor.ts`
- Added `showProjectMembersHint` boolean property (opt-in, default false)
- Added CSS for `.project-members-hint` styled hint box
- In compact mode (used by project settings), renders an info hint:
  "To make this project visible to all hub users, add the **hub-members** group."
- Hint is scoped to project-members context only — does not appear on the admin
  group detail page or any other usage of the editor.

### 3. `web/src/components/pages/project-settings.ts`
- Set `showProjectMembersHint` on the `<scion-group-member-editor>` instance
- Updated `sectionDescription` from "create and manage agents" to
  "access this project and its agents" to reflect the new access model

## Verification

- `npm run typecheck` — passes cleanly (zero errors)
- `npm run lint` — all errors are pre-existing (confirmed by stash-checking
  the same files before changes); no new lint issues introduced
- No Go files were touched

## Observations

- The lint configuration has widespread pre-existing prettier/formatting issues
  across the web codebase. These are not related to this change.
